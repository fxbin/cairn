// loop_async_delegate.go 实现异步委派在 Agent Loop 内的停靠、终态吸收与
// 下轮交付：父 Loop 在模型无更多工具调用但存在非终态异步子时不终态化、
// 不发模型请求，停靠等待子完成信号或 steering；子完成事实由应用层回写父
// Journal，本 Loop 仅在安全点经 resync 吸收并按 steering 事件合同持久化
// 一次性交付标记。停靠期间不追加自身事件，外部事实只经 resyncProjection
// 进入投影，保持串行单写入者合同。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// steeringKindSteer 是用户补充指导的既有 steering 种类。
const steeringKindSteer = "steer"

// steeringKindSubagentResult 标记一次异步子终态摘要的下轮交付，复用
// steering_input_submitted/consumed 事件合同；消息重建按该种类选择注入前缀。
const steeringKindSubagentResult = "subagent_result"

// asyncChildResultInputIDPrefix 是子结果交付标记的幂等标识前缀。
const asyncChildResultInputIDPrefix = "subagent-result-"

// asyncChildResultMessagePrefix 是注入下一次模型请求的子结果摘要前缀。
const asyncChildResultMessagePrefix = "[异步子任务完成报告]\n"

// asyncChildSummaryMaxBytes 限制单次交付给父上下文的子终态摘要长度。
const asyncChildSummaryMaxBytes = 2048

// asyncParkSteeringPollInterval 是停靠期间探测 steering 到达的周期；停靠
// 只探测收件箱长度、不取走值，消费统一由安全点完成，避免停靠吞掉投递。
const asyncParkSteeringPollInterval = 20 * time.Millisecond

// asyncChildWriterBlockedCode 是停靠期间拒绝父侧副作用工具的专用失败码。
const asyncChildWriterBlockedCode = "async_child_writer_active"

// asyncChildResultInputID 构造子结果交付标记的幂等标识。
func asyncChildResultInputID(childRunID string) string {
	return asyncChildResultInputIDPrefix + childRunID
}

// AsyncChildSet 登记本进程内活跃异步子运行的完成信号；Launch 登记新子，
// Settle 关闭并移除已完成子，Done 供父 Loop 停靠等待。父 Loop（读）与子
// goroutine（收束写）并发访问，以互斥锁保护。登记表只承载进程内运行时
// 事实，不写 Journal；跨进程重建依赖父投影与显式恢复门禁。
type AsyncChildSet struct {
	mu      sync.Mutex
	signals map[string]chan struct{}
}

// NewAsyncChildSet 创建空的异步子完成信号登记表。
func NewAsyncChildSet() *AsyncChildSet {
	return &AsyncChildSet{signals: make(map[string]chan struct{})}
}

// Launch 登记一个新异步子并返回其完成信号通道。
func (set *AsyncChildSet) Launch(childRunID string) chan struct{} {
	set.mu.Lock()
	defer set.mu.Unlock()
	signal := make(chan struct{})
	set.signals[childRunID] = signal
	return signal
}

// Settle 关闭并移除子的完成信号；重复收束幂等。
func (set *AsyncChildSet) Settle(childRunID string) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if signal, exists := set.signals[childRunID]; exists {
		delete(set.signals, childRunID)
		close(signal)
	}
}

// Done 返回子的完成信号；子不在登记表（已收束或从未在本进程启动）时缺席。
func (set *AsyncChildSet) Done(childRunID string) (<-chan struct{}, bool) {
	set.mu.Lock()
	defer set.mu.Unlock()
	signal, exists := set.signals[childRunID]
	return signal, exists
}

// pendingAsyncChildIDs 返回投影中尚未完成（delegated）的子身份，按字典序
// 排序保证确定性。
func pendingAsyncChildIDs(projection *domain.RunProjection) []string {
	pending := make([]string, 0, len(projection.Children))
	for childID, record := range projection.Children {
		if record.State == "delegated" {
			pending = append(pending, childID)
		}
	}
	sort.Strings(pending)
	return pending
}

// asyncChildDelivery 是一次待交付的子终态摘要。
type asyncChildDelivery struct {
	childRunID string
	outcome    string
}

