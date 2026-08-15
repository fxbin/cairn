package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

const (
	consecutiveOperationFailureLimit = 3
	periodicTurnWindow               = 6
	noProgressTurnLimit              = 6

	loopGuardRepeatedOperationFailure = "repeated_operation_failure"
	loopGuardPeriodicTurnCycle        = "periodic_turn_cycle"
	loopGuardNoProgress               = "no_workspace_or_verifier_progress"
)

// LoopGuardExceededError 表示运行因确定性的重复失败、周期或无进展规则而提前终止。
type LoopGuardExceededError struct{ Reason string }

// Error 返回可用于事件和基准分类的稳定熔断原因。
func (err *LoopGuardExceededError) Error() string {
	return fmt.Sprintf("loop guard triggered: %s", err.Reason)
}

// openedTurnCount 从事件日志重建已经消耗的模型轮次，保证暂停或恢复不会重置硬上限。
func openedTurnCount(events []domain.Event) int {
	count := 0
	for _, event := range events {
		if event.Type == domain.EventTurnOpened {
			count++
		}
	}
	return count
}

// repeatedOperationFailureGuard 返回连续相同操作指纹失败达到阈值时的熔断原因。
func repeatedOperationFailureGuard(events []domain.Event) string {
	fingerprints := make(map[domain.OperationID]string)
	lastFingerprint := ""
	consecutive := 0
	for _, event := range events {
		if event.Type == domain.EventPolicyDecided && event.OperationID != nil {
			var payload struct {
				Fingerprint string `json:"fingerprint"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				fingerprints[*event.OperationID] = payload.Fingerprint
			}
			continue
		}
		switch event.Type {
		case domain.EventOperationFailed:
			if event.OperationID == nil || fingerprints[*event.OperationID] == "" {
				lastFingerprint, consecutive = "", 0
				continue
			}
			fingerprint := fingerprints[*event.OperationID]
			if fingerprint == lastFingerprint {
				consecutive++
			} else {
				lastFingerprint, consecutive = fingerprint, 1
			}
		case domain.EventOperationSucceeded, domain.EventOperationCancelled, domain.EventOperationReconciled:
			lastFingerprint, consecutive = "", 0
		}
	}
	if consecutive >= consecutiveOperationFailureLimit {
		return loopGuardRepeatedOperationFailure
	}
	return ""
}

// operationSignatureForTurn 摘要指定轮次内有序的操作指纹，供跨轮周期检测使用。
func operationSignatureForTurn(events []domain.Event, turnID domain.TurnID) string {
	fingerprints := make([]string, 0)
	for _, event := range events {
		if event.Type != domain.EventPolicyDecided || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload struct {
			Fingerprint string `json:"fingerprint"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Fingerprint != "" {
			fingerprints = append(fingerprints, payload.Fingerprint)
		}
	}
	if len(fingerprints) == 0 {
		return ""
	}
	return stableFingerprint(fingerprints)
}

// progressFingerprint 摘要 verifier 可见工作区、最近验证结果与累计工具证据集合，
// 忽略事件序号等假进展。证据以全部工具调用的 (tool, arguments) 去重集合参与指纹
// ——exec 探索与只读工具同等计入：模型把探索从 read/search 转向 exec grep 时证据
// 不应停止增长（跨模型实测：GLM TopicEye #1 读证据仅 7 项即冻结、8 轮被杀，其
// 探索 4 次走 exec）。一轮中出现任何首次调用的工具组合即视为新证据（指纹变化、
// 熔断窗口重置），重复完全相同的调用不产生新证据。工作区变异由观察值独立重置
// 窗口，因此计入 exec 不会掩盖真正的无进展；原地重复徘徊仍会被终止。
func progressFingerprint(workspaceObservation string, events []domain.Event) string {
	verificationResult := ""
	verificationOK := false
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != domain.EventVerificationFinished {
			continue
		}
		var payload domain.VerificationFinishedPayload
		if json.Unmarshal(events[index].Payload, &payload) == nil {
			verificationOK = payload.OK
			verificationResult = payload.ResultFingerprint
		}
		break
	}
	evidence := make([]string, 0, 16)
	seen := make(map[string]struct{}, 16)
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed {
			continue
		}
		var payload toolCallPayload
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		candidate := payload.ToolName + "\x00" + string(payload.Arguments)
		if _, duplicate := seen[candidate]; duplicate {
			continue
		}
		seen[candidate] = struct{}{}
		evidence = append(evidence, candidate)
	}
	sort.Strings(evidence)
	return stableFingerprint(struct {
		Workspace          string   `json:"workspace"`
		VerificationOK     bool     `json:"verification_ok"`
		VerificationResult string   `json:"verification_result,omitempty"`
		ReadEvidence       []string `json:"read_evidence,omitempty"`
	}{Workspace: workspaceObservation, VerificationOK: verificationOK, VerificationResult: verificationResult, ReadEvidence: evidence})
}

