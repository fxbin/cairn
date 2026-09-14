package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// contextBytesPerEstimatedToken 统一请求估算与摘要空间换算的字节口径。
const contextBytesPerEstimatedToken = 2

const contextCompactionStrategy = "deterministic_journal_checkpoint_v4"

// contextCheckpointPrefix 声明检查点的使用纪律：优先基于清单内已有发现推进，
// 不重复探索。措辞本身就是行为合同——v1 的"需要细节时请重新读取工作区"曾被
// 观测为压缩后重读循环的放大器（AgentScope 失败 run：压缩后 search 塌缩、
// read 爆炸、零 patch 耗尽 48 轮）。
const contextCheckpointPrefix = "此前的对话原文已被压缩为以下可审计检查点。系统提示和原始任务保持不变；检查点首先列出已检查对象清单，请基于已有发现继续推进，仅当清单之外的信息确有必要时才读取或搜索。\n"

// CompactionProbeEvidence 是离线预筛可消费的闭合探针证据：相同输入的两次
// 摘要、工具调用与回执计数均来自内置固定消息，不含任务正文或本机路径。
type CompactionProbeEvidence struct {
	FirstSummary  []byte
	SecondSummary []byte
	ToolCalls     int
	ToolResults   int
}

// ProbeContextCompaction 对固定的成对工具历史执行两次压缩，供正式评测启动
// 前验证确定性、工具配对与证据锚保留；它不读取工作区也不产生副作用。
func ProbeContextCompaction(maximumBytes, inventoryBudgetPercent int) CompactionProbeEvidence {
	messages := []ports.Message{
		{Role: "system", Content: "固定离线探针"},
		{Role: "user", Content: "核对压缩器的确定性与证据锚。"},
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "probe-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"probe.go"}`)}}},
		{Role: "tool", ToolCallID: "probe-read", Content: "已读取目标并保留事实证据。"},
		{Role: "user", Content: "验证反馈：工具调用与回执已经配对。"},
	}
	first := summarizeMessagesWithInventoryBudget(messages, maximumBytes, inventoryBudgetPercent)
	second := summarizeMessagesWithInventoryBudget(messages, maximumBytes, inventoryBudgetPercent)
	return CompactionProbeEvidence{
		FirstSummary: []byte(first), SecondSummary: []byte(second),
		ToolCalls: 1, ToolResults: 1,
	}
}