// undeliveredCompletedChildren 返回已完成且尚未持久化交付标记的子身份，
// 交付去重以 steering 消费幂等标识为准，进程内与 resume 回放共用同一口径。
func (loop *Loop) undeliveredCompletedChildren(projection *domain.RunProjection) []asyncChildDelivery {
	consumed := steeringConsumedIDs(loop.steeringConsumed)
	deliveries := make([]asyncChildDelivery, 0, len(projection.Children))
	for childID, record := range projection.Children {
		if record.State != "completed" {
			continue
		}
		if _, done := consumed[asyncChildResultInputID(childID)]; done {
			continue
		}
		deliveries = append(deliveries, asyncChildDelivery{childRunID: childID, outcome: record.Outcome})
	}
	sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].childRunID < deliveries[j].childRunID })
	return deliveries
}

// asyncChildWriterActive 再同步投影并报告是否存在非终态子；停靠期间父侧
// 副作用类工具据此失败关闭，只读工具与 steering 不受限。
func (loop *Loop) asyncChildWriterActive(ctx context.Context, projection *domain.RunProjection) (bool, error) {
	if err := loop.resyncProjection(ctx, projection); err != nil {
		return false, err
	}
	return len(pendingAsyncChildIDs(projection)) > 0, nil
}

// refuseParentSideEffectWhileChildActive 在异步子活跃期间拒绝父侧副作用类
// 工具调用：按策略拒绝路径的审计形状落盘（提议 → 决策 → 操作失败 → 模型
// 可见结果），操作从未启动，模型可在剩余轮次内改用只读方案或等待子收束。
func (loop *Loop) refuseParentSideEffectWhileChildActive(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, operationID domain.OperationID, call ports.ToolCall) (tools.Result, error) {
	result := tools.Failure(asyncChildWriterBlockedCode, "an async child run is still active; parent side-effect tools stay closed until it settles (read-only tools and steering remain available)")
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
		return tools.Result{}, err
	}
	if err := loop.appendToolResult(ctx, projection, input, turnID, operationID, result); err != nil {
		return tools.Result{}, err
	}
	if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
		return result, err
	}
	return result, nil
}

// deliverCompletedAsyncChildren 在安全点吸收外部完成事实并把尚未交付的子
// 终态摘要作为有界用户侧消息注入下一次模型请求；交付以 steering 事件的
// submitted/consumed 对持久化，幂等标识进入消费去重表，resume 回放不重复注入。
func (loop *Loop) deliverCompletedAsyncChildren(ctx context.Context, projection *domain.RunProjection, input RunInput, messages []ports.Message) ([]ports.Message, error) {
	if err := loop.resyncProjection(ctx, projection); err != nil {
		return messages, err
	}
	for _, delivery := range loop.undeliveredCompletedChildren(projection) {
		summary, err := loop.asyncChildSummary(ctx, delivery.childRunID, delivery.outcome)
		if err != nil {
			return messages, err
		}
		contentRef, err := loop.storeAsyncChildSummary(summary)
		if err != nil {
			return messages, err
		}
		inputID := asyncChildResultInputID(delivery.childRunID)
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventSteeringInputSubmitted, Payload: mustJSON(steeringInputPayload{
			InputID: inputID, ContentRef: contentRef, Kind: steeringKindSubagentResult,
		})}); err != nil {
			return messages, err
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventSteeringInputConsumed, Payload: mustJSON(steeringConsumedPayload{
			InputID: inputID, ContentRef: contentRef, Outcome: "applied", Kind: steeringKindSubagentResult,
		})}); err != nil {
			return messages, err
		}
		messages = append(messages, ports.Message{Role: "user", Content: asyncChildResultMessagePrefix + summary})
		if loop.steeringConsumed == nil {
			loop.steeringConsumed = map[string]struct{}{}
		}
		loop.steeringConsumed[inputID] = struct{}{}
	}
	return messages, nil
}

// storeAsyncChildSummary 把交付摘要脱敏落制品并返回引用；制品库不可用时
// 失败关闭，不得把摘要内联进事件。
func (loop *Loop) storeAsyncChildSummary(summary string) (*artifact.Ref, error) {
	if loop.Artifacts == nil {
		return nil, errors.New("artifact store is required to deliver async child results")
	}
	ref, err := loop.Artifacts.Put([]byte(summary), "text/plain")
	if err != nil {
		return nil, fmt.Errorf("store async child summary: %w", err)
	}
	return &ref, nil
}

