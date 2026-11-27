// task_context.go 以只读方式加载任务、工作流链、运行 Journal 与制品库的事实，交给合同层
// 的 ComposeTaskContext 投影任务上下文。每个来源分别打开、分别捕获失败并映射为稳定
// 原因码；只有任务本身读不到才返回错误。
//
// [INPUT]: 任务库、工作流事件链、运行 Journal 与内容寻址制品库，全部只读打开。
// [OUTPUT]: contract.TaskContextV1；任务不存在或任务库不可读时返回错误。
// [POS]: 应用层只读读取器。不取工作区锁或实例锁，不写任何文件，不改任何运行事实；验证报告
// 只取输出制品里的报告项，模型的最终回复不进入投影。
// [PROTOCOL]: Update this header on change, then check AGENTS.md
//
// Author: fxbin
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/fxbin/cairn/internal/capabilities/task"
	tasksqlite "github.com/fxbin/cairn/internal/capabilities/task/sqlite"
	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// ErrTaskNotFound 表示指定标识的任务在当前工作区的任务库里不存在。
var ErrTaskNotFound = errors.New("task not found")

// taskContextReadAttempts 是一次读取内为跟上并发推进的工作流而重读权威投影的次数上限。
const taskContextReadAttempts = 2

// TaskContext 以只读方式投影持久任务的上下文：逐字段带来源、版本锚点与可信状态。
// 完整性失败不返回错误，而是落到对应字段的 blocked；任务不存在或任务库打不开才返回错误。
func (Service) TaskContext(ctx context.Context, scope TaskScope, id TaskID) (contract.TaskContextV1, error) {
	if err := ctx.Err(); err != nil {
		return contract.TaskContextV1{}, err
	}
	scope, hash, err := scope.resolve()
	if err != nil {
		return contract.TaskContextV1{}, err
	}
	store, err := tasksqlite.Open(ctx, scope.databasePath(hash), true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return contract.TaskContextV1{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
		}
		return contract.TaskContextV1{}, err
	}
	defer store.Close()
	current, assignments, err := store.Read(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contract.TaskContextV1{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
		}
		return contract.TaskContextV1{}, err
	}
	if current.WorkspaceHash != hash {
		return contract.TaskContextV1{}, fmt.Errorf("%w: task workspace mismatch", ErrStateCorrupt)
	}
	input := taskContextTaskInput(current)
	taskContextSources(ctx, scope.StateDir, hash, current, assignments, &input)
	return contract.ComposeTaskContext(input)
}

// taskContextBlocked 把来源失败整理成合同层的失败说明。
func taskContextBlocked(reason contract.TaskContextReason) *contract.TaskSourceFailure {
	return &contract.TaskSourceFailure{Status: contract.TaskFactBlocked, Reason: reason}
}

// taskContextTaskInput 整理任务库自身提供的事实。验收合同按三种读取结果分别落已知、
// 尚无与不可信。
func taskContextTaskInput(current task.Task) contract.TaskContextInput {
	remaining := make([]string, 0, len(current.Progress.Remaining))
	remaining = append(remaining, current.Progress.Remaining...)
	input := contract.TaskContextInput{
		SchemaVersion: contract.TaskContextSchemaVersion, TaskID: string(current.ID), WorkspaceHash: current.WorkspaceHash,
		Goal: current.Definition.Goal, State: string(current.State), Revision: current.Revision, Remaining: remaining,
	}
	acceptance, found, err := current.Definition.Contract()
	switch {
	case err != nil:
		input.AcceptanceFailure = taskContextBlocked(contract.TaskReasonAcceptanceContractMismatch)
	case !found:
		input.AcceptanceFailure = &contract.TaskSourceFailure{Status: contract.TaskFactUnknown, Reason: contract.TaskReasonNoStructuredAcceptance}
	default:
		input.Acceptance = &acceptance
	}
	if completion := current.Completion; completion != nil {
		input.Completion = &contract.TaskCompletionInput{
			RunID: string(completion.RunID), Sequence: completion.Sequence, EventHash: completion.EventHash, AcceptanceDigest: completion.AcceptanceDigest,
		}
	}
	return input
}

