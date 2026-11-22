// [INPUT]: 依赖终态 Run Journal、冻结验收合同与工作区制品。
// [OUTPUT]: 提供只读验证的目标版本、标准正文与有界制品引用。
// [POS]: 由应用层从宿主事实构造上下文，不读 Worker 会话正文。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件构造独立验证器的目标上下文。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

const (
	verifierContextMaxArtifacts = 32
	verifierContextMaxBytes     = 128 << 10
)

// ErrVerifierTarget 表示目标 Run 的身份、终态或制品无法核验。
var ErrVerifierTarget = errors.New("verification target is invalid")

// verifierContext 只包含宿主可追溯的目标与验收输入。
type verifierContext struct {
	TargetRunID          domain.RunID                   `json:"target_run_id"`
	TerminalSequence     uint64                         `json:"terminal_sequence"`
	TargetVersion        string                         `json:"target_version"`
	WorkspaceObservation string                         `json:"workspace_observation"`
	AcceptanceDigest     string                         `json:"acceptance_digest"`
	Criteria             []contract.AcceptanceCriterion `json:"criteria"`
	ProgramCriteria      []contract.CriterionEvidence   `json:"program_criteria,omitempty"`
	Artifacts            []verifierArtifact             `json:"artifacts"`
	ArtifactsLimited     bool                           `json:"artifacts_limited"`
}

// verifierArtifact 将制品引用锚定到目标 Journal 的具体事件。
type verifierArtifact struct {
	RunID         domain.RunID     `json:"run_id"`
	EventSequence uint64           `json:"event_sequence"`
	EventType     domain.EventType `json:"event_type"`
	Path          string           `json:"path,omitempty"`
	Ref           contract.Ref     `json:"ref"`
}

// loadVerifierContext 复核目标归属和终态，收集已归档制品及冻结标准正文。
func loadVerifierContext(ctx context.Context, journal ports.Journal, workspaceHash, workspaceObservation string, request VerifierRequest) (verifierContext, error) {
	criteria, err := verifierCriteria(request)
	if err != nil {
		return verifierContext{}, err
	}
	events, err := journal.Load(ctx, request.TargetRunID)
	if err != nil {
		return verifierContext{}, fmt.Errorf("%w: %w", ErrVerifierTarget, err)
	}
	target, err := domain.Replay(events)
	if err != nil {
		return verifierContext{}, fmt.Errorf("%w: replay: %w", ErrVerifierTarget, err)
	}
	if target.ID != request.TargetRunID || target.WorkspaceHash != workspaceHash {
		return verifierContext{}, fmt.Errorf("%w: run is missing or belongs to another workspace", ErrVerifierTarget)
	}
	if target.State != domain.RunCompleted && target.State != domain.RunFailed {
		return verifierContext{}, fmt.Errorf("%w: run is not a completed or failed execution", ErrVerifierTarget)
	}
	terminal := events[len(events)-1]
	if terminal.Type != domain.EventRunCompleted && terminal.Type != domain.EventRunFailed {
		return verifierContext{}, fmt.Errorf("%w: run has no terminal anchor at its chain head", ErrVerifierTarget)
	}
	program, err := verifierProgramContext(events, target, workspaceObservation, request, workspaceHash)
	if err != nil {
		return verifierContext{}, err
	}
	artifacts, limited, err := verifierTargetArtifacts(events, manifestArtifactRoot(request.StateDir, workspaceHash))
	if err != nil {
		return verifierContext{}, fmt.Errorf("%w: %w", ErrVerifierTarget, err)
	}
	return verifierContext{
		TargetRunID: target.ID, TerminalSequence: terminal.Seq, TargetVersion: terminal.EventHash,
		WorkspaceObservation: workspaceObservation, AcceptanceDigest: request.Acceptance.Digest,
		Criteria: criteria, ProgramCriteria: program, Artifacts: artifacts, ArtifactsLimited: limited,
	}, nil
}

