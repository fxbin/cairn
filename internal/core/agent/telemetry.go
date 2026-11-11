// Package agent 包含串行、单写入者的 Agent 执行循环。
package agent

import (
	"encoding/json"

	"github.com/fxbin/cairn/internal/domain"
)

// TelemetrySink 是早期语义遥测的窄端口：只消费已经成功追加到事件日志的领域
// 事件并派生语义观测信号。遥测可以丢失、采样或停用，永远不能决定 completed、
// replay、resume、reconcile 或 verifier 结果——事件日志是唯一运行事实源
// （路线图 AB-O1 事实合同）。
//
// 已被 internal/telemetry.Sink 取代：现行运行路径只使用 internal/telemetry 的
// 聚合与导出面。本接口与其参考实现保留为历史参考，不再接入任何运行路径，
// 也不得被新的代码引用。
type TelemetrySink interface {
	ObserveRunEvent(event domain.Event)
}

// SemanticTelemetry 是 TelemetrySink 的历史参考实现：从已持久化事件派生 Run、
// Turn、Operation、Verifier 与 Budget 维度的聚合语义计数。它无状态副作用、
// 不落盘。
//
// 已被 internal/telemetry 取代：现行语义观测面在 internal/telemetry 内实现，
// 额外承担 Trace 关联、度量口径、维度白名单、隐私守卫与可选导出。本实现保留
// 只为说明既有合同，现已没有调用方。
type SemanticTelemetry struct {
	RunID          string
	Turns          int
	Operations     int
	OperationsByID map[int]string
	VerifierOK     int
	VerifierFailed int
	InputTokens    int64
	OutputTokens   int64
	StopReason     string
	Completed      bool
}

// NewSemanticTelemetry 创建以 cairn.run.id 关联的语义遥测记录器；run id 用于
// span 关联而非维度标签，避免高基数。
func NewSemanticTelemetry(runID string) *SemanticTelemetry {
	return &SemanticTelemetry{RunID: runID, OperationsByID: make(map[int]string)}
}

// ObserveRunEvent 处理单个已持久化事件；解码失败静默跳过（遥测不产生错误路径）。
func (telemetry *SemanticTelemetry) ObserveRunEvent(event domain.Event) {
	switch event.Type {
	case domain.EventTurnOpened:
		telemetry.Turns++
	case domain.EventOperationStarted:
		telemetry.Operations++
	case domain.EventVerificationFinished:
		var payload domain.VerificationFinishedPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			if payload.OK {
				telemetry.VerifierOK++
			} else {
				telemetry.VerifierFailed++
			}
		}
	case domain.EventModelResponseFinished:
		var payload struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Usage != nil {
			telemetry.InputTokens += int64(payload.Usage.PromptTokens)
			telemetry.OutputTokens += int64(payload.Usage.CompletionTokens)
		}
	case domain.EventRunFailed:
		var payload struct {
			ErrorClass string `json:"error_class"`
			Budget     string `json:"budget_dimension"`
			Error      string `json:"error"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil {
			switch {
			case payload.Budget != "":
				telemetry.StopReason = "budget:" + payload.Budget
			case payload.ErrorClass != "":
				telemetry.StopReason = payload.ErrorClass
			case payload.Error != "":
				telemetry.StopReason = payload.Error
			default:
				telemetry.StopReason = "run_failed"
			}
		}
	case domain.EventRunCompleted:
		telemetry.Completed = true
	case domain.EventRunCancelled:
		telemetry.StopReason = "cancelled"
	}
}