// compactContextIfNeeded 在安全恢复边界把过长对话替换为持久化检查点。
// 证据字段（ADR-0007）在压缩时刻从事件流一次性派生：被替代的 turn 清单、
// 最近一次实际 Provider 输入计数与上一检查点切点都取自日志，不做 Loop 内存
// 状态，因此 resume 后立即压缩也能产出正确证据。
func (loop *Loop) compactContextIfNeeded(ctx context.Context, projection *domain.RunProjection, input RunInput, messages []ports.Message) ([]ports.Message, error) {
	if !loop.ContextAutoCompact || loop.ContextMaxPromptTokens <= 0 {
		return messages, nil
	}
	before := loop.estimatePromptTokens(messages)
	if before <= loop.contextCompactionThreshold() {
		return messages, nil
	}
	if projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil {
		return nil, errors.New("context compaction requires a closed turn without pending work")
	}
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return nil, fmt.Errorf("load events for context compaction: %w", err)
	}
	// 检查点只能引用当前投影已观察到的事实；旧摘要不是新的摘要输入。
	boundedEvents := make([]domain.Event, 0, len(events))
	rawEvents := make([]domain.Event, 0, len(events))
	for _, event := range events {
		if event.Seq > projection.LastSeq {
			continue
		}
		boundedEvents = append(boundedEvents, event)
		if event.Type != domain.EventContextCompacted {
			rawEvents = append(rawEvents, event)
		}
	}
	history, err := loop.rebuildMessagesFromEvents(rawEvents, input.Task, true)
	if err != nil {
		return nil, fmt.Errorf("rebuild context compaction history: %w", err)
	}
	priorContext := loop.PriorContext
	historyStart := 2
	if priorContext != "" {
		historyStart++
	}
	task := history[historyStart-1].Content
	// 固定提示超过预留阈值但仍在硬上限内时，没有历史可供压缩，不能添加
	// 空检查点反而放大请求；真实超限仍失败关闭。
	if len(history) == historyStart {
		if loop.estimatePromptTokens(history) > loop.ContextMaxPromptTokens {
			return nil, fmt.Errorf("context limit %d cannot fit the persistent prompt and tool definitions", loop.ContextMaxPromptTokens)
		}
		return history, nil
	}
	maximumSummaryBytes := loop.compactionSummaryLimit(task, priorContext)
	if maximumSummaryBytes < 1 {
		return nil, fmt.Errorf("context limit %d cannot fit the persistent prompt and tool definitions", loop.ContextMaxPromptTokens)
	}
	summary := summarizeMessagesWithInventoryBudgetFrom(history, maximumSummaryBytes, loop.contextInventoryBudgetPercent(), historyStart)
	compacted := loop.contextMessages(task, priorContext, summary)
	after := loop.estimatePromptTokens(compacted)
	if after >= before && before <= loop.ContextMaxPromptTokens {
		return messages, nil
	}
	if after > loop.ContextMaxPromptTokens {
		return nil, fmt.Errorf("context checkpoint requires %d tokens, exceeding configured limit %d", after, loop.ContextMaxPromptTokens)
	}
	evidence := compactionEvidenceFromEvents(boundedEvents)
	payload := domain.ContextCompactedPayload{
		ThroughSeq:               projection.LastSeq,
		Summary:                  summary,
		EstimatedTokensBefore:    before,
		EstimatedTokensAfter:     after,
		Strategy:                 contextCompactionStrategy,
		SummaryVersion:           domain.ContextSummaryVersion,
		Trigger:                  domain.ContextCompactionTriggerPromptEstimate,
		UnitsSource:              domain.ContextUnitsSourceEstimateBytesDiv2,
		LastProviderPromptTokens: evidence.lastProviderPromptTokens,
		CompactedTurnIDs:         evidence.compactedTurnIDs,
		RetainedTurnIDs:          []string{},
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventContextCompacted, Payload: mustJSON(payload)}); err != nil {
		return nil, err
	}
	return compacted, nil
}

// compactionEvidence 保存压缩时刻从事件流派生的证据：被替代的 turn 清单与
// 最近一次实际 Provider 输入计数。
type compactionEvidence struct {
	compactedTurnIDs         []string
	lastProviderPromptTokens int64
}