// verifierProgramContext 核对历史工作区观察及冻结合同，并只传标准级检查状态。
func verifierProgramContext(events []domain.Event, target domain.RunProjection, current string, request VerifierRequest, hash string) ([]contract.CriterionEvidence, error) {
	if target.LastVerificationSeq == 0 {
		return nil, fmt.Errorf("%w: run has no verifiable workspace version", ErrVerifierTarget)
	}
	index := int(target.LastVerificationSeq - 1)
	finish := events[index]
	var payload domain.VerificationFinishedPayload
	if finish.TurnID == nil || json.Unmarshal(finish.Payload, &payload) != nil || payload.VerificationID == "" {
		return nil, fmt.Errorf("%w: run has no bound verification finish", ErrVerifierTarget)
	}
	start, found := verificationStartedForFinish(events, index, *finish.TurnID, payload.VerificationID)
	if !found || start.workspaceObservation == "" || start.lastMutationSeq != target.LastMutationSeq ||
		(payload.WorkspaceObservation != "" && payload.WorkspaceObservation != start.workspaceObservation) {
		return nil, fmt.Errorf("%w: run verification version is missing or stale", ErrVerifierTarget)
	}
	if current != start.workspaceObservation {
		return nil, fmt.Errorf("%w: workspace differs from the target run version", ErrVerifierTarget)
	}
	if start.planRef == nil {
		return nil, fmt.Errorf("%w: run has no frozen verification plan", ErrVerifierTarget)
	}
	content, err := readArtifactContent(manifestArtifactRoot(request.StateDir, hash), *start.planRef)
	if err != nil {
		return nil, fmt.Errorf("%w: read frozen verification plan: %w", ErrVerifierTarget, err)
	}
	var plan contract.VerificationPlanSpec
	if json.Unmarshal(content, &plan) != nil || plan.Version != contract.VerificationPlanVersion ||
		plan.VerificationID != payload.VerificationID || plan.LastMutationSeq != target.LastMutationSeq ||
		plan.ConfigFingerprint != target.ConfigFingerprint || plan.CommandsFingerprint != start.commandsFingerprint {
		return nil, fmt.Errorf("%w: verification plan does not match the target journal", ErrVerifierTarget)
	}
	if plan.Acceptance == nil || plan.Acceptance.Validate() != nil || plan.Acceptance.Digest != request.Acceptance.Digest {
		return nil, fmt.Errorf("%w: frozen acceptance contract does not match the request", ErrVerifierTarget)
	}
	if len(payload.EvidenceRef) == 0 {
		return nil, nil
	}
	evidence, err := acceptanceEvidenceFromRun(events, finish.Seq, request.StateDir, hash, request.Acceptance)
	if err != nil || evidence == nil {
		return nil, fmt.Errorf("%w: target acceptance evidence cannot be bound", ErrVerifierTarget)
	}
	return evidence.Criteria, nil
}

// verifierTargetArtifacts 只选择已执行的变更结果和程序验证证据。事件中的
// 预览、模型文本、请求参数、配置与规约正文均不进入提示。
func verifierTargetArtifacts(events []domain.Event, root string) ([]verifierArtifact, bool, error) {
	started := make(map[domain.OperationID]string)
	var candidates []verifierArtifact
	for _, event := range events {
		var payload struct {
			SideEffectClass string        `json:"side_effect_class"`
			PlannedPath     string        `json:"planned_path"`
			ResultRef       *contract.Ref `json:"result_ref"`
			EvidenceRef     *contract.Ref `json:"evidence_ref"`
		}
		switch event.Type {
		case domain.EventOperationStarted, domain.EventOperationSucceeded, domain.EventVerificationFinished:
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, false, fmt.Errorf("decode artifact source event %d: %w", event.Seq, err)
			}
		default:
			continue
		}
		switch event.Type {
		case domain.EventOperationStarted:
			if payload.SideEffectClass == "workspace" || payload.SideEffectClass == "process" {
				started[*event.OperationID] = payload.PlannedPath
			}
		case domain.EventOperationSucceeded:
			path, ok := started[*event.OperationID]
			if ok && payload.ResultRef != nil {
				candidates = append(candidates, verifierArtifact{RunID: event.RunID, EventSequence: event.Seq, EventType: event.Type, Path: path, Ref: *payload.ResultRef})
			}
		case domain.EventVerificationFinished:
			if payload.EvidenceRef != nil {
				candidates = append(candidates, verifierArtifact{RunID: event.RunID, EventSequence: event.Seq, EventType: event.Type, Ref: *payload.EvidenceRef})
			}
		}
	}
	refs := make([]verifierArtifact, 0, min(len(candidates), verifierContextMaxArtifacts))
	seen := make(map[string]bool, len(refs))
	for index := len(candidates) - 1; index >= 0; index-- {
		candidate := candidates[index]
		if seen[candidate.Ref.SHA256] {
			continue
		}
		if len(refs) == verifierContextMaxArtifacts {
			return refs, true, nil
		}
		if err := checkArtifactReference(root, candidate.Ref); err != nil {
			return nil, false, fmt.Errorf("artifact from target event %d: %w", candidate.EventSequence, err)
		}
		candidate.Ref.TruncatedPreview = ""
		refs = append(refs, candidate)
		seen[candidate.Ref.SHA256] = true
	}
	return refs, false, nil
}