// asyncChildSummary 只读回放子运行 Journal 提取有界终态摘要：completed 取
// 最终回复，failed 取失败说明，cancelled 只报告终态；子历史缺失或损坏失败
// 关闭，不猜测子结果。
func (loop *Loop) asyncChildSummary(ctx context.Context, childRunID string, outcome string) (string, error) {
	events, err := loop.Journal.Load(ctx, domain.RunID(childRunID))
	if err != nil {
		return "", fmt.Errorf("load async child %s for delivery: %w", childRunID, err)
	}
	detail := ""
	for index := len(events) - 1; index >= 0; index-- {
		switch events[index].Type {
		case domain.EventRunCompleted:
			var payload struct {
				FinalResponse string `json:"final_response"`
			}
			if json.Unmarshal(events[index].Payload, &payload) == nil {
				detail = payload.FinalResponse
			}
		case domain.EventRunFailed:
			var payload struct {
				Error      string `json:"error"`
				ErrorClass string `json:"error_class"`
			}
			if json.Unmarshal(events[index].Payload, &payload) == nil && payload.Error != "" {
				detail = payload.Error
				if payload.ErrorClass != "" {
					detail = payload.Error + " (" + payload.ErrorClass + ")"
				}
			}
		default:
			continue
		}
		if detail != "" {
			break
		}
	}
	summary := "child_run_id: " + childRunID + "\noutcome: " + outcome
	if strings.TrimSpace(detail) != "" {
		summary += "\nsummary: " + truncateUTF8(detail, asyncChildSummaryMaxBytes)
	}
	return summary, nil
}

// truncateUTF8 按字节上限截断并保持 UTF-8 边界完整。
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// parkForPendingAsyncChildren 在父模型无更多工具调用但异步子事实未收束时
// 停靠：先把模型说明作为助手消息落盘并以 continue 关闭当前轮次（run 不
// 终态化、不发模型请求），再等待全部非终态子完成或 steering 到达；仅有
// 未交付完成子时不停靠等待，直接回到安全点交付。存在无活跃信号的非终态子
// （进程重启遗留或同步审批暂停子）时失败关闭，引导显式恢复。返回 true 表示
// 需要回到安全点继续；false 表示无异步子事实，按既有流程收尾。
func (loop *Loop) parkForPendingAsyncChildren(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, assistantText string) (bool, error) {
	if err := loop.resyncProjection(ctx, projection); err != nil {
		return false, err
	}
	pending := pendingAsyncChildIDs(projection)
	if len(pending) == 0 && len(loop.undeliveredCompletedChildren(projection)) == 0 {
		return false, nil
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventAssistantMessageAppended, TurnID: &turnID, Payload: mustJSON(domain.AssistantMessagePayload{Text: assistantText})}); err != nil {
		return false, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
		return false, err
	}
	if len(pending) == 0 {
		return true, nil
	}
	steeringTicker := time.NewTicker(asyncParkSteeringPollInterval)
	defer steeringTicker.Stop()
	for {
		current := pendingAsyncChildIDs(projection)
		if len(current) == 0 {
			return true, nil
		}
		var waitOn <-chan struct{}
		for _, childID := range current {
			signal, live := loop.AsyncChildren.Done(childID)
			if !live {
				return false, fmt.Errorf("async child run %s has no live runner; finish it explicitly (resume the child run, or resume %s --abort-run) before the parent can continue", childID, childID)
			}
			if waitOn == nil {
				waitOn = signal
			}
		}
		// steering 只做到达探测，值仍留在收件箱由安全点统一消费与持久化。
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-steeringTicker.C:
			if len(loop.steeringInbox) > 0 {
				return true, nil
			}
		case <-waitOn:
			if err := loop.resyncProjection(ctx, projection); err != nil {
				return false, err
			}
		}
	}
}

// asyncAdvanceRequired 报告恢复现场是否存在必须先推进的异步子事实：非终态
// 子，或已完成但尚未交付的子。供最终响应恢复入口避免在子事实未吸收前直接
// 终态化运行。
func asyncAdvanceRequired(projection *domain.RunProjection, events []domain.Event) bool {
	delivered := make(map[string]struct{})
	for _, event := range events {
		if event.Type != domain.EventSteeringInputConsumed {
			continue
		}
		var payload struct {
			InputID string `json:"input_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && strings.HasPrefix(payload.InputID, asyncChildResultInputIDPrefix) {
			delivered[payload.InputID] = struct{}{}
		}
	}
	for childID, record := range projection.Children {
		if record.State == "delegated" {
			return true
		}
		if record.State == "completed" {
			if _, done := delivered[asyncChildResultInputID(childID)]; !done {
				return true
			}
		}
	}
	return false
}
