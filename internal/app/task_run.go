// [INPUT]: 依赖任务分配存储、普通 Run 创建入口与 Journal 只读投影。
// [OUTPUT]: 提供幂等任务分配、关联工作流、执行历史及证据引用。
// [POS]: 在 Run 的排他锁内先提交任务绑定，再允许写 Run 创建事实。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接任务分配与既有 Run 生命周期。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/task"
	tasksqlite "github.com/fxbin/cairn/internal/capabilities/task/sqlite"
	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

var taskAssignmentID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// TaskRunOptions 指定客户端稳定的分配 ID、任务版本和目标会话。
type TaskRunOptions struct {
	AssignmentID  string
	Revision      uint64
	SessionID     string
	PreviousRunID domain.RunID
	OnEvent       EventSink
}

// TaskAssignmentView 按 Journal 事实展示关联运行，包含验证失败的证据锚点。
type TaskAssignmentView struct {
	task.Assignment
	State        domain.RunState             `json:"run_state,omitempty"`
	Phase        string                      `json:"phase"`
	Verification contract.CompletionEvidence `json:"verification"`
	Evidence     *task.Evidence              `json:"evidence,omitempty"`
	// Acceptance 是从 Run 证据制品校验通过的标准级证据，仅用于完成判定，
	// 不写入任务库也不出现在展示 JSON 中。
	Acceptance *contract.AcceptanceEvidence `json:"-"`
	// Evaluation 是由 Acceptance + Coverage 派生的多级验收结论；同样只用于
	// 完成判定，不持久化、不展示。
	Evaluation *contract.AcceptanceEvaluation `json:"-"`
}

// RunTask 以同一分配 ID 重试请求时返回原绑定。已提交绑定但缺少创建事实时失败关闭。
func (service Service) RunTask(ctx context.Context, scope TaskScope, id TaskID, options TaskRunOptions) (TaskView, error) {
	if !taskAssignmentID.MatchString(options.AssignmentID) || !taskAssignmentID.MatchString(options.SessionID) {
		return TaskView{}, errors.New("task assignment and session require valid identifiers")
	}
	scope, _, err := scope.resolve()
	if err != nil {
		return TaskView{}, err
	}
	view, err := service.GetTask(ctx, scope, id)
	if err != nil {
		return TaskView{}, err
	}
	fingerprint := scope.Config.Fingerprint()
	if found, err := view.existingAssignment(options, fingerprint); found {
		return view, err
	}
	if err := view.canAssign(options.Revision); err != nil {
		return view, err
	}
	acceptance, hasAcceptance, err := view.Definition.Contract()
	if err != nil {
		return TaskView{}, fmt.Errorf("task acceptance contract: %w", err)
	}
	runID := domain.NewRunID()
	runOptions := RunOptions{
		Task: view.Prompt(), Workspace: scope.Workspace, StateDir: scope.StateDir, Config: scope.Config,
		SessionID: options.SessionID, PrevRunID: options.PreviousRunID, RequireSessionTail: options.PreviousRunID != "",
		PreassignedRunID: runID, OnEvent: options.OnEvent,
	}
	if hasAcceptance {
		runOptions.Acceptance = &acceptance
	}
	// 显式开启交接、调用方没有自带前驱运行、且任务已有历史时，把任务的交接摘要作为前情
	// 交给新会话里的 Worker。上下文不可信则不启动这次分配。
	handoffRequest := taskHandoffRequest{target: config.TaskHandoffAssignment, callerProvidesContext: options.PreviousRunID != "", requireHistory: true}
	handoff, err := handoffRequest.text(ctx, service, scope, id)
	if err != nil {
		return TaskView{}, err
	}
	runOptions.PriorContext = handoff
	bound := false
	_, runErr := service.run(ctx, runOptions, func(boot *Boot, draft *domain.EventDraft) error {
		if boot.WorkspaceHash != view.WorkspaceHash {
			return ErrTaskConflict
		}
		assignment := task.Assignment{
			ID: options.AssignmentID, Revision: options.Revision, RunID: runID, SessionID: options.SessionID,
			PreviousRunID: options.PreviousRunID, CreationEventID: domain.NewID("evt"),
			ConfigFingerprint: fingerprint, CreatedAt: time.Now().UTC(),
		}
		if err := bindTaskRun(ctx, scope, id, boot, draft, assignment); err != nil {
			return err
		}
		bound = true
		return nil
	})
	// 调用被取消后，仍尽力返回已持久化的分配身份和对应运行阶段。
	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskResultQueryTimeout)
	defer cancel()
	latest, queryErr := service.GetTask(queryCtx, scope, id)
	if queryErr != nil {
		return view, errors.Join(runErr, queryErr)
	}
	found, bindingErr := latest.existingAssignment(options, fingerprint)
	if found && !bound {
		return latest, bindingErr
	}
	return latest, errors.Join(runErr, bindingErr)
}

const taskResultQueryTimeout = 5 * time.Second

