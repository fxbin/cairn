// [INPUT]: 冻结工作流定义、Worker 与 Program Journal、独立审查输出。
// [OUTPUT]: 组装固定程序验证和隔离审查，持久化逐标准验收报告。
// [POS]: 复用步骤尝试与 Run 锁。应用门面只接受可追溯的证据。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接程序验证和独立审查节点。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

const (
	workflowVerificationWorker = iota
	workflowVerificationProgram
	workflowVerificationReview
)

const (
	workflowReviewInvalid = "verification_review_invalid"
	workflowReviewUnclear = "verification_review_inconclusive"
)

// workflowReviewReceipt 锚定独立审查终态及当时验证的工作区版本。
type workflowReviewReceipt struct {
	RunID          domain.RunID       `json:"run_id"`
	Terminal       workflow.RunAnchor `json:"terminal"`
	SubjectVersion string             `json:"subject_version"`
	Findings       ValidatedFindings  `json:"findings"`
}

// workflowConfiguredNode 从冻结定义决定每个阶段的配置。
func workflowConfiguredNode(base config.Config, definition *WorkflowDefinition, node WorkflowNode) (config.Config, error) {
	if node.Kind == workflow.StepKindReview {
		return verifierConfig(base)
	}
	if definition.Verification != nil && node.ID == definition.Nodes[workflowVerificationWorker].ID {
		base.Verification.Commands = nil
		base.Verification.AutoAfterPatch = false
	}
	return workflowNodeConfig(base, node)
}

// requireIsolatedVerificationState 拒绝把 Agent 状态和制品存放在审查工具视图内。
func requireIsolatedVerificationState(workspaceRoot, stateDir string) error {
	directory := stateDir
	for {
		resolved, err := filepath.EvalSymlinks(directory)
		if err == nil {
			relative, err := filepath.Rel(workspaceRoot, resolved)
			if err != nil {
				return err
			}
			if directory != stateDir {
				suffix, err := filepath.Rel(directory, stateDir)
				if err != nil {
					return err
				}
				relative = filepath.Join(relative, suffix)
			}
			if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return errors.New("independent verification requires its state directory outside the workspace")
			}
			return nil
		}
		parent := filepath.Dir(directory)
		if !errors.Is(err, os.ErrNotExist) || parent == directory {
			return err
		}
		directory = parent
	}
}

// workflowAttemptSession 将独立审查从 Worker 会话链切离。
func workflowAttemptSession(sessionID, kind string) string {
	if kind == workflow.StepKindReview {
		return ""
	}
	return sessionID
}

// requireWorkflowVerificationVersion 在 Run 排他锁内核对被审查工作区版本。
func requireWorkflowVerificationVersion(boot *Boot, expected string) error {
	if boot.Loop.VerificationWorkspace == nil || expected == "" {
		return fmt.Errorf("%w: verification subject observation is missing", ErrVerifierTarget)
	}
	actual, err := boot.Loop.VerificationWorkspace.Observation()
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("%w: workspace differs from the frozen verification subject", ErrVerifierTarget)
	}
	return nil
}

// requireCurrentWorkflowVerification 供持有工作区锁的显式验收核对报告。
func requireCurrentWorkflowVerification(workspacePath string, configuration config.Config, report *WorkflowVerificationReport) error {
	if report == nil || report.Evaluation.Verdict != contract.VerdictPassed {
		return fmt.Errorf("%w: no passed independent verification report", ErrVerifierTarget)
	}
	subject, ok := report.Scope.Object(workflowReviewCoverageID)
	if !ok {
		return fmt.Errorf("%w: no independent review subject", ErrVerifierTarget)
	}
	workspace, _, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return err
	}
	actual, err := workspace.ObservationView(configuration.Verification.VolatilePaths).Observation()
	if err != nil {
		return err
	}
	if actual != subject.Version {
		return fmt.Errorf("%w: workspace changed after independent verification", ErrVerifierTarget)
	}
	return nil
}

