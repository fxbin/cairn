// [INPUT]: 依赖任务版本存储、冻结验收和版本化工作流。
// [OUTPUT]: 提供自主任务启动、冷恢复与验收锚点。
// [POS]: 任务先记录绑定。工作流和 Run Journal 分别保存推进与运行事实。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接持久任务与自主验证工作流。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/task"
	tasksqlite "github.com/fxbin/cairn/internal/capabilities/task/sqlite"
	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

const (
	// MaxTaskWorkflowReworkAttempts 限制一次用户授权可追加的 Worker 尝试次数。
	MaxTaskWorkflowReworkAttempts = 3
	taskWorkflowDefinitionPrefix  = "wfl_task_"
	taskWorkerStepID              = "worker"
	taskProgramStepID             = "program"
	taskReviewStepID              = "review"
)

// TaskWorkflowOptions 绑定用户看到的任务版本及明确的返工额度。零表示不返工。
type TaskWorkflowOptions struct {
	Revision       uint64
	ReworkAttempts int
	OnEvent        EventSink
}

// StartTaskWorkflow 在同一任务上持久绑定唯一实例，再使用原工作流入口推进。
func (service Service) StartTaskWorkflow(ctx context.Context, scope TaskScope, id TaskID, options TaskWorkflowOptions) (TaskView, error) {
	if options.Revision == 0 || options.ReworkAttempts < 0 || options.ReworkAttempts > MaxTaskWorkflowReworkAttempts {
		return TaskView{}, fmt.Errorf("task workflow requires a revision and a rework limit from 0 to %d", MaxTaskWorkflowReworkAttempts)
	}
	scope, hash, err := scope.resolve()
	if err != nil {
		return TaskView{}, err
	}
	view, attached, err := service.attachTaskWorkflow(ctx, scope, hash, id, &options)
	if err != nil || !attached {
		return view, err
	}
	return service.driveTaskWorkflow(ctx, scope, view, options.OnEvent)
}

// ContinueTaskWorkflow 重放已授权实例。准备阶段只允许补建尚未绑定的同一身份。
func (service Service) ContinueTaskWorkflow(ctx context.Context, scope TaskScope, id TaskID, onEvent EventSink) (TaskView, error) {
	scope, hash, err := scope.resolve()
	if err != nil {
		return TaskView{}, err
	}
	view, _, err := service.attachTaskWorkflow(ctx, scope, hash, id, nil)
	if err != nil {
		return view, err
	}
	return service.driveTaskWorkflow(ctx, scope, view, onEvent)
}

// attachTaskWorkflow 用工作区锁串行化单独分配、准备与确认。退出锁后才启动 Run。
func (service Service) attachTaskWorkflow(ctx context.Context, scope TaskScope, hash string, id TaskID, options *TaskWorkflowOptions) (TaskView, bool, error) {
	lease, err := acquireWorkspaceLock(scope.StateDir, hash)
	if err != nil {
		return TaskView{}, false, err
	}
	defer lease.Release()
	store, err := tasksqlite.Open(ctx, scope.databasePath(hash), false)
	if err != nil {
		return TaskView{}, false, err
	}
	defer store.Close()
	view, err := readTaskView(ctx, store, scope.StateDir, hash, id)
	if err != nil {
		return TaskView{}, false, err
	}
	if view.WorkflowBinding != nil {
		if options != nil && (view.WorkflowBinding.AuthorizedRevision != options.Revision ||
			view.WorkflowBinding.ReworkAttempts != options.ReworkAttempts) {
			return view, false, ErrTaskConflict
		}
		if view.WorkflowBinding.ConfigFingerprint != scope.Config.Fingerprint() {
			return view, false, ErrConfigDrift
		}
		if view.WorkflowBinding.Attached && options != nil {
			return view, false, nil
		}
	}
	if view.State != task.Active {
		return view, false, ErrTaskTransition
	}
	if view.WorkflowBinding == nil {
		if options == nil {
			return view, false, ErrTaskTransition
		}
		if err := view.canAssign(options.Revision); err != nil {
			return view, false, err
		}
		if err := validateHookConfiguration(scope.Config); err != nil {
			return view, false, err
		}
		binding, err := freezeTaskWorkflow(scope, view.Task, options.ReworkAttempts)
		if err != nil {
			return view, false, err
		}
		if err := view.PrepareWorkflow(binding); err != nil {
			return view, false, err
		}
		if err := commitTaskWorkflowBinding(ctx, store, &view.Task); err != nil {
			return view, false, err
		}
	}
	if view.WorkflowBinding.Attached {
		return view, false, nil
	}
	if err := ensureTaskWorkflowInstance(ctx, scope, view.Task); err != nil {
		return view, false, err
	}
	if err := view.AttachWorkflow(); err != nil {
		return view, false, err
	}
	if err := commitTaskWorkflowBinding(ctx, store, &view.Task); err != nil {
		return view, false, err
	}
	view, err = readTaskView(ctx, store, scope.StateDir, hash, id)
	return view, true, err
}