// bindTaskRun 重查任务版本和运行事实，原子提交分配后为创建事件设置固定锚点。
func bindTaskRun(ctx context.Context, scope TaskScope, id TaskID, boot *Boot, draft *domain.EventDraft, assignment task.Assignment) error {
	store, err := tasksqlite.Open(ctx, scope.databasePath(boot.WorkspaceHash), false)
	if err != nil {
		return err
	}
	defer store.Close()
	view, err := readTaskViewFromJournal(ctx, store, boot.Journal, scope.StateDir, boot.WorkspaceHash, id)
	if err != nil {
		return err
	}
	if err := view.canAssign(assignment.Revision); err != nil {
		return err
	}
	var created domain.RunCreatedPayload
	if err := json.Unmarshal(draft.Payload, &created); err != nil {
		return err
	}
	if created.SessionID != assignment.SessionID || created.PrevRunID != assignment.PreviousRunID ||
		created.ConfigFingerprint != assignment.ConfigFingerprint {
		return ErrTaskConflict
	}
	draft.EventID = assignment.CreationEventID
	view.UpdatedAt = assignment.CreatedAt
	return store.Commit(ctx, view.Task, &assignment)
}

// existingAssignment 返回重试时已存在的绑定，拒绝用相同 ID 更改请求参数。
func (view TaskView) existingAssignment(options TaskRunOptions, fingerprint string) (bool, error) {
	for _, assignment := range view.Assignments {
		if assignment.ID != options.AssignmentID {
			continue
		}
		if assignment.Revision != options.Revision || assignment.SessionID != options.SessionID ||
			assignment.PreviousRunID != options.PreviousRunID || assignment.ConfigFingerprint != fingerprint {
			return true, ErrTaskConflict
		}
		if assignment.State == "" {
			return true, ErrTaskAssignmentUnconfirmed
		}
		return true, nil
	}
	return false, nil
}

// canAssign 拒绝过期、暂停、终态或仍有待处理运行的任务。
func (view TaskView) canAssign(revision uint64) error {
	if view.Revision != revision {
		return ErrTaskConflict
	}
	if view.State != task.Active || view.WorkflowBinding != nil {
		return ErrTaskTransition
	}
	return view.requireSettledAssignments()
}

// requireSettledAssignments 核对全部分配，防止新增 Run 或终态掩盖待审批和待对账事实。
func (view TaskView) requireSettledAssignments() error {
	for _, assignment := range view.Assignments {
		if assignment.State == "" {
			return fmt.Errorf("%w: %s", ErrTaskAssignmentUnconfirmed, assignment.RunID)
		}
		if !assignment.settled() {
			return fmt.Errorf("%w: %s (%s)", ErrActiveRun, assignment.RunID, assignment.Phase)
		}
	}
	if view.Workflow != nil {
		for _, step := range view.Workflow.Steps {
			if step.State == string(workflow.StepRunning) || step.State == string(workflow.StepUncertain) {
				return fmt.Errorf("%w: workflow step %s (%s)", ErrActiveRun, step.ID, step.RunPhase)
			}
		}
	}
	return nil
}

// settled 判断 Run 是否已经进入 Journal 终态。
func (assignment TaskAssignmentView) settled() bool {
	switch assignment.State {
	case domain.RunCompleted, domain.RunFailed, domain.RunCancelled:
		return true
	default:
		return false
	}
}

// readTaskView 打开已有 Journal。Journal 尚未创建时保留分配的待确认状态。
func readTaskView(ctx context.Context, store *tasksqlite.Store, stateDir, hash string, id TaskID) (TaskView, error) {
	current, assignments, err := store.Read(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	var journal ports.Journal
	if len(assignments) != 0 {
		readOnly, err := openReadOnlyJournalContext(ctx, stateDir, hash)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return TaskView{}, err
		}
		if err == nil {
			defer readOnly.Close()
			journal = readOnly
		}
	}
	return projectTaskView(ctx, current, assignments, journal, stateDir, hash)
}