// workflowVerificationSource 加载并核对当前实例的某个已成功节点。
func (execution workflowExecution) workflowVerificationSource(ctx context.Context, snapshot *workflow.Snapshot, index int) (domain.RunProjection, []domain.Event, error) {
	node := execution.definition.Nodes[index]
	attempts := snapshot.Attempts[node.ID]
	if len(attempts) == 0 || attempts[len(attempts)-1].State != workflow.StepSucceeded {
		return domain.RunProjection{}, nil, fmt.Errorf("%w: verification source %q did not settle successfully", ErrVerifierTarget, node.ID)
	}
	return execution.loadVersionedStepRun(ctx, workflowAttemptSession(snapshot.SessionID, node.Kind), attempts[len(attempts)-1])
}

// workflowObservedRunVersion 从该 Run 最后的验证事件取工作区观察。
func workflowObservedRunVersion(run domain.RunProjection, events []domain.Event) (string, error) {
	if run.LastVerificationSeq == 0 || run.WorkspaceObservation == "" {
		return "", fmt.Errorf("%w: run %s has no observed subject version", ErrVerifierTarget, run.ID)
	}
	finish := events[run.LastVerificationSeq-1]
	var payload domain.VerificationFinishedPayload
	if finish.TurnID == nil || json.Unmarshal(finish.Payload, &payload) != nil {
		return "", fmt.Errorf("%w: run %s has no bound verification finish", ErrVerifierTarget, run.ID)
	}
	start, ok := verificationStartedForFinish(events, int(finish.Seq-1), *finish.TurnID, payload.VerificationID)
	if !ok || start.lastMutationSeq != run.LastMutationSeq || start.workspaceObservation != run.WorkspaceObservation {
		return "", fmt.Errorf("%w: run %s workspace observation is stale", ErrVerifierTarget, run.ID)
	}
	return start.workspaceObservation, nil
}

// workflowReviewPrompt 只向审查传递冻结标准、程序证据及 Worker 变更路径。
func (execution workflowExecution) workflowReviewPrompt(ctx context.Context, snapshot *workflow.Snapshot) (string, string, error) {
	worker, workerEvents, err := execution.workflowVerificationSource(ctx, snapshot, workflowVerificationWorker)
	if err != nil {
		return "", "", err
	}
	program, programEvents, err := execution.workflowVerificationSource(ctx, snapshot, workflowVerificationProgram)
	if err != nil {
		return "", "", err
	}
	version, err := workflowObservedRunVersion(worker, workerEvents)
	if err != nil {
		return "", "", err
	}
	request := VerifierRequest{TargetRunID: program.ID, StateDir: execution.stateDir, Acceptance: *execution.definition.Verification}
	criteria, err := verifierProgramContext(programEvents, program, version, request, execution.workspaceHash)
	if err != nil || len(criteria) == 0 {
		return "", "", errors.Join(errors.New("workflow program verification has no bound criterion evidence"), err)
	}
	refRoot := manifestArtifactRoot(execution.stateDir, execution.workspaceHash)
	artifacts, limited, err := verifierTargetArtifacts(workerEvents, refRoot)
	if err != nil {
		return "", "", err
	}
	terminal := workerEvents[len(workerEvents)-1]
	prompt, err := verifierTaskPrompt(verifierContext{
		TargetRunID: worker.ID, TerminalSequence: terminal.Seq, TargetVersion: terminal.EventHash,
		WorkspaceObservation: version, AcceptanceDigest: request.Acceptance.Digest,
		Criteria: request.Acceptance.Criteria, ProgramCriteria: criteria, Artifacts: artifacts, ArtifactsLimited: limited,
	})
	return prompt, version, err
}