// taskContextSources 加载任务库之外的三个来源：工作流链、运行 Journal 与制品库，以及
// 应用层的参考视图。没有绑定工作流的任务只读取手工分配的运行；已挂接的工作流按
// “加载权威投影、核对绑定、读取运行、取参考视图”的顺序进行。
func taskContextSources(ctx context.Context, stateDir, hash string, current task.Task, assignments []task.Assignment, input *contract.TaskContextInput) {
	binding := current.WorkflowBinding
	if binding == nil {
		refs := make([]taskContextRunRef, 0, len(assignments))
		for index := range assignments {
			refs = append(refs, taskContextRunRef{runID: assignments[index].RunID, role: contract.TaskRoleAssignment, assignment: &assignments[index]})
		}
		taskContextRuns(ctx, stateDir, hash, refs, input)
		input.Artifacts = taskContextArtifacts(stateDir, hash, taskContextCompletionArtifact(current))
		return
	}
	input.WorkflowBound = true
	if !binding.Attached {
		// 已绑定未挂接时没有可读的实例，也没有手工分配：由工作流派生的事实尚不存在。
		input.WorkflowFailure = &contract.TaskSourceFailure{Status: contract.TaskFactUnknown, Reason: contract.TaskReasonBindingNotAttached}
		return
	}
	if validateTaskWorkflowBinding(current) != nil {
		input.WorkflowFailure = taskContextBlocked(contract.TaskReasonBindingMismatch)
		return
	}
	for attempt := 0; attempt < taskContextReadAttempts; attempt++ {
		taskContextBoundSources(ctx, stateDir, hash, current, input)
		if input.WorkflowFailure != nil {
			return
		}
		// 参考视图是从同一份权威投影算出来的，本身比不出过时。读取期间工作流可能被
		// 推进：读完再加载一次权威投影，序号未变才认为这一轮的事实相互一致。
		if !taskContextAdvanced(ctx, stateDir, hash, current, input.Workflow.LastSeq) {
			return
		}
		if attempt == taskContextReadAttempts-1 {
			// 重读一次后仍在推进：保留最后一轮的权威事实，只把参考视图标为过时，
			// 由调用方决定是否再读一次。
			input.Reference = nil
			input.ReferenceFailure = taskContextBlocked(contract.TaskReasonReferenceViewStale)
			return
		}
		input.Workflow, input.Runs, input.RunsFailure, input.Artifacts = nil, nil, nil, nil
		input.Reference, input.ReferenceFailure = nil, nil
	}
}

// taskContextAdvanced 判断读取期间工作流是否被推进：重新加载权威投影并比较序号。
// 重新加载失败或绑定不再一致同样视为已变化。
func taskContextAdvanced(ctx context.Context, stateDir, hash string, current task.Task, sequence uint64) bool {
	latest, err := loadTaskWorkflowSnapshot(ctx, stateDir, hash, current)
	if err != nil || validateTaskWorkflowSnapshot(current, latest) != nil {
		return true
	}
	return latest.LastSeq != sequence
}

// taskContextBoundSources 读取已挂接工作流的全部来源：权威投影、绑定与定义核对、运行、
// 制品与参考视图。失败直接写入输入。
func taskContextBoundSources(ctx context.Context, stateDir, hash string, current task.Task, input *contract.TaskContextInput) {
	snapshot, err := loadTaskWorkflowSnapshot(ctx, stateDir, hash, current)
	if err != nil {
		input.WorkflowFailure = taskContextBlocked(taskContextLoadFailure(err))
		return
	}
	if validateTaskWorkflowSnapshot(current, snapshot) != nil {
		input.WorkflowFailure = taskContextBlocked(contract.TaskReasonBindingMismatch)
		return
	}
	definition, err := verificationBoundDefinition(stateDir, hash, snapshot)
	if err != nil || validateTaskWorkflowDefinition(current, &definition) != nil {
		input.WorkflowFailure = taskContextBlocked(contract.TaskReasonDefinitionMismatch)
		return
	}
	workflowInput, refs, artifacts := taskContextWorkflow(current, snapshot)
	input.Workflow = workflowInput
	taskContextRuns(ctx, stateDir, hash, refs, input)
	input.Artifacts = taskContextArtifacts(stateDir, hash, artifacts)
	execution := workflowExecution{stateDir: stateDir, workspaceHash: hash, definition: &definition}
	view, err := execution.workflowStatusViewForTask(ctx, snapshot, &current)
	if err != nil {
		// 参考视图无法生成时阶段留空；由它派生的证据与发现失败关闭，运行事实不受影响。
		input.ReferenceFailure = taskContextBlocked(contract.TaskReasonReferenceViewInvalid)
		return
	}
	workflowInput.Phase = view.Phase
	// 工作流链上的“需对账步骤”要等对账动作发生后才会写入；进程在副作用操作进行中被杀
	// 时链上还没有这条记录，待对账只能从运行的未决操作看出来。两个信号任一成立即算数。
	workflowInput.RequiresReconcile = workflowInput.RequiresReconcile || view.PendingReconcile != nil
	input.Reference = taskContextReference(view)
}

