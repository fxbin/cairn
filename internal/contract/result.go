package contract

import (
	"encoding/json"

	"github.com/fxbin/cairn/internal/domain"
)

// resultSchemaVersion 是 Result 的当前产品合同版本。
const resultSchemaVersion = 1

// CompletionEvidence 绑定一次运行的完成依据：最新验证序号与结果、最后
// 变更序号，以及"验证覆盖全部变更"的可信完成判定。它绝不引用模型自述。
type CompletionEvidence struct {
	VerificationSeq uint64 `json:"verification_seq,omitempty"`
	VerificationOK  bool   `json:"verification_ok,omitempty"`
	LastMutationSeq uint64 `json:"last_mutation_seq,omitempty"`
	Trusted         bool   `json:"trusted,omitempty"`
}

// ResultMetrics 汇总一次运行的非敏感资源消耗；ToolCalls 统计实际开始执行的
// 操作数（operation_started），不含被拒绝或参数非法的模型提议。UsageEstimated
// 为真表示 token 数中至少一项来自字节估算而非 Provider 实际回报。
type ResultMetrics struct {
	Turns              int   `json:"turns"`
	ToolCalls          int   `json:"tool_calls"`
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	UsageEstimated     bool  `json:"usage_estimated,omitempty"`
	ContextCompactions int   `json:"context_compactions"`
	WallTimeMS         int64 `json:"wall_time_ms,omitempty"`
}

// Result 是一次 Run 的版本化终局投影：内嵌 RunManifest 定位身份，
// Outcome/State/FailureClass/StopReason 描述结局，Evidence 与 Metrics
// 提供完成证据与资源事实。它完全由事件派生，不含最终回复正文。
type Result struct {
	SchemaVersion int                `json:"schema_version"`
	Manifest      RunManifest        `json:"manifest"`
	State         string             `json:"state"`
	Outcome       string             `json:"outcome"`
	FailureClass  string             `json:"failure_class,omitempty"`
	StopReason    string             `json:"stop_reason,omitempty"`
	Evidence      CompletionEvidence `json:"evidence"`
	Metrics       ResultMetrics      `json:"metrics"`
}

// CompletionTrusted 是"可信完成"判定的唯一公式：运行必须处于 completed、
// 验证成功、存在验证事件、验证序号覆盖最后一次工作区变更，且冻结计划
// 的全部命令均有成功终态。无完整命令证据（含零命令空验证）
// 一律不可信；只读任务可在无命令计划下 completed，但 trusted=false。
// app 的 RunSummary 与本包 Result 共用该实现，避免两套判定漂移。
func CompletionTrusted(state domain.RunState, verificationSucceeded bool, lastVerificationSeq, lastMutationSeq uint64, verificationCommandsSucceeded bool) bool {
	return state == domain.RunCompleted && verificationSucceeded && lastVerificationSeq != 0 && verificationCommandsSucceeded && (lastMutationSeq == 0 || lastVerificationSeq > lastMutationSeq)
}

// ResultFromRun 由事件流投影 Result。它先经 ManifestFromRun 构建身份，
// 再回放事件汇总终态、失败分类、完成证据与指标；输入事件必须以
// run_created 开头且已通过哈希链校验（与 domain.Replay 的输出一致）。
// verificationPlan 是最新验证引用的已校验制品正文；缺失时可读但不可信。
func ResultFromRun(events []domain.Event, projection domain.RunProjection, task TaskSpec, frozenConfig, verificationPlan json.RawMessage) (Result, error) {
	manifest, err := ManifestFromRun(events, task, frozenConfig)
	if err != nil {
		return Result{}, err
	}
	result := Result{SchemaVersion: resultSchemaVersion, Manifest: manifest, State: string(projection.State), Outcome: projection.Phase()}
	result.FailureClass = ClassifyTerminal(events)
	result.StopReason = TerminalStopReason(events)
	for _, event := range events {
		switch event.Type {
		case domain.EventTurnOpened:
			result.Metrics.Turns++
		case domain.EventOperationStarted:
			result.Metrics.ToolCalls++
		case domain.EventContextCompacted:
			result.Metrics.ContextCompactions++
		case domain.EventModelResponseFinished:
			var payload struct {
				Usage *struct {
					PromptTokens     int64 `json:"prompt_tokens"`
					CompletionTokens int64 `json:"completion_tokens"`
				} `json:"usage"`
				UsageEstimated bool `json:"usage_estimated"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Usage != nil {
				result.Metrics.InputTokens += payload.Usage.PromptTokens
				result.Metrics.OutputTokens += payload.Usage.CompletionTokens
				result.Metrics.UsageEstimated = result.Metrics.UsageEstimated || payload.UsageEstimated
			}
		case domain.EventVerificationFinished:
			var payload domain.VerificationFinishedPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				result.Evidence.VerificationSeq = event.Seq
				result.Evidence.VerificationOK = payload.OK
			}
		}
	}
	result.Evidence.LastMutationSeq = projection.LastMutationSeq
	commandsSucceeded := VerificationCommandsSucceeded(events, VerificationEvidenceSpec{
		Plan: verificationPlan, Commands: manifest.Verification.Commands,
		Fingerprint: manifest.Verification.Fingerprint, ConfigFingerprint: manifest.ConfigFingerprint,
		FinishedSeq: projection.LastVerificationSeq, LastMutationSeq: projection.LastMutationSeq,
	})
	result.Evidence.Trusted = CompletionTrusted(projection.State, projection.VerificationSucceeded, projection.LastVerificationSeq, projection.LastMutationSeq, commandsSucceeded)
	if len(events) != 0 {
		first, last := events[0], events[len(events)-1]
		if duration := last.OccurredAt.Sub(first.OccurredAt); duration > 0 {
			result.Metrics.WallTimeMS = duration.Milliseconds()
		}
	}
	return result, nil
}

// TerminalStopReason 从终态事件提取人可读停止原因；它是 app 诊断摘要与
// contract Result 共用的唯一实现，只读取已脱敏的持久化文本。
func TerminalStopReason(events []domain.Event) string {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		switch event.Type {
		case domain.EventRunFailed:
			var payload struct {
				Error      string `json:"error"`
				ErrorClass string `json:"error_class"`
				Budget     string `json:"budget_dimension"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				return "run failed"
			}
			reason := payload.ErrorClass
			if reason == "" {
				reason = payload.Budget
			}
			if reason == "" {
				reason = payload.Error
			}
			if reason == "" {
				reason = "run failed"
			}
			return reason
		case domain.EventRunCancelled:
			var payload struct {
				Cause string `json:"cause"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Cause != "" {
				return "cancelled: " + payload.Cause
			}
			return "cancelled"
		case domain.EventRunCompleted:
			return ""
		}
	}
	return ""
}