// workflowVerificationReport 投影已经结算的程序或独立审查。报告从不授予验收。
func (execution workflowExecution) workflowVerificationReport(ctx context.Context, id workflow.ID, node WorkflowNode, run domain.RunProjection, events []domain.Event, classification string) (*WorkflowVerificationReport, string, error) {
	if execution.definition.Verification == nil || (node.Kind != workflow.StepKindVerify && node.Kind != workflow.StepKindReview) {
		return nil, classification, nil
	}
	snapshot, err := execution.reload(ctx, id)
	if err != nil {
		return nil, "", err
	}
	worker, workerEvents, err := execution.workflowVerificationSource(ctx, snapshot, workflowVerificationWorker)
	if err != nil {
		return nil, "", err
	}
	version, err := workflowObservedRunVersion(worker, workerEvents)
	if err != nil {
		return nil, "", err
	}
	input := workflowVerificationInputs{
		Acceptance: *execution.definition.Verification, WorkerRunID: worker.ID, WorkspaceVersion: version,
	}
	program, programEvents := run, events
	if node.Kind == workflow.StepKindReview {
		program, programEvents, err = execution.workflowVerificationSource(ctx, snapshot, workflowVerificationProgram)
		if err != nil {
			return nil, "", err
		}
	}
	input.ProgramRunID = program.ID
	input.ProgramEvidence, input.ProgramRef, err = execution.workflowProgramEvidence(program, programEvents, input)
	if err != nil {
		return nil, "", err
	}
	if input.ProgramEvidence == nil {
		input.ProgramError = "program verification has no sufficient terminal evidence"
	}
	if node.Kind == workflow.StepKindReview {
		input.ReviewRunID = run.ID
		input.ReviewRef, input.Findings, err = execution.workflowReviewEvidence(run, events, workerEvents, input)
		if err != nil {
			return nil, "", err
		}
		if input.ReviewRef == nil {
			input.ReviewError = "independent review did not produce a valid report from the frozen worktree"
			classification = workflowReviewInvalid
		}
	}
	report, err := buildWorkflowVerificationReport(input)
	if err != nil {
		return nil, "", err
	}
	if node.Kind == workflow.StepKindReview && classification == "" && report.Evaluation.Verdict != contract.VerdictPassed {
		classification = workflowReviewUnclear
	}
	return &report, classification, nil
}

// workflowProgramEvidence 只投影同版本的标准级程序证据。
func (execution workflowExecution) workflowProgramEvidence(run domain.RunProjection, events []domain.Event, input workflowVerificationInputs) (*contract.AcceptanceEvidence, *contract.Ref, error) {
	if run.State != domain.RunCompleted || !run.VerificationSucceeded {
		return nil, nil, nil
	}
	observed, err := workflowObservedRunVersion(run, events)
	if err != nil {
		return nil, nil, err
	}
	if observed != input.WorkspaceVersion {
		return nil, nil, fmt.Errorf("%w: program evidence workspace differs from worker output", ErrVerifierTarget)
	}
	evidence, err := acceptanceEvidenceFromRun(events, run.LastVerificationSeq, execution.stateDir, execution.workspaceHash, input.Acceptance)
	if err != nil || evidence == nil {
		return nil, nil, errors.Join(errors.New("cannot bind workflow program evidence"), err)
	}
	var finished domain.VerificationFinishedPayload
	var ref contract.Ref
	if json.Unmarshal(events[run.LastVerificationSeq-1].Payload, &finished) != nil || json.Unmarshal(finished.EvidenceRef, &ref) != nil {
		return nil, nil, fmt.Errorf("%w: invalid workflow program reference", ErrStateCorrupt)
	}
	ref.TruncatedPreview = ""
	return evidence, &ref, nil
}