// readTaskViewFromJournal 复用持有锁的运行 Journal，避免二次连接跨越启动边界。
func readTaskViewFromJournal(ctx context.Context, store *tasksqlite.Store, journal ports.Journal, stateDir, hash string, id TaskID) (TaskView, error) {
	current, assignments, err := store.Read(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	return projectTaskView(ctx, current, assignments, journal, stateDir, hash)
}

// projectTaskView 保留任务生命周期，并逐条核验分配和已验收的证据锚点。
func projectTaskView(ctx context.Context, current task.Task, assignments []task.Assignment, journal ports.Journal, stateDir, hash string) (TaskView, error) {
	if current.WorkspaceHash != hash {
		return TaskView{}, fmt.Errorf("%w: task workspace mismatch", ErrStateCorrupt)
	}
	view := TaskView{Task: current, Assignments: make([]TaskAssignmentView, 0, len(assignments))}
	for index, assignment := range assignments {
		item, err := projectTaskAssignment(ctx, journal, assignment, stateDir, hash, current.Definition)
		if err != nil {
			return TaskView{}, err
		}
		if !item.settled() && (current.State != task.Active || index != len(assignments)-1) {
			return TaskView{}, fmt.Errorf("%w: task assignment %s must be settled", ErrStateCorrupt, assignment.ID)
		}
		view.Assignments = append(view.Assignments, item)
	}
	related, err := projectTaskWorkflow(ctx, stateDir, hash, current)
	if err != nil {
		return TaskView{}, err
	}
	view.Workflow = related
	switch current.State {
	case task.Active, task.Paused, task.Cancelled:
		if current.Completion == nil {
			return view, nil
		}
	case task.Completed:
		if current.WorkflowBinding != nil {
			evidence, err := taskWorkflowCompletion(view, stateDir)
			if err != nil {
				return TaskView{}, fmt.Errorf("%w: completed task workflow: %w", ErrStateCorrupt, err)
			}
			if current.Completion != nil && len(current.Progress.Remaining) == 0 && evidence.Equal(*current.Completion) {
				return view, nil
			}
			return TaskView{}, fmt.Errorf("%w: completed task workflow evidence mismatch", ErrStateCorrupt)
		}
		if len(view.Assignments) > 0 && current.Completion != nil && len(current.Progress.Remaining) == 0 {
			latest := view.Assignments[len(view.Assignments)-1]
			if latest.Verification.Trusted && latest.Evidence != nil && latest.Evidence.Equal(*current.Completion) {
				return view, nil
			}
		}
	}
	return TaskView{}, fmt.Errorf("%w: invalid task state or completion evidence", ErrStateCorrupt)
}

// projectTaskAssignment 从既有严格投影读取阶段和验证证据，不把运行终态写回任务库。
func projectTaskAssignment(ctx context.Context, journal ports.Journal, assignment task.Assignment, stateDir, hash string, definition task.Definition) (TaskAssignmentView, error) {
	view := TaskAssignmentView{Assignment: assignment, Phase: "binding_unconfirmed"}
	if journal == nil {
		return view, nil
	}
	events, err := journal.Load(ctx, assignment.RunID)
	if err != nil || len(events) == 0 {
		return view, err
	}
	run, err := domain.Replay(events)
	if err != nil {
		return view, fmt.Errorf("%w: task run %s: %w", ErrStateCorrupt, assignment.RunID, err)
	}
	if run.ID != assignment.RunID || run.WorkspaceHash != hash || run.SessionID != assignment.SessionID ||
		run.PrevRunID != assignment.PreviousRunID || run.ConfigFingerprint != assignment.ConfigFingerprint ||
		events[0].EventID != assignment.CreationEventID {
		return view, fmt.Errorf("%w: task run %s binding mismatch", ErrStateCorrupt, assignment.RunID)
	}
	if err := checkRunArtifactReferences(events, stateDir, hash); err != nil {
		return view, fmt.Errorf("%w: task run %s: %w", ErrStateCorrupt, assignment.RunID, err)
	}
	result, err := runResultFromEvents(events, run, stateDir, hash)
	if err != nil {
		return view, fmt.Errorf("%w: task run %s: %w", ErrStateCorrupt, assignment.RunID, err)
	}
	view.Phase, view.State, view.Verification = run.Phase(), run.State, result.Evidence
	if seq := result.Evidence.VerificationSeq; seq > 0 {
		if seq > uint64(len(events)) || events[seq-1].Type != domain.EventVerificationFinished {
			return view, fmt.Errorf("%w: task run %s invalid verification anchor", ErrStateCorrupt, assignment.RunID)
		}
		view.Evidence = &task.Evidence{RunID: assignment.RunID, Sequence: seq, EventHash: events[seq-1].EventHash}
		acceptance, hasAcceptance, err := definition.Contract()
		if err != nil {
			return view, fmt.Errorf("%w: task run %s: %w", ErrStateCorrupt, assignment.RunID, err)
		}
		if hasAcceptance {
			evidence, err := acceptanceEvidenceFromRun(events, seq, stateDir, hash, acceptance)
			if err != nil {
				return view, fmt.Errorf("%w: task run %s: %w", ErrStateCorrupt, assignment.RunID, err)
			}
			if evidence != nil {
				view.Acceptance = evidence
				evaluation, evalErr := evaluationFromEvidence(acceptance, evidence)
				if evalErr != nil {
					return view, fmt.Errorf("%w: task run %s: %w", ErrStateCorrupt, assignment.RunID, evalErr)
				}
				view.Evaluation = &evaluation
				view.Evidence.AcceptanceDigest = acceptance.Digest
				// 证据引用取 verification_finished 中的制品引用；其格式已由
				// acceptanceEvidenceFromRun 校验，这里只做一次解析绑定。
				var finished domain.VerificationFinishedPayload
				if json.Unmarshal(events[seq-1].Payload, &finished) == nil {
					var reference contract.Ref
					if json.Unmarshal(finished.EvidenceRef, &reference) == nil {
						view.Evidence.EvidenceRef = &reference
					}
				}
			}
		}
	}
	return view, nil
}