// commitTaskWorkflowBinding 按既有任务 CAS 提交绑定，成功后推进返回版本。
func commitTaskWorkflowBinding(ctx context.Context, store *tasksqlite.Store, current *task.Task) error {
	current.UpdatedAt = time.Now().UTC()
	if err := store.Commit(ctx, *current, nil); err != nil {
		return err
	}
	current.Revision++
	return nil
}

// freezeTaskWorkflow 冻结用户任务、只有三个阶段的验证定义与明确授权的返工额度。
func freezeTaskWorkflow(scope TaskScope, current task.Task, rework int) (task.WorkflowBinding, error) {
	acceptance, found, err := current.Definition.Contract()
	if err != nil {
		return task.WorkflowBinding{}, err
	}
	if !found {
		return task.WorkflowBinding{}, errors.New("task workflow requires a structured acceptance contract")
	}
	if err := requireIsolatedVerificationState(scope.Workspace, scope.StateDir); err != nil {
		return task.WorkflowBinding{}, err
	}
	prompt := current.Prompt() + "\n\n用 apply_patch 修改文件。构建和测试由程序验证节点执行。独立审查将逐一读取已定位的文件版本。"
	nodes := []WorkflowNode{
		{ID: taskWorkerStepID, Kind: workflow.StepKindExecute, Prompt: prompt, MaxAttempts: 1 + rework},
		{ID: taskProgramStepID, Kind: workflow.StepKindVerify, MaxAttempts: 1 + rework},
		{ID: taskReviewStepID, Kind: workflow.StepKindReview, MaxAttempts: 1 + rework},
	}
	if rework > 0 {
		nodes[1].RetryFrom = taskWorkerStepID
	}
	definition, err := checkedWorkflowDraft(current.WorkspaceHash, WorkflowDefinitionDraft{
		ID: taskWorkflowDefinitionID(current.ID), Name: "Task workflow", Nodes: nodes, Verification: &acceptance,
	})
	if err != nil {
		return task.WorkflowBinding{}, err
	}
	if err := validateWorkflowRuntime(definition, scope.Config); err != nil {
		return task.WorkflowBinding{}, err
	}
	content, err := json.Marshal(definition)
	if err != nil {
		return task.WorkflowBinding{}, err
	}
	artifacts, err := openArtifactStore(scope.StateDir, current.WorkspaceHash, scope.Config.Output)
	if err != nil {
		return task.WorkflowBinding{}, err
	}
	ref, err := artifacts.Put(content, "application/json")
	if err != nil {
		return task.WorkflowBinding{}, err
	}
	ref.TruncatedPreview = ""
	binding := task.WorkflowBinding{
		WorkflowID: string(workflow.NewID()), DefinitionID: string(definition.ID), DefinitionRevision: definition.Revision,
		DefinitionArtifact: ref, ConfigFingerprint: scope.Config.Fingerprint(), AuthorizedRevision: current.Revision,
		ReworkAttempts: rework,
	}
	if _, err := readWorkflowDefinitionArtifact(scope.StateDir, current.WorkspaceHash, taskWorkflowDefinitionRef(binding)); err != nil {
		return task.WorkflowBinding{}, err
	}
	return binding, nil
}