// turnLoopGuard 检查最近六个继续轮次的 A-B 周期和稳定无进展状态。
func turnLoopGuard(events []domain.Event) string {
	closed := make([]domain.TurnClosedPayload, 0, periodicTurnWindow)
	for _, event := range events {
		if event.Type != domain.EventTurnClosed {
			continue
		}
		var payload domain.TurnClosedPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Outcome == "continue" {
			closed = append(closed, payload)
		}
	}
	if len(closed) >= periodicTurnWindow {
		window := closed[len(closed)-periodicTurnWindow:]
		if window[0].OperationSignature != "" && window[1].OperationSignature != "" &&
			window[0].OperationSignature != window[1].OperationSignature &&
			window[0].OperationSignature == window[2].OperationSignature &&
			window[0].OperationSignature == window[4].OperationSignature &&
			window[1].OperationSignature == window[3].OperationSignature &&
			window[1].OperationSignature == window[5].OperationSignature {
			return loopGuardPeriodicTurnCycle
		}
	}
	if len(closed) >= noProgressTurnLimit {
		window := closed[len(closed)-noProgressTurnLimit:]
		baseline := window[0].ProgressFingerprint
		if baseline != "" {
			stable := true
			for _, payload := range window[1:] {
				stable = stable && payload.ProgressFingerprint == baseline
			}
			if stable {
				return loopGuardNoProgress
			}
		}
	}
	return ""
}

// currentOperationFailureGuard 从最新事件尾部判断是否刚达到连续失败阈值。
func (loop *Loop) currentOperationFailureGuard(ctx context.Context, runID domain.RunID) error {
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), runID)
	if err != nil {
		return err
	}
	if reason := repeatedOperationFailureGuard(events); reason != "" {
		return &LoopGuardExceededError{Reason: reason}
	}
	return nil
}

// stableToolResultFingerprint 摘要工具结果中的稳定字段，并剔除命令耗时等非语义噪声。
func stableToolResultFingerprint(result tools.Result) string {
	previewText := stripDurationField(result.Preview)
	preview := any(previewText)
	if result.Preview != "" {
		var decoded any
		if json.Unmarshal([]byte(previewText), &decoded) == nil {
			preview = removeVolatileResultFields(decoded)
		}
	}
	var data any
	if len(result.Data) > 0 && json.Unmarshal(result.Data, &data) == nil {
		data = removeVolatileResultFields(data)
	}
	return stableFingerprint(struct {
		Code      string `json:"code"`
		Data      any    `json:"data,omitempty"`
		Preview   any    `json:"preview,omitempty"`
		Truncated bool   `json:"truncated"`
		Bytes     int64  `json:"bytes"`
		SHA256    string `json:"sha256,omitempty"`
	}{Code: result.Code, Data: data, Preview: preview, Truncated: result.Truncated, Bytes: result.Bytes, SHA256: result.SHA256})
}

// stripDurationField 将可能被截断的 JSON 预览中的 duration_ms 数值归零。
func stripDurationField(value string) string {
	const marker = `"duration_ms":`
	for offset := 0; ; {
		index := strings.Index(value[offset:], marker)
		if index < 0 {
			return value
		}
		start := offset + index + len(marker)
		end := start
		for end < len(value) && value[end] >= '0' && value[end] <= '9' {
			end++
		}
		if end > start {
			value = value[:start] + "0" + value[end:]
			offset = start + 1
			continue
		}
		offset = start
	}
}

// removeVolatileResultFields 递归删除命令结果中的耗时字段，避免时间抖动伪装成 verifier 进展。
func removeVolatileResultFields(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, child := range typed {
			if key != "duration_ms" {
				cleaned[key] = removeVolatileResultFields(child)
			}
		}
		return cleaned
	case []any:
		cleaned := make([]any, len(typed))
		for index, child := range typed {
			cleaned[index] = removeVolatileResultFields(child)
		}
		return cleaned
	default:
		return value
	}
}

// stableFingerprint 对可 JSON 编码的稳定状态计算内容寻址摘要。
func stableFingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("encode loop guard fingerprint: %w", err))
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// continuationPayload 为继续轮次生成可回放的操作与进展摘要；events 是调用方
// 已加载的当前事件视图，避免同一收尾流程内重复全量重载日志。
func (loop *Loop) continuationPayload(events []domain.Event, turnID domain.TurnID) (domain.TurnClosedPayload, error) {
	observation, err := loop.verificationObservation()
	if err != nil {
		return domain.TurnClosedPayload{}, fmt.Errorf("observe progress before closing turn: %w", err)
	}
	return domain.TurnClosedPayload{
		Outcome:             "continue",
		OperationSignature:  operationSignatureForTurn(events, turnID),
		ProgressFingerprint: progressFingerprint(observation, events),
	}, nil
}

// closeContinuationTurn 持久化继续轮次摘要，并在最近窗口满足周期或无进展规则时终止运行。
// 全流程只加载一次事件日志：追加 TurnClosed 后在内存中合成同型事件供跨轮检测，
// 其载荷字段与落库事件一致，无需为刚写入的事件再次全量重载。
func (loop *Loop) closeContinuationTurn(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) error {
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return err
	}
	if reason := repeatedOperationFailureGuard(events); reason != "" {
		return loop.persistLoopGuardExceeded(ctx, projection, input, reason)
	}
	payload, err := loop.continuationPayload(events, turnID)
	if err != nil {
		return err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(payload)}); err != nil {
		return err
	}
	events = append(events, domain.Event{Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(payload)})
	if reason := turnLoopGuard(events); reason != "" {
		return loop.persistLoopGuardExceeded(ctx, projection, input, reason)
	}
	return nil
}