// taskContextLoadFailure 把加载任务工作流时的错误映射为稳定原因码，不带错误原文。
func taskContextLoadFailure(err error) contract.TaskContextReason {
	if errors.Is(err, ErrWorkflowNotFound) {
		return contract.TaskReasonWorkflowMissing
	}
	return contract.TaskReasonWorkflowChainInvalid
}

// taskContextWorkflow 从权威投影整理合同层的工作流输入，并列出全部被引用的运行与制品。
// 运行集合取各步骤全部尝试绑定的运行：返工会删除 StepRuns 里的条目，不能依赖它。
func taskContextWorkflow(current task.Task, snapshot *workflow.Snapshot) (*contract.TaskWorkflowInput, []taskContextRunRef, []contract.TaskArtifactInput) {
	binding := current.WorkflowBinding
	input := &contract.TaskWorkflowInput{
		WorkflowID: string(snapshot.WorkflowID), ConfigFingerprint: snapshot.ConfigFingerprint,
		AuthorizationSource: snapshot.AuthorizationSource, PlanSHA256: snapshot.PlanSHA256,
		AuthorizedRevision: binding.AuthorizedRevision, Attached: binding.Attached, LastSeq: snapshot.LastSeq,
		RequiresReconcile: len(snapshot.RequiresReconcile) > 0, Attempts: []contract.TaskAttemptInput{},
	}
	refs := make([]taskContextRunRef, 0)
	artifacts := []contract.TaskArtifactInput{{SHA256: binding.DefinitionArtifact.SHA256, Role: contract.TaskRoleDefinition}}
	seen := make(map[domain.RunID]bool)
	for _, spec := range snapshot.Specs {
		attempts := snapshot.Attempts[spec.ID]
		for index := range attempts {
			attempt := &attempts[index]
			input.Attempts = append(input.Attempts, contract.TaskAttemptInput{
				Step: spec.ID, Number: attempt.Number, RunID: string(attempt.RunID), State: string(attempt.State),
				Terminal: taskContextTerminal(attempt.Terminal), OutputSHA256: attempt.Output.SHA, Decision: attempt.Decision,
			})
			if attempt.RunID != "" && !seen[attempt.RunID] {
				seen[attempt.RunID] = true
				refs = append(refs, taskContextRunRef{runID: attempt.RunID, role: spec.ID, kind: spec.Kind, attempt: attempt, session: snapshot.SessionID})
			}
			if attempt.Output.SHA != "" {
				artifacts = append(artifacts, contract.TaskArtifactInput{SHA256: attempt.Output.SHA, Role: contract.TaskRoleAttemptOutput})
			}
		}
	}
	return input, refs, artifacts
}

// taskContextTerminal 把工作流尝试的终态锚点转成合同层类型；未收束的尝试没有锚点。
func taskContextTerminal(anchor *workflow.RunAnchor) *contract.TaskRunAnchor {
	if anchor == nil {
		return nil
	}
	return &contract.TaskRunAnchor{Sequence: anchor.Sequence, EventHash: anchor.EventHash}
}

// taskContextCompletionArtifact 返回完成记录引用的证据制品；没有则返回空。
func taskContextCompletionArtifact(current task.Task) []contract.TaskArtifactInput {
	if current.Completion == nil || current.Completion.EvidenceRef == nil {
		return nil
	}
	return []contract.TaskArtifactInput{{SHA256: current.Completion.EvidenceRef.SHA256, Role: contract.TaskRoleCompletionEvidence}}
}

// taskContextArtifacts 核对每个被引用的制品在内容寻址库里在位且内容与摘要一致。
func taskContextArtifacts(stateDir, hash string, artifacts []contract.TaskArtifactInput) []contract.TaskArtifactInput {
	root := manifestArtifactRoot(stateDir, hash)
	checked := make([]contract.TaskArtifactInput, 0, len(artifacts))
	seen := make(map[contract.TaskArtifactInput]bool, len(artifacts))
	for _, artifact := range artifacts {
		if seen[artifact] || artifact.SHA256 == "" {
			continue
		}
		seen[artifact] = true
		switch err := checkArtifactReference(root, contract.Ref{SHA256: artifact.SHA256}); {
		case err == nil:
		case errors.Is(err, os.ErrNotExist):
			artifact.Missing = true
		default:
			artifact.Tampered = true
		}
		checked = append(checked, artifact)
	}
	return checked
}