// ensureTaskWorkflowInstance 只允许准备阶段创建预分配的实例。重试核对同一创建锚点。
func ensureTaskWorkflowInstance(ctx context.Context, scope TaskScope, current task.Task) error {
	binding := current.WorkflowBinding
	if binding == nil || binding.Attached || current.State != task.Active {
		return ErrTaskTransition
	}
	_, err := readTaskWorkflowSnapshot(ctx, scope.StateDir, current.WorkspaceHash, current)
	if !errors.Is(err, ErrWorkflowNotFound) {
		return err
	}
	ref := taskWorkflowDefinitionRef(*binding)
	definition, err := readWorkflowDefinitionArtifact(scope.StateDir, current.WorkspaceHash, ref)
	if err != nil {
		return err
	}
	if err := validateTaskWorkflowDefinition(current, &definition); err != nil {
		return err
	}
	if err := validateHookConfiguration(scope.Config); err != nil {
		return err
	}
	if err := requireIsolatedVerificationState(scope.Workspace, scope.StateDir); err != nil {
		return err
	}
	if err := validateWorkflowRuntime(definition, scope.Config); err != nil {
		return err
	}
	_, err = createWorkflowInstanceAt(ctx, WorkflowScope(scope), current.WorkspaceHash, definition, ref, workflow.ID(binding.WorkflowID), string(current.ID))
	return err
}

