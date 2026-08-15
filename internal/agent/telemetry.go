// Package agent 包含串行、单写入者的 Agent 执行循环。
package agent

import (
	"encoding/json"

	"github.com/fxbin/cairn/internal/domain"
)

// TelemetrySink 只消费已经成功追加到事件日志的领域事件并派生语义观测信号。
// 遥测可以丢失、采样或停用，永远不能决定 completed、replay、resume、reconcile
// 或 verifier 结果——事件日志是唯一运行事实源（路线图 AB-O1 事实合同）。
type TelemetrySink interface {
	ObserveRunEvent(event domain.Event)
}

// SemanticTelemetry 是 TelemetrySink 的参考实现：从已持久化事件派生 Run、Turn、
// Operation、Verifier 与 Budget 维度的聚合语义计数。它无状态副作用、不落盘，
// 供诊断与后续 OTLP 导出器复用；默认不接入任何运行路径。
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