// workflowReviewEvidence 只接收独立成功 Run，并要求 Journal 有实际文件读取。
func (execution workflowExecution) workflowReviewEvidence(run domain.RunProjection, events, workerEvents []domain.Event, input workflowVerificationInputs) (*contract.Ref, ValidatedFindings, error) {
	if run.State != domain.RunCompleted || run.ParentRunID != "" || run.SessionID != "" || !workflowReviewReadFiles(events, workerEvents) {
		return nil, ValidatedFindings{}, nil
	}
	version, err := workflowObservedRunVersion(run, events)
	if err != nil || version != input.WorkspaceVersion {
		return nil, ValidatedFindings{}, nil
	}
	findings, err := parseVerifierOutput(verifierFinalText(events), VerifierRequest{TargetRunID: input.WorkerRunID, Acceptance: input.Acceptance})
	if err != nil {
		return nil, ValidatedFindings{}, nil
	}
	verdict, err := ValidateVerifierFindings(VerifierResult{
		VerifierRunID: run.ID, Findings: findings, target: verifierContext{TargetRunID: input.WorkerRunID},
	}, "workflow-independent-review")
	if err != nil {
		return nil, ValidatedFindings{}, err
	}
	store, err := openArtifactStore(execution.stateDir, execution.workspaceHash, execution.configuration.Output)
	if err != nil {
		return nil, ValidatedFindings{}, err
	}
	terminal := events[len(events)-1]
	ref, err := store.Put(contract.MustEncodeRedactedJSON(workflowReviewReceipt{
		RunID: run.ID, Terminal: workflow.RunAnchor{Sequence: terminal.Seq, EventHash: terminal.EventHash},
		SubjectVersion: version, Findings: verdict,
	}), "application/json")
	if err != nil {
		return nil, ValidatedFindings{}, err
	}
	ref.TruncatedPreview = ""
	return &ref, verdict, nil
}

// workflowReviewReadFiles 要求完整读取每个可定位变更的实际版本。无定位
// 或读不全时保留为证据不足。搜索结果不计为完整文件审查。
func workflowReviewReadFiles(reviewEvents, workerEvents []domain.Event) bool {
	type changedFile struct {
		Path            string `json:"planned_path"`
		Digest          string `json:"planned_after"`
		SideEffectClass string `json:"side_effect_class"`
		MutationBarrier bool   `json:"mutation_barrier"`
	}
	changes := make(map[string]string)
	started := make(map[domain.OperationID]changedFile)
	for _, event := range workerEvents {
		if event.OperationID == nil {
			continue
		}
		switch event.Type {
		case domain.EventOperationStarted:
			var file changedFile
			if json.Unmarshal(event.Payload, &file) != nil {
				return false
			}
			if file.SideEffectClass == "workspace" || file.SideEffectClass == "process" && file.MutationBarrier {
				if file.Path == "" || file.Digest == "" {
					return false
				}
				started[*event.OperationID] = file
			}
		case domain.EventOperationSucceeded, domain.EventOperationReconciled:
			file, found := started[*event.OperationID]
			if !found {
				continue
			}
			if event.Type == domain.EventOperationReconciled {
				var result struct {
					Verdict string `json:"verdict"`
				}
				if json.Unmarshal(event.Payload, &result) != nil || result.Verdict != "applied_success" {
					continue
				}
			}
			changes[file.Path] = file.Digest
		}
	}
	if len(changes) == 0 {
		return false
	}
	for _, event := range reviewEvents {
		if event.Type != domain.EventOperationSucceeded {
			continue
		}
		var receipt struct {
			ToolName string              `json:"tool_name"`
			Result   contract.ToolResult `json:"result"`
		}
		var body struct {
			Path   string `json:"path"`
			Offset int64  `json:"offset"`
		}
		if json.Unmarshal(event.Payload, &receipt) == nil && receipt.ToolName == "read_file" &&
			receipt.Result.OK && !receipt.Result.Truncated && receipt.Result.SHA256 != "" &&
			json.Unmarshal(receipt.Result.Data, &body) == nil && body.Offset == 0 &&
			changes[body.Path] == receipt.Result.SHA256 {
			delete(changes, body.Path)
		}
	}
	return len(changes) == 0
}