// taskContextRunRef 是一条被任务引用的运行及其核对所需的绑定信息。
type taskContextRunRef struct {
	runID      domain.RunID
	role       string
	kind       string
	attempt    *workflow.Attempt
	assignment *task.Assignment
	session    string
}

// taskContextRuns 只读打开 Journal，按任务引用的运行逐个加载并回放验链。不用整库
// 检查：同一状态目录下无关运行的损坏不应连累这个任务的上下文。正在执行的尝试可能
// 还没落下创建事件，这是可观察的创建窗口，按尚未出现处理。
func taskContextRuns(ctx context.Context, stateDir, hash string, refs []taskContextRunRef, input *contract.TaskContextInput) {
	if len(refs) == 0 {
		input.Runs = []contract.TaskRunInput{}
		return
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, hash)
	if err != nil {
		input.RunsFailure = taskContextBlocked(contract.TaskReasonJournalUnavailable)
		return
	}
	defer journal.Close()
	runs := make([]contract.TaskRunInput, 0, len(refs))
	for _, ref := range refs {
		events, err := journal.Load(ctx, ref.runID)
		if err != nil {
			input.RunsFailure = taskContextBlocked(contract.TaskReasonJournalChainInvalid)
			return
		}
		if len(events) == 0 {
			if ref.attempt != nil && ref.attempt.State == workflow.StepRunning && ref.attempt.Terminal == nil {
				continue
			}
			input.RunsFailure = taskContextBlocked(contract.TaskReasonRunMissing)
			return
		}
		run, err := domain.Replay(events)
		if err != nil {
			input.RunsFailure = taskContextBlocked(contract.TaskReasonJournalChainInvalid)
			return
		}
		if ref.attempt != nil {
			// 绑定不一致与终态锚点不符是两类问题，先单独核对绑定以给出准确的原因码；
			// 终态锚点交给既有的读取函数，与命令运行路径同一口径。
			if taskContextAttemptBindingMismatch(run, events, ref, hash) {
				input.RunsFailure = taskContextBlocked(contract.TaskReasonBindingMismatch)
				return
			}
			if _, _, err := readWorkflowAttempt(ctx, journal, hash, workflowAttemptSession(ref.session, ref.kind), *ref.attempt); err != nil {
				input.RunsFailure = taskContextBlocked(taskContextRunFailure(err))
				return
			}
		} else if taskContextAssignmentMismatch(run, events, ref.assignment, hash) {
			input.RunsFailure = taskContextBlocked(contract.TaskReasonBindingMismatch)
			return
		}
		runs = append(runs, taskRunContextInput(ref.role, run, events))
	}
	sort.Slice(runs, func(left, right int) bool { return runs[left].RunID < runs[right].RunID })
	input.Runs = runs
}

// taskContextAttemptBindingMismatch 核对工作流尝试绑定的运行与尝试记录的身份、来源会话、
// 配置指纹和创建事件是否一致。
func taskContextAttemptBindingMismatch(run domain.RunProjection, events []domain.Event, ref taskContextRunRef, hash string) bool {
	attempt := ref.attempt
	return run.ID != attempt.RunID || run.WorkspaceHash != hash || run.SessionID != workflowAttemptSession(ref.session, ref.kind) ||
		run.ConfigFingerprint != attempt.ConfigFingerprint || len(events) == 0 || events[0].EventID != attempt.CreationEventID
}

// taskContextRunFailure 把核对工作流尝试时的错误映射为稳定原因码：尝试声明的终态锚点
// 与 Journal 链头不符，或运行与尝试的绑定不一致。
func taskContextRunFailure(err error) contract.TaskContextReason {
	if errors.Is(err, ErrWorkflowAttemptUnconfirmed) {
		return contract.TaskReasonRunMissing
	}
	return contract.TaskReasonTerminalAnchorMismatch
}