// readTaskWorkflowSnapshot 核对同一工作区的任务、实例、定义和配置锚点。
func readTaskWorkflowSnapshot(ctx context.Context, stateDir, hash string, current task.Task) (*workflow.Snapshot, error) {
	if err := validateTaskWorkflowBinding(current); err != nil {
		return nil, err
	}
	store, err := openWorkflowStoreReadOnly(stateDir, hash)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	execution := workflowExecution{stateDir: stateDir, workspaceHash: hash, store: store}
	snapshot, err := execution.reload(ctx, workflow.ID(current.WorkflowBinding.WorkflowID))
	if err != nil {
		return nil, err
	}
	if err := validateTaskWorkflowSnapshot(current, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// validateTaskWorkflowBinding 核对已保存的绑定形状与版本推进。
func validateTaskWorkflowBinding(current task.Task) error {
	binding := current.WorkflowBinding
	if binding == nil || !WorkflowDefinitionID(binding.WorkflowID).Valid() ||
		WorkflowDefinitionID(binding.DefinitionID) != taskWorkflowDefinitionID(current.ID) ||
		binding.AuthorizedRevision == 0 || binding.AuthorizedRevision >= current.Revision ||
		binding.Attached && current.Revision-binding.AuthorizedRevision < 2 ||
		binding.ReworkAttempts < 0 || binding.ReworkAttempts > MaxTaskWorkflowReworkAttempts ||
		binding.ConfigFingerprint == "" || current.Definition.AcceptanceDigest == "" {
		return fmt.Errorf("%w: invalid task workflow binding", ErrStateCorrupt)
	}
	if err := taskWorkflowDefinitionRef(*binding).Validate(); err != nil {
		return fmt.Errorf("%w: invalid task workflow definition reference: %v", ErrStateCorrupt, err)
	}
	return nil
}

// validateTaskWorkflowSnapshot 拒绝跨任务、跨实例、跨定义的绑定及未确认推进。
func validateTaskWorkflowSnapshot(current task.Task, snapshot *workflow.Snapshot) error {
	binding := current.WorkflowBinding
	expected := taskWorkflowDefinitionRef(*binding)
	if snapshot.TaskID != string(current.ID) || snapshot.WorkspaceHash != current.WorkspaceHash ||
		string(snapshot.WorkflowID) != binding.WorkflowID || snapshot.AuthorizationSource != workflow.TaskWorkflowAuthorization ||
		snapshot.Definition == nil || *snapshot.Definition != expected || snapshot.ConfigFingerprint != binding.ConfigFingerprint ||
		!binding.Attached && snapshot.LastSeq != 1 {
		return fmt.Errorf("%w: task workflow creation does not match task binding", ErrStateCorrupt)
	}
	return nil
}

// validateTaskWorkflowDefinition 将工作流的验收合同和有界返工与任务授权比较。
func validateTaskWorkflowDefinition(current task.Task, definition *WorkflowDefinition) error {
	acceptance, found, err := current.Definition.Contract()
	if err != nil {
		return fmt.Errorf("%w: task workflow acceptance contract: %v", ErrStateCorrupt, err)
	}
	if !found || definition == nil || definition.Verification == nil || definition.Verification.Digest != acceptance.Digest ||
		len(definition.Nodes) != 3 {
		return fmt.Errorf("%w: task workflow acceptance contract mismatch", ErrStateCorrupt)
	}
	binding := current.WorkflowBinding
	for _, node := range definition.Nodes {
		if node.MaxAttempts != 1+binding.ReworkAttempts {
			return fmt.Errorf("%w: task workflow rework limit mismatch", ErrStateCorrupt)
		}
	}
	expectedRetry := ""
	if binding.ReworkAttempts > 0 {
		expectedRetry = taskWorkerStepID
	}
	worker, program, review := definition.Nodes[0], definition.Nodes[1], definition.Nodes[2]
	if worker.ID != taskWorkerStepID || program.ID != taskProgramStepID || review.ID != taskReviewStepID ||
		worker.RetryFrom != "" || program.RetryFrom != expectedRetry || review.RetryFrom != "" {
		return fmt.Errorf("%w: task workflow rework target mismatch", ErrStateCorrupt)
	}
	return nil
}

// taskWorkflowDefinitionID 将私有工作流定义固定到唯一任务。
func taskWorkflowDefinitionID(id TaskID) WorkflowDefinitionID {
	return WorkflowDefinitionID(taskWorkflowDefinitionPrefix + string(id))
}

// taskWorkflowDefinitionRef 将任务保存的制品锚点转为工作流定义引用。
func taskWorkflowDefinitionRef(binding task.WorkflowBinding) WorkflowDefinitionRef {
	return WorkflowDefinitionRef{
		ID: WorkflowDefinitionID(binding.DefinitionID), Revision: binding.DefinitionRevision, Artifact: binding.DefinitionArtifact,
	}
}

// projectTaskWorkflow 只读重放现有工作流。确认后找不到实例一律视为状态损坏。
func projectTaskWorkflow(ctx context.Context, stateDir, hash string, current task.Task) (*WorkflowStatusView, error) {
	if current.WorkflowBinding == nil {
		return nil, nil
	}
	snapshot, err := readTaskWorkflowSnapshot(ctx, stateDir, hash, current)
	if errors.Is(err, ErrWorkflowNotFound) {
		if current.WorkflowBinding.Attached {
			return nil, fmt.Errorf("%w: attached task workflow is missing", ErrStateCorrupt)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	definition, err := verificationBoundDefinition(stateDir, hash, snapshot)
	if err != nil {
		return nil, err
	}
	if err := validateTaskWorkflowDefinition(current, &definition); err != nil {
		return nil, err
	}
	execution := workflowExecution{stateDir: stateDir, workspaceHash: hash, definition: &definition}
	view, err := execution.workflowStatusView(ctx, snapshot)
	return &view, err
}

// driveTaskWorkflow 复用工作流的授权链、审批、对账与步骤推进。
func (service Service) driveTaskWorkflow(ctx context.Context, scope TaskScope, view TaskView, onEvent EventSink) (TaskView, error) {
	if view.Workflow == nil || view.WorkflowBinding == nil || !view.WorkflowBinding.Attached {
		return view, ErrTaskTransition
	}
	if view.Workflow.Finished {
		return view, nil
	}
	_, runErr := service.WorkflowResume(ctx, WorkflowResumeOptions{
		WorkflowID: workflow.ID(view.WorkflowBinding.WorkflowID), Workspace: scope.Workspace, StateDir: scope.StateDir,
		Config: scope.Config, OnEvent: onEvent,
	})
	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskResultQueryTimeout)
	defer cancel()
	latest, queryErr := service.GetTask(queryCtx, scope, view.ID)
	if queryErr != nil {
		return view, errors.Join(runErr, queryErr)
	}
	if errors.Is(runErr, ErrWorkflowFinished) && latest.Workflow != nil && latest.Workflow.Finished {
		runErr = nil
	}
	return latest, runErr
}

// requireActiveTaskWorkflow 在推进前交叉核对已有 Task 归属与用户授权。
func (execution workflowExecution) requireActiveTaskWorkflow(ctx context.Context, snapshot *workflow.Snapshot) error {
	if snapshot.TaskID == "" {
		return nil
	}
	store, err := tasksqlite.Open(ctx, (TaskScope{StateDir: execution.stateDir}).databasePath(execution.workspaceHash), true)
	if err != nil {
		return fmt.Errorf("%w: read owning task: %w", ErrStateCorrupt, err)
	}
	defer store.Close()
	current, _, err := store.Read(ctx, TaskID(snapshot.TaskID))
	if err != nil {
		return fmt.Errorf("%w: read owning task: %w", ErrStateCorrupt, err)
	}
	if err := validateTaskWorkflowBinding(current); err != nil {
		return err
	}
	if err := validateTaskWorkflowSnapshot(current, snapshot); err != nil {
		return err
	}
	if err := validateTaskWorkflowDefinition(current, execution.definition); err != nil {
		return err
	}
	if current.State != task.Active || !current.WorkflowBinding.Attached {
		return ErrTaskTransition
	}
	return nil
}

// taskWorkflowCompletion 固定通过报告的审查终态和输出制品。
func taskWorkflowCompletion(view TaskView, stateDir string) (*task.Evidence, error) {
	if view.WorkflowBinding == nil || !view.WorkflowBinding.Attached || view.Workflow == nil ||
		!view.Workflow.Finished || view.Workflow.FinishState != workflow.RunStateCompleted {
		return nil, ErrTaskUnverified
	}
	report := view.Workflow.Verification
	if report == nil || report.Evaluation.Verdict != contract.VerdictPassed ||
		report.Evaluation.AcceptanceDigest != view.Definition.AcceptanceDigest {
		return nil, ErrTaskUnverified
	}
	for _, step := range view.Workflow.Steps {
		if step.ID != taskReviewStepID || step.RunID != report.ReviewRunID || step.State != string(workflow.StepSucceeded) || len(step.Attempts) == 0 {
			continue
		}
		attempt := step.Attempts[len(step.Attempts)-1]
		if attempt.RunID != report.ReviewRunID || attempt.Terminal == nil || attempt.Terminal.Sequence == 0 ||
			attempt.Terminal.EventHash == "" || attempt.Output.Ref == "" || attempt.Output.Ref != attempt.Output.SHA {
			return nil, ErrTaskUnverified
		}
		ref := contract.Ref{SHA256: attempt.Output.Ref, MediaType: "application/json"}
		content, err := readArtifactContent(manifestArtifactRoot(stateDir, view.WorkspaceHash), ref)
		if err != nil {
			return nil, fmt.Errorf("%w: read task workflow completion artifact: %w", ErrStateCorrupt, err)
		}
		ref.Bytes = int64(len(content))
		return &task.Evidence{
			WorkflowID: view.WorkflowBinding.WorkflowID, RunID: attempt.RunID, Sequence: attempt.Terminal.Sequence,
			EventHash: attempt.Terminal.EventHash, AcceptanceDigest: report.Evaluation.AcceptanceDigest, EvidenceRef: &ref,
		}, nil
	}
	return nil, ErrTaskUnverified
}

// completeTaskFromWorkflow 由显式验收提交。锁内重新观察当前版本并清除已通过标准。
func completeTaskFromWorkflow(scope TaskScope, view *TaskView, runID domain.RunID) error {
	evidence, err := taskWorkflowCompletion(*view, scope.StateDir)
	if err != nil {
		return err
	}
	if runID != evidence.RunID {
		return ErrTaskUnverified
	}
	if scope.Config.Fingerprint() != view.WorkflowBinding.ConfigFingerprint {
		return ErrConfigDrift
	}
	if err := requireCurrentWorkflowVerification(scope.Workspace, scope.Config, view.Workflow.Verification); err != nil {
		return err
	}
	view.Progress.Remaining = remainingAfterWorkflowAcceptance(view.Definition, view.Progress.Remaining, view.Workflow.Verification.Evaluation)
	return view.Transition(task.Completed, evidence)
}

// remainingAfterWorkflowAcceptance 保留用户新增的待办和未被报告证实的标准。
func remainingAfterWorkflowAcceptance(definition task.Definition, remaining []string, evaluation contract.AcceptanceEvaluation) []string {
	verdicts := make(map[string]contract.EvaluationVerdict, len(evaluation.Criteria))
	for _, criterion := range evaluation.Criteria {
		verdicts[criterion.CriterionID] = criterion.Verdict
	}
	passed := make(map[string]bool, len(definition.Criteria))
	for _, criterion := range definition.Criteria {
		prior, found := passed[criterion.Statement]
		passed[criterion.Statement] = (!found || prior) && verdicts[criterion.ID] == contract.VerdictPassed
	}
	return slices.DeleteFunc(slices.Clone(remaining), func(item string) bool { return passed[item] })
}
