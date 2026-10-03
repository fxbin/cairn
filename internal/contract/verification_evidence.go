// verification_evidence.go 从已校验的 Journal 事件中核对冻结验证命令的完整执行链。
//
// Author: fxbin
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/fxbin/cairn/internal/domain"
)

// verificationCommandRecord 保存一条冻结命令从提议到终态的最小可信证据。
type verificationCommandRecord struct {
	operationID       domain.OperationID
	askFingerprint    string
	approvalRequested bool
	approvalGranted   bool
	allowed           bool
	started           bool
	terminal          bool
	succeeded         bool
}

// VerificationEvidenceSpec 绑定最新验证的冻结配置、计划制品与完成序号；计划
// 正文须由调用方按 plan_ref 从已校验的内容寻址存储读取。
type VerificationEvidenceSpec struct {
	Plan              json.RawMessage
	Commands          int
	Fingerprint       string
	ConfigFingerprint string
	FinishedSeq       uint64
	LastMutationSeq   uint64
}

// VerificationCommandsSucceeded 仅在指定验证的每条冻结命令按序提议、获
// Policy 授权、实际 started，且以成功操作或成功对账终结时返回真。调用方应
// 先验证哈希链并经 domain.Replay 校验事件状态；缺失旧证据只降低可信等级，
// 不改变旧事件的可回放性。
func VerificationCommandsSucceeded(events []domain.Event, spec VerificationEvidenceSpec) bool {
	if spec.Commands <= 0 || spec.Fingerprint == "" || spec.ConfigFingerprint == "" || spec.FinishedSeq == 0 || len(spec.Plan) == 0 {
		return false
	}
	finishIndex := -1
	var verificationID string
	var turnID domain.TurnID
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationFinished || event.Seq != spec.FinishedSeq || event.TurnID == nil {
			continue
		}
		var payload domain.VerificationFinishedPayload
		if json.Unmarshal(event.Payload, &payload) != nil || !payload.OK || payload.VerificationID == "" {
			return false
		}
		finishIndex = index
		verificationID = payload.VerificationID
		turnID = *event.TurnID
		break
	}
	if finishIndex < 0 {
		return false
	}
	startIndex := -1
	var startPayload struct {
		VerificationID       string `json:"verification_id"`
		CommandsFingerprint  string `json:"commands_fingerprint"`
		LastMutationSeq      uint64 `json:"last_mutation_seq"`
		WorkspaceObservation string `json:"workspace_observation"`
		PlanRef              *struct {
			SHA256 string `json:"sha256"`
		} `json:"plan_ref"`
	}
	for index := finishIndex - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationStarted || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		if json.Unmarshal(event.Payload, &startPayload) != nil || startPayload.VerificationID != verificationID || startPayload.CommandsFingerprint != spec.Fingerprint || startPayload.PlanRef == nil || startPayload.WorkspaceObservation == "" {
			return false
		}
		startIndex = index
		break
	}
	if startIndex < 0 {
		return false
	}
	for _, event := range events[finishIndex+1:] {
		if event.Type != domain.EventRunResumed {
			continue
		}
		var payload struct {
			WorkspaceObservation string `json:"workspace_observation"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || (payload.WorkspaceObservation != "" && payload.WorkspaceObservation != startPayload.WorkspaceObservation) {
			return false
		}
	}
	planDigest := sha256.Sum256(spec.Plan)
	if startPayload.PlanRef.SHA256 != "sha256:"+hex.EncodeToString(planDigest[:]) {
		return false
	}
	var plan VerificationPlanSpec
	if json.Unmarshal(spec.Plan, &plan) != nil || plan.Version != VerificationPlanVersion || plan.VerificationID != verificationID || plan.ConfigFingerprint != spec.ConfigFingerprint || plan.LastMutationSeq != startPayload.LastMutationSeq || plan.LastMutationSeq != spec.LastMutationSeq || len(plan.Commands) != spec.Commands || plan.CommandsFingerprint != spec.Fingerprint {
		return false
	}
	actualFingerprint, err := FingerprintVerificationCommands(plan.Commands)
	if err != nil || actualFingerprint != spec.Fingerprint {
		return false
	}
	records := make([]verificationCommandRecord, spec.Commands)
	byOperation := make(map[domain.OperationID]int, spec.Commands)
	proposed := 0
	for index := startIndex + 1; index < finishIndex; index++ {
		event := events[index]
		if event.Type == domain.EventOperationStarted {
			var payload struct {
				SideEffectClass string `json:"side_effect_class"`
				MutationBarrier bool   `json:"mutation_barrier"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.SideEffectClass == "workspace" || (payload.SideEffectClass == "process" && payload.MutationBarrier) {
				return false
			}
			if event.OperationID == nil {
				return false
			}
			if _, known := byOperation[*event.OperationID]; !known {
				return false
			}
		}
		if event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		if event.Type == domain.EventVerificationStarted || event.Type == domain.EventVerificationFinished {
			return false
		}
		if event.Type == domain.EventVerificationCommandProposed {
			if event.OperationID == nil || proposed >= spec.Commands {
				return false
			}
			var payload struct {
				VerificationID string          `json:"verification_id"`
				CommandIndex   int             `json:"command_index"`
				Arguments      json.RawMessage `json:"arguments"`
				PlanRef        *struct {
					SHA256 string `json:"sha256"`
				} `json:"plan_ref"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.VerificationID != verificationID || payload.CommandIndex != proposed || payload.PlanRef == nil || payload.PlanRef.SHA256 != startPayload.PlanRef.SHA256 {
				return false
			}
			actualArguments, actualErr := canonicalVerificationArguments(payload.Arguments)
			expectedArguments, expectedErr := canonicalVerificationArguments(plan.Commands[proposed].Arguments)
			if actualErr != nil || expectedErr != nil || !bytes.Equal(actualArguments, expectedArguments) {
				return false
			}
			if _, duplicate := byOperation[*event.OperationID]; duplicate {
				return false
			}
			records[proposed].operationID = *event.OperationID
			byOperation[*event.OperationID] = proposed
			proposed++
			continue
		}
		if event.OperationID == nil {
			continue
		}
		commandIndex, found := byOperation[*event.OperationID]
		if !found {
			continue
		}
		record := &records[commandIndex]
		switch event.Type {
		case domain.EventPolicyDecided:
			var payload struct {
				Effect      string `json:"effect"`
				Fingerprint string `json:"fingerprint"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || record.started || payload.Fingerprint == "" {
				return false
			}
			switch payload.Effect {
			case "ask":
				if record.allowed || record.askFingerprint != "" || payload.Fingerprint == "" {
					return false
				}
				record.askFingerprint = payload.Fingerprint
			case "allow":
				if record.allowed || (record.askFingerprint != "" && (!record.approvalGranted || payload.Fingerprint != record.askFingerprint)) {
					return false
				}
				record.allowed = true
			case "deny":
				return false
			default:
				return false
			}
		case domain.EventApprovalRequested:
			var payload struct {
				Fingerprint string `json:"fingerprint"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || record.askFingerprint == "" || record.approvalRequested || payload.Fingerprint != record.askFingerprint {
				return false
			}
			record.approvalRequested = true
		case domain.EventApprovalGranted:
			var payload struct {
				Fingerprint string `json:"fingerprint"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || !record.approvalRequested || record.approvalGranted || payload.Fingerprint != record.askFingerprint {
				return false
			}
			record.approvalGranted = true
		case domain.EventApprovalDenied:
			return false
		case domain.EventOperationStarted:
			var payload struct {
				ToolName        string `json:"tool_name"`
				SideEffectClass string `json:"side_effect_class"`
				MutationBarrier bool   `json:"mutation_barrier"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.ToolName != "exec_command" || payload.SideEffectClass != "process" || payload.MutationBarrier || !record.allowed || record.started {
				return false
			}
			record.started = true
		case domain.EventOperationSucceeded:
			var payload struct {
				ToolName string `json:"tool_name"`
				Result   struct {
					OK bool `json:"ok"`
				} `json:"result"`
			}
			if !record.started || record.terminal || json.Unmarshal(event.Payload, &payload) != nil || payload.ToolName != "exec_command" || !payload.Result.OK {
				return false
			}
			record.terminal = true
			record.succeeded = true
		case domain.EventOperationReconciled:
			var payload struct {
				Verdict              string `json:"verdict"`
				WorkspaceObservation string `json:"workspace_observation"`
			}
			if !record.started || record.terminal || json.Unmarshal(event.Payload, &payload) != nil || payload.Verdict != "applied_success" || payload.WorkspaceObservation != startPayload.WorkspaceObservation {
				return false
			}
			record.terminal = true
			record.succeeded = true
		case domain.EventOperationFailed, domain.EventOperationCancelled:
			return false
		}
	}
	if proposed != spec.Commands {
		return false
	}
	for _, record := range records {
		if !record.succeeded {
			return false
		}
	}
	return true
}