// taskContextAssignmentMismatch 核对手工分配的运行与分配记录的身份、会话、配置指纹和
// 创建事件是否一致。
func taskContextAssignmentMismatch(run domain.RunProjection, events []domain.Event, assignment *task.Assignment, hash string) bool {
	return run.ID != assignment.RunID || run.WorkspaceHash != hash || run.SessionID != assignment.SessionID ||
		run.PrevRunID != assignment.PreviousRunID || run.ConfigFingerprint != assignment.ConfigFingerprint ||
		len(events) == 0 || events[0].EventID != assignment.CreationEventID
}

// taskRunContextInput 从通过哈希链校验的事件里提取单个运行的事实摘要：身份、来源会话、
// 状态、链头、审批决定与未了结的操作。模型输出与工具结果正文不进入输入。
func taskRunContextInput(role string, run domain.RunProjection, events []domain.Event) contract.TaskRunInput {
	input := contract.TaskRunInput{
		RunID: string(run.ID), Role: role, SessionID: run.SessionID, PrevRunID: string(run.PrevRunID),
		State: string(run.State), Phase: run.Phase(), Terminal: run.Terminal(),
		PendingApproval: run.PendingApproval != nil, ApprovalDecisions: []contract.TaskDecision{},
	}
	if len(events) > 0 {
		input.LastSeq = events[len(events)-1].Seq
		input.HeadHash = events[len(events)-1].EventHash
	}
	if run.PendingOperation != nil {
		input.PendingOperationID = string(run.PendingOperation.ID)
	}
	sideEffects := make(map[domain.OperationID]bool)
	for _, event := range events {
		switch event.Type {
		case domain.EventOperationStarted:
			if taskContextSideEffect(event.Payload) {
				input.StartedOperations++
				if event.OperationID != nil {
					sideEffects[*event.OperationID] = true
				}
			}
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
			if event.OperationID != nil && sideEffects[*event.OperationID] {
				input.SettledOperations++
			}
		case domain.EventApprovalGranted, domain.EventApprovalDenied:
			var payload struct {
				Fingerprint string `json:"fingerprint"`
			}
			_ = json.Unmarshal(event.Payload, &payload)
			decision := contract.TaskDecision{Kind: string(event.Type), RunID: string(run.ID), Fingerprint: payload.Fingerprint, EventSeq: event.Seq, EventHash: event.EventHash}
			if event.OperationID != nil {
				decision.OperationID = string(*event.OperationID)
			}
			input.ApprovalDecisions = append(input.ApprovalDecisions, decision)
		case domain.EventVerificationFinished:
			input.VerificationSeq, input.VerificationEventHash = event.Seq, event.EventHash
		}
	}
	return input
}

// taskContextSideEffect 判断一条操作启动事件是否声明了副作用；只读操作不计入。
func taskContextSideEffect(payload json.RawMessage) bool {
	var started struct {
		SideEffectClass string `json:"side_effect_class"`
	}
	_ = json.Unmarshal(payload, &started)
	return started.SideEffectClass != "" && started.SideEffectClass != "none"
}

// taskContextReference 把应用层参考视图里的验证报告整理成合同层输入。没有报告时返回
// 空，投影据此落 unknown。
func taskContextReference(view WorkflowStatusView) *contract.TaskReferenceInput {
	report := view.Verification
	if report == nil {
		return nil
	}
	groups := []struct {
		status   string
		findings []contract.Finding
	}{{"confirmed", report.Findings.Confirmed}, {"inconclusive", report.Findings.Inconclusive}, {"rejected", report.Findings.Rejected}}
	findings := make([]contract.TaskFindingFact, 0)
	for _, group := range groups {
		for _, finding := range group.findings {
			digests := make([]string, 0, len(finding.EvidenceRefs))
			for _, reference := range finding.EvidenceRefs {
				digests = append(digests, reference.SHA256)
			}
			findings = append(findings, contract.TaskFindingFact{
				ID: finding.ID, TargetID: finding.TargetID, CriterionID: finding.CriterionID, Category: finding.Category,
				Severity: string(finding.Severity), Status: group.status, Blocking: finding.IsBlocking(), EvidenceSHA256s: digests,
			})
		}
	}
	return &contract.TaskReferenceInput{Sequence: view.Sequence, Report: &contract.TaskReportInput{
		WorkerRunID: string(report.WorkerRunID), ProgramRunID: string(report.ProgramRunID), ReviewRunID: string(report.ReviewRunID),
		Verdict: string(report.Evaluation.Verdict), ScopeDigest: report.Scope.Digest, CoverageDigest: report.Coverage.Digest,
		Criteria: report.Evaluation.Criteria, Findings: findings,
	}}
}