// compactionEvidenceFromEvents 从截至当前投影的完整事件派生增量压缩证据；
// 保留旧检查点切点以避免重复计算被替代轮次，最近真实输入计数缺失时保持零值。
func compactionEvidenceFromEvents(events []domain.Event) compactionEvidence {
	var evidence compactionEvidence
	_, previousThrough := latestContextCheckpoint(events)
	evidence.compactedTurnIDs = make([]string, 0, 4)
	for _, event := range events {
		if event.Type != domain.EventTurnOpened || event.TurnID == nil {
			continue
		}
		if event.Seq <= previousThrough {
			continue
		}
		evidence.compactedTurnIDs = append(evidence.compactedTurnIDs, string(*event.TurnID))
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventModelResponseFinished {
			continue
		}
		var payload struct {
			Usage *struct {
				PromptTokens int64 `json:"prompt_tokens"`
			} `json:"usage"`
			UsageEstimated bool `json:"usage_estimated"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Usage != nil && !payload.UsageEstimated {
			// 取最近一次真实回报（含 0），不被更早回报覆盖；估算兜底跳过。
			evidence.lastProviderPromptTokens = payload.Usage.PromptTokens
			break
		}
	}
	return evidence
}

// contextCompactionThreshold 按配置比例在模型名义上下文上限前保留余量，吸收
// 工具定义、JSON 编码与 tokenizer 差异，避免临界请求消耗整轮输入预算。
func (loop *Loop) contextCompactionThreshold() int {
	reserve := loop.ContextMaxPromptTokens * loop.contextThresholdReservePercent() / 100
	threshold := loop.ContextMaxPromptTokens - reserve
	if threshold < 1 {
		return 1
	}
	return threshold
}

// contextThresholdReservePercent 返回闭合后的阈值余量比例；零值兼容直接构造
// Loop 的内部测试与嵌入调用，等价于产品默认值 20%。
func (loop *Loop) contextThresholdReservePercent() int {
	if loop.ContextThresholdReservePercent <= 0 {
		return 20
	}
	return loop.ContextThresholdReservePercent
}

// contextInventoryBudgetPercent 返回闭合后的探索清单预算比例；零值兼容直接
// 构造 Loop 的内部调用，等价于产品默认值 25%。
func (loop *Loop) contextInventoryBudgetPercent() int {
	if loop.ContextInventoryBudgetPercent <= 0 {
		return 25
	}
	return loop.ContextInventoryBudgetPercent
}

// contextMessages 按系统提示、会话前情、原始任务、检查点的固定顺序构建持久上下文。
func (loop *Loop) contextMessages(task, priorContext, summary string) []ports.Message {
	messages := []ports.Message{{Role: "system", Content: loop.prompt()}}
	if priorContext != "" {
		messages = append(messages, ports.Message{Role: "user", Content: sessionContextBlock(priorContext)})
	}
	messages = append(messages, ports.Message{Role: "user", Content: task})
	if summary != "" {
		messages = append(messages, ports.Message{Role: "user", Content: contextCheckpointPrefix + summary})
	}
	return messages
}

// compactionSummaryLimit 在静态工具定义和固定消息之后，为检查点预留字节空间。
func (loop *Loop) compactionSummaryLimit(task, priorContext string) int {
	// 非空占位符让估算包含检查点角色和前缀；额外保留该字节作为舍入余量。
	persistent := loop.contextMessages(task, priorContext, " ")
	remaining := (loop.ContextMaxPromptTokens - loop.estimatePromptTokens(persistent)) * contextBytesPerEstimatedToken
	if remaining < 1 {
		return 0
	}
	if loop.ContextMaxSummaryBytes > 0 && remaining > loop.ContextMaxSummaryBytes {
		remaining = loop.ContextMaxSummaryBytes
	}
	return remaining
}

// estimatePromptTokens 用每两个 UTF-8 字节一个 token 的悲观近似估算下一次请求输入，
// 为源码、JSON 和中英文混合内容保留压缩余量，避免低估导致累计输入预算先被耗尽。
func (loop *Loop) estimatePromptTokens(messages []ports.Message) int {
	bytes := 0
	for _, message := range messages {
		bytes += len(message.Role) + len(message.Content) + len(message.ToolCallID)
		for _, call := range message.ToolCalls {
			bytes += len(call.ID) + len(call.Name) + len(call.Arguments)
		}
	}
	for _, spec := range loop.Tools.Specs() {
		bytes += len(spec.Name) + len(spec.Description) + len(spec.Parameters)
	}
	return (bytes + contextBytesPerEstimatedToken - 1) / contextBytesPerEstimatedToken
}

// summarizeMessages 优先保留探索清单、最近的工具结果、模型决策和验证反馈，
// 不复制原始数据体。探索清单获得独立预算切片（至多四分之一）：它是从工具
// 调用确定性派生的去重"已检查对象"视图，压缩后防止模型重读循环。
func summarizeMessages(messages []ports.Message, maximumBytes int) string {
	return summarizeMessagesWithInventoryBudget(messages, maximumBytes, 25)
}

// summarizeMessagesWithInventoryBudget 按显式比例切分探索清单与近期事实预算；
// 比例由冻结配置进入运行时，越界值在配置装配前失败关闭。
func summarizeMessagesWithInventoryBudget(messages []ports.Message, maximumBytes, inventoryBudgetPercent int) string {
	return summarizeMessagesWithInventoryBudgetFrom(messages, maximumBytes, inventoryBudgetPercent, 2)
}

// summarizeMessagesWithInventoryBudgetFrom 从指定历史起点生成摘要，确保系统提示、
// 会话前情与原始任务等固定消息不进入检查点；默认入口仍保持跳过前两条消息。
func summarizeMessagesWithInventoryBudgetFrom(messages []ports.Message, maximumBytes, inventoryBudgetPercent, historyStart int) string {
	if maximumBytes < 1 {
		return ""
	}
	if historyStart < 0 {
		historyStart = 0
	}
	if historyStart > len(messages) {
		historyStart = len(messages)
	}
	// 清单预算按冻结比例分配，但不低于 512 字节（覆盖中文表头与常见路径）；
	// 极小预算下清单优先于近期事实——丢失探索记忆的代价高于丢失最后一条
	// 工具结果的预览。
	inventoryBudget := maximumBytes * inventoryBudgetPercent / 100
	if inventoryBudget < 512 {
		inventoryBudget = min(512, maximumBytes)
	}
	inventory := explorationInventory(messages[historyStart:], inventoryBudget)
	factBudget := maximumBytes
	if inventory != "" {
		factBudget = maximumBytes - len(inventory)
	}
	facts := summarizeRecentFactsFrom(messages, factBudget, historyStart)
	if facts == "" && inventory == "" {
		return clipText("无可保留的历史事实；请以原始任务和当前工作区为准。", maximumBytes)
	}
	return string(artifact.Redact([]byte(inventory + facts)))
}

// summarizeRecentFacts 从尾部反向保留预算内的近期事实；先前检查点占用一半预算。
func summarizeRecentFacts(messages []ports.Message, maximumBytes int) string {
	return summarizeRecentFactsFrom(messages, maximumBytes, 2)
}

// summarizeRecentFactsFrom 从指定历史起点反向保留近期事实。
func summarizeRecentFactsFrom(messages []ports.Message, maximumBytes, historyStart int) string {
	if maximumBytes < 1 {
		return ""
	}
	toolNames := make(map[string]string)
	previousCheckpoint := ""
	for _, message := range messages[historyStart:] {
		for _, call := range message.ToolCalls {
			toolNames[call.ID] = call.Name
		}
		if message.Role == "user" && strings.HasPrefix(message.Content, contextCheckpointPrefix) {
			previousCheckpoint = strings.TrimPrefix(message.Content, contextCheckpointPrefix)
		}
	}
	entries := make([]string, 0, len(messages))
	used := 0
	preservedCheckpoint := ""
	if previousCheckpoint != "" {
		available := maximumBytes/2 - len("- 先前检查点：\n")
		if available > 0 {
			preservedCheckpoint = "- 先前检查点：" + clipText(previousCheckpoint, available) + "\n"
			used = len(preservedCheckpoint)
		}
	}
	for index := len(messages) - 1; index >= historyStart; index-- {
		if messages[index].Role == "user" && strings.HasPrefix(messages[index].Content, contextCheckpointPrefix) {
			continue
		}
		entry := summarizeMessage(messages[index], toolNames)
		if entry == "" {
			continue
		}
		entry = "- " + entry + "\n"
		if used+len(entry) > maximumBytes {
			remaining := maximumBytes - used
			if remaining > len("- …\n") {
				entries = append(entries, "- "+clipText(strings.TrimSpace(entry[2:]), remaining-len("- …\n"))+"…\n")
			}
			break
		}
		entries = append(entries, entry)
		used += len(entry)
	}
	if len(entries) == 0 {
		if preservedCheckpoint != "" {
			return preservedCheckpoint
		}
		return ""
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return preservedCheckpoint + strings.Join(entries, "")
}

// explorationInventory 从全部历史工具调用确定性派生去重的已检查对象清单；
// 只读工具列路径或查询，exec 列 argv 首词，patch 列路径。清单内的事实跨压缩
// 保留，是压缩后"不要重复探索"的机器可核对依据。
func explorationInventory(messages []ports.Message, maximumBytes int) string {
	if maximumBytes < 1 {
		return ""
	}
	header := "已检查对象（勿重复探索）：\n"
	if maximumBytes < len(header) {
		return ""
	}
	seen := make(map[string]bool)
	lines := make([]string, 0, 16)
	used := len(header)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			target := toolExplorationTarget(call.Name, call.Arguments)
			if target == "" {
				continue
			}
			key := call.Name + ":" + target
			if seen[key] {
				continue
			}
			seen[key] = true
			line := "- " + call.Name + " " + target + "\n"
			if used+len(line) > maximumBytes {
				if used+len("- …\n") <= maximumBytes {
					lines = append(lines, "- …\n")
				}
				return header + strings.Join(lines, "")
			}
			lines = append(lines, line)
			used += len(line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return header + strings.Join(lines, "")
}

// toolExplorationTarget 提取一次工具调用的探索目标摘要；无法识别目标时为空。
func toolExplorationTarget(name string, arguments json.RawMessage) string {
	var fields struct {
		Path  string   `json:"path"`
		Query string   `json:"query"`
		Argv  []string `json:"argv"`
	}
	if len(arguments) == 0 || json.Unmarshal(arguments, &fields) != nil {
		return ""
	}
	switch {
	case fields.Path != "":
		return clipText(fields.Path, 160)
	case fields.Query != "":
		return clipText(fields.Query, 160)
	case len(fields.Argv) > 0:
		return clipText(fields.Argv[0], 160)
	default:
		return ""
	}
}

// summarizeMessage 将单条历史消息归约为不含工具原始 data 字段的事实记录。
func summarizeMessage(message ports.Message, toolNames map[string]string) string {
	switch message.Role {
	case "tool":
		name := toolNames[message.ToolCallID]
		if name == "" {
			name = "unknown_tool"
		}
		return fmt.Sprintf("工具 %s（调用 %s）：%s", name, message.ToolCallID, summarizeToolResult(name, message.Content))
	case "assistant":
		parts := make([]string, 0, len(message.ToolCalls)+1)
		if text := strings.TrimSpace(message.Content); text != "" {
			// 模型说明是压缩后最有价值的综合推理（早于任何工具结果的合成结论），
			// 保留额度高于普通事实条目，防止探索-综合链在检查点处断裂。
			parts = append(parts, "模型说明："+clipText(string(artifact.Redact([]byte(text))), 960))
		}
		for _, call := range message.ToolCalls {
			parts = append(parts, fmt.Sprintf("请求工具 %s（调用 %s）", call.Name, call.ID))
		}
		return strings.Join(parts, "；")
	case "user":
		text := strings.TrimSpace(message.Content)
		if text == "" {
			return ""
		}
		return "用户或验证反馈：" + clipText(string(artifact.Redact([]byte(text))), 480)
	default:
		return ""
	}
}

// summarizeToolResult 保留状态、定位、预览和摘要；data 只经工具专用白名单投影，
// 不复制原文，且完整脱敏必须先于任何预览截断。
func summarizeToolResult(name, content string) string {
	var result struct {
		OK        bool            `json:"ok"`
		Code      string          `json:"code"`
		Preview   string          `json:"preview"`
		Truncated bool            `json:"truncated"`
		Bytes     int64           `json:"bytes"`
		SHA256    string          `json:"sha256"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(artifact.RedactJSON([]byte(content)), &result); err != nil {
		return "返回了无法解析的结构化结果：" + clipText(string(artifact.Redact([]byte(content))), 320)
	}
	parts := []string{fmt.Sprintf("ok=%t", result.OK), "code=" + result.Code}
	if result.OK {
		if evidence := checkpointToolEvidence(name, result.Data); evidence != "" {
			parts = append(parts, evidence)
		}
	}
	if result.Preview != "" {
		parts = append(parts, "preview="+clipText(result.Preview, 480))
	}
	if result.Bytes > 0 {
		parts = append(parts, fmt.Sprintf("bytes=%d", result.Bytes))
	}
	if result.SHA256 != "" {
		parts = append(parts, "sha256="+result.SHA256)
	}
	if result.Truncated {
		parts = append(parts, "truncated=true")
	}
	return strings.Join(parts, "，")
}

// clipText 在 UTF-8 字符边界截断文本，避免摘要因单条消息越过既定上限。
func clipText(value string, maximumBytes int) string {
	if maximumBytes < 1 {
		return ""
	}
	if len(value) <= maximumBytes {
		return value
	}
	if maximumBytes <= len("…") {
		return ""
	}
	limit := maximumBytes - len("…")
	clipped := value[:limit]
	for !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + "…"
}

// estimateResponseTokens 用字节/2 启发式估算一次响应的输出 token，供提供方
// 未回报 usage 时的预算兜底；工具调用的标识与参数 JSON 字节数一并计入。
func estimateResponseTokens(response collectedResponse) int {
	total := len(response.Text)
	for _, call := range response.ToolCalls {
		total += len(call.ID) + len(call.Name) + len(call.Arguments)
	}
	return (total + contextBytesPerEstimatedToken - 1) / contextBytesPerEstimatedToken
}
