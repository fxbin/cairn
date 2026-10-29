// workflow_exec.go 提供工作流执行链的显式执行、恢复与人工门用例（ADR-0021
// 片 2）：固定顺序计划 [execute, verify, accept]，执行与验证步骤分别经普通
// run 与零调用脚本化 run 驱动并在步骤事件中绑定 run_id，恢复唯一性来自链重
// 放与 run 投影回填。执行器自身不装配 Boot、不持有工作区排他锁：锁唯一归
// 属步骤 run 的执行路径，工作流状态只经独立 workflow.db 以链头 CAS 追加。
// Author: fxbin
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	workflowsqlite "github.com/fxbin/cairn/internal/adapters/workflow/sqlite"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/workflow"
)

// 工作流固定计划的步骤标识：执行、冻结验证与人工审批门。
const (
	workflowStepExecuteID = "execute"
	workflowStepVerifyID  = "verify"
	workflowStepAcceptID  = "accept"
)

// 工作流事件的显式授权来源常量；只进事件负载用于审计，不授予任何权限。
const (
	workflowAuthorizationRun    = "cli:workflow-run"
	workflowAuthorizationAccept = "cli:workflow-accept"
)

// workflowCommitTimeout 是工作流链写入提交上下文的时限：取消事实等必须落链
// 的写入在脱离调用方取消后仍有明确的时间边界，不无限等待。
const workflowCommitTimeout = 5 * time.Second

// workflowVerifyPlanKind 与版本标识验证步骤脚本化 run 的 task_ref 制品；
// Resume 与 Reconcile 以和回退计划相同的方式识别脚本化续跑。
const (
	workflowVerifyPlanKind    = "workflow-verify"
	workflowVerifyPlanVersion = 1
)

// workflowVerifyPlanArtifact 是验证步骤 run 创建事件 task_ref 指向的计划
// 制品：它只承载工作流与步骤身份，验证参数始终由冻结配置重新推导。
type workflowVerifyPlanArtifact struct {
	Version    int    `json:"version"`
	Kind       string `json:"kind"`
	WorkflowID string `json:"workflow_id"`
	StepID     string `json:"step_id"`
}

// ErrWorkflowFinished 表示目标工作流已经进入终态，恢复与人工门入口拒绝。
var ErrWorkflowFinished = errors.New("workflow already finished")

// WorkflowRunOptions 描述创建并推进一个工作流所需的输入。
type WorkflowRunOptions struct {
	Task      string
	Workspace string
	StateDir  string
	Config    config.Config
	OnEvent   EventSink
}

// WorkflowResumeOptions 描述恢复既有工作流所需的输入；不含审批决定，审批
// 与对账一律先在 run 级 resume 入口解决。
type WorkflowResumeOptions struct {
	WorkflowID workflow.ID
	Workspace  string
	StateDir   string
	Config     config.Config
	OnEvent    EventSink
}

// WorkflowAcceptOptions 描述通过人工审批门所需的输入。
type WorkflowAcceptOptions struct {
	WorkflowID workflow.ID
	Workspace  string
	StateDir   string
	Config     config.Config
	OnEvent    EventSink
}

// WorkflowOutcome 是执行类工作流命令的统一返回：链重放状态视图与下一步
// 人工动作提示；提示只是指引，不代表已发生授权。
type WorkflowOutcome struct {
	View       WorkflowStatusView `json:"view"`
	NextAction string             `json:"next_action,omitempty"`
}

// workflowExecution 携带一次推进循环的全部固定输入；store 由调用方打开并
// 在用例返回后关闭，本结构只做链追加与只读重放。
type workflowExecution struct {
	service       Service
	store         *workflowsqlite.Store
	workspacePath string
	workspaceHash string
	stateDir      string
	configuration config.Config
	task          string
	taskIO        workflow.StepIO
	onEvent       EventSink
}

// workflowFixedPlanSteps 返回固定顺序计划：执行 → 冻结验证 → 人工审批门。
func workflowFixedPlanSteps() []workflow.StepSpec {
	return []workflow.StepSpec{
		{ID: workflowStepExecuteID, Index: 0, Kind: workflow.StepKindExecute},
		{ID: workflowStepVerifyID, Index: 1, Kind: workflow.StepKindVerify},
		{ID: workflowStepAcceptID, Index: 2, Kind: workflow.StepKindAccept},
	}
}

// workflowPlanDigest 以步骤清单、任务制品摘要与配置指纹计算计划摘要；编
// 码失败返回空串并由负载校验失败关闭。
func workflowPlanDigest(steps []workflow.StepSpec, taskSHA256, configFingerprint string) string {
	body, err := json.Marshal(struct {
		Steps             []workflow.StepSpec `json:"steps"`
		TaskSHA256        string              `json:"task_sha256"`
		ConfigFingerprint string              `json:"config_fingerprint"`
	}{Steps: steps, TaskSHA256: taskSHA256, ConfigFingerprint: configFingerprint})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// WorkflowRun 创建固定计划工作流并顺序推进：任务文本以脱敏制品绑定进创建
// 事件，执行步骤经普通 run、验证步骤经零调用脚本化 run 驱动；需要人工介入
// （审批、对账、accept 门）时如实返回对应错误，绝不自动授权或重试不确定副
// 作用。计划含验证步骤而冻结验证命令为空时创建即失败关闭，不写任何状态。
func (service Service) WorkflowRun(ctx context.Context, options WorkflowRunOptions) (WorkflowOutcome, error) {
	if strings.TrimSpace(options.Task) == "" {
		return WorkflowOutcome{}, errors.New("workflow task is required")
	}
	if err := ctx.Err(); err != nil {
		return WorkflowOutcome{}, err
	}
	commands, err := verificationCommands(options.Config.Verification)
	if err != nil {
		return WorkflowOutcome{}, fmt.Errorf("build frozen verification commands: %w", err)
	}
	if len(commands) == 0 {
		return WorkflowOutcome{}, errors.New("workflow verify step requires frozen verification commands; configure verification.commands before starting a workflow")
	}
	_, hash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	artifacts, err := openArtifactStore(options.StateDir, hash, options.Config.Output)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	taskRef, err := artifacts.Put([]byte(options.Task), "text/plain; charset=utf-8")
	if err != nil {
		return WorkflowOutcome{}, fmt.Errorf("store workflow task artifact: %w", err)
	}
	// 后续步骤（含恢复路径）一律使用制品中的脱敏文本，保证创建与恢复的任
	// 务语义一致；制品读取路径负责摘要一致性校验。
	storedTask, err := artifacts.Open(taskRef)
	if err != nil {
		return WorkflowOutcome{}, fmt.Errorf("read back workflow task artifact: %w", err)
	}
	taskIO := workflow.StepIO{Ref: taskRef.SHA256, SHA: taskRef.SHA256, Summary: "workflow task"}
	store, err := workflowsqlite.Open(workflowDBPath(options.StateDir, hash))
	if err != nil {
		return WorkflowOutcome{}, err
	}
	defer store.Close()
	id := workflow.NewID()
	steps := workflowFixedPlanSteps()
	if _, err := store.Append(ctx, 0, workflow.EventDraft{
		WorkflowID:    id,
		WorkspaceHash: hash,
		Type:          workflow.EventRunCreated,
		Payload: artifact.MustEncodeRedactedJSON(workflow.RunCreatedPayload{
			Steps:               steps,
			PlanSHA256:          workflowPlanDigest(steps, taskRef.SHA256, options.Config.Fingerprint()),
			ConfigFingerprint:   options.Config.Fingerprint(),
			AuthorizationSource: workflowAuthorizationRun,
			Task:                taskIO,
		}),
	}); err != nil {
		return WorkflowOutcome{}, err
	}
	execution := workflowExecution{
		service: service, store: store, workspacePath: options.Workspace, workspaceHash: hash,
		stateDir: options.StateDir, configuration: options.Config, task: string(storedTask), taskIO: taskIO, onEvent: options.OnEvent,
	}
	return execution.advance(ctx, id)
}

// WorkflowResume 恢复既有工作流：先校验配置指纹与创建事件一致（漂移失败关
// 闭），再从链重放唯一确定当前位置并对账回填、续跑或顺序推进；uncertain
// 步骤在其绑定 run 进入终态前拒绝推进。
func (service Service) WorkflowResume(ctx context.Context, options WorkflowResumeOptions) (WorkflowOutcome, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowOutcome{}, err
	}
	_, hash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	store, err := openWorkflowStoreExisting(options.StateDir, hash)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	defer store.Close()
	execution := workflowExecution{
		service: service, store: store, workspacePath: options.Workspace, workspaceHash: hash,
		stateDir: options.StateDir, configuration: options.Config, onEvent: options.OnEvent,
	}
	snapshot, err := execution.reload(ctx, options.WorkflowID)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	if snapshot.Finished {
		return WorkflowOutcome{}, fmt.Errorf("%w: %s is %s", ErrWorkflowFinished, options.WorkflowID, snapshot.FinishState)
	}
	if snapshot.ConfigFingerprint != options.Config.Fingerprint() {
		return WorkflowOutcome{}, fmt.Errorf("%w: workflow %s was created under a different configuration", ErrConfigDrift, options.WorkflowID)
	}
	artifacts, err := openArtifactStore(options.StateDir, hash, options.Config.Output)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	task, err := readWorkflowTask(artifacts, snapshot.Task)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	execution.task, execution.taskIO = task, snapshot.Task
	return execution.advance(ctx, options.WorkflowID)
}

// WorkflowAccept 通过人工审批门：仅在 accept 步骤处于 pending 且前序步骤全
// 部成功时，显式写入步骤成功并收束工作流为 completed；授权来源随步骤事件
// 记录。其余状态一律拒绝，不存在任何自动通过路径。
func (service Service) WorkflowAccept(ctx context.Context, options WorkflowAcceptOptions) (WorkflowOutcome, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowOutcome{}, err
	}
	_, hash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	store, err := openWorkflowStoreExisting(options.StateDir, hash)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	defer store.Close()
	execution := workflowExecution{
		service: service, store: store, workspacePath: options.Workspace, workspaceHash: hash,
		stateDir: options.StateDir, configuration: options.Config, onEvent: options.OnEvent,
	}
	snapshot, err := execution.reload(ctx, options.WorkflowID)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	if snapshot.Finished {
		return WorkflowOutcome{}, fmt.Errorf("%w: %s is %s", ErrWorkflowFinished, options.WorkflowID, snapshot.FinishState)
	}
	if snapshot.ConfigFingerprint != options.Config.Fingerprint() {
		return WorkflowOutcome{}, fmt.Errorf("%w: workflow %s was created under a different configuration", ErrConfigDrift, options.WorkflowID)
	}
	if len(snapshot.RequiresReconcile) > 0 {
		return WorkflowOutcome{}, fmt.Errorf("%w: reconcile step %q before accepting", ErrWaitingReconcile, snapshot.RequiresReconcile[0])
	}
	index := snapshot.ResumeIndex
	if index < 0 || index >= len(snapshot.Steps) || snapshot.Kinds[snapshot.Steps[index].ID] != workflow.StepKindAccept {
		return WorkflowOutcome{}, fmt.Errorf("workflow %s is not waiting at the accept gate; continue with 'cairn workflow resume %s'", options.WorkflowID, options.WorkflowID)
	}
	step := snapshot.Steps[index]
	if step.State != workflow.StepPending {
		return WorkflowOutcome{}, fmt.Errorf("accept step %q is %s, expected pending", step.ID, step.State)
	}
	if _, err := execution.appendDraft(ctx, options.WorkflowID, workflow.EventStepStarted, workflow.StepStartedPayload{
		StepID: step.ID, AuthorizationSource: workflowAuthorizationAccept,
	}); err != nil {
		return WorkflowOutcome{}, err
	}
	if _, err := execution.appendDraft(ctx, options.WorkflowID, workflow.EventStepFinished, workflow.StepFinishedPayload{
		StepID: step.ID, State: workflow.StepSucceeded, Output: workflow.StepIO{Summary: "human accepted"},
	}); err != nil {
		return WorkflowOutcome{}, err
	}
	if err := execution.appendRunFinished(ctx, options.WorkflowID, workflow.RunStateCompleted, -1); err != nil {
		return WorkflowOutcome{}, err
	}
	final, err := execution.reload(ctx, options.WorkflowID)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	return workflowOutcomeFor(final, ""), nil
}

// advance 从当前链头推进工作流直至终态、人工介入或错误；每一轮都以重放投
// 影为唯一事实源，不在内存另立权威状态。
func (execution workflowExecution) advance(ctx context.Context, id workflow.ID) (WorkflowOutcome, error) {
	for {
		snapshot, err := execution.reload(ctx, id)
		if err != nil {
			return WorkflowOutcome{}, err
		}
		if snapshot.Finished {
			hint := ""
			switch snapshot.FinishState {
			case workflow.RunStateFailed:
				hint = "工作流已失败；如需重试请创建新工作流"
			case workflow.RunStateCancelled:
				hint = "工作流已取消；如需继续请创建新工作流"
			}
			return workflowOutcomeFor(snapshot, hint), nil
		}
		if uncertain := snapshot.RequiresReconcile; len(uncertain) > 0 {
			settled, settleErr := execution.reconcileUncertainStep(ctx, id, snapshot, uncertain[0])
			if settleErr != nil {
				return WorkflowOutcome{}, settleErr
			}
			if settled {
				continue
			}
			runID := snapshot.StepRuns[uncertain[0]]
			return workflowOutcomeFor(snapshot, ""), fmt.Errorf("%w: workflow step %q has an unresolved side effect in run %s; reconcile with 'cairn resume %s --resolve <operation>=<verdict>' before resuming the workflow", ErrWaitingReconcile, uncertain[0], runID, runID)
		}
		index := snapshot.ResumeIndex
		if index < 0 {
			// 全部步骤成功但没有终态事件的收束窗口：唯一一致动作是补写
			// completed，不产生新的步骤副作用。
			if err := execution.appendRunFinished(ctx, id, workflow.RunStateCompleted, -1); err != nil {
				return WorkflowOutcome{}, err
			}
			continue
		}
		if index >= len(snapshot.Steps) {
			return WorkflowOutcome{}, fmt.Errorf("%w: workflow resume index %d is out of range", ErrStateCorrupt, index)
		}
		step := snapshot.Steps[index]
		var driveErr error
		switch snapshot.Kinds[step.ID] {
		case workflow.StepKindAccept:
			return workflowOutcomeFor(snapshot, "人工审批门：确认结果后执行 cairn workflow accept "+string(id)), nil
		case workflow.StepKindExecute:
			driveErr = execution.driveStep(ctx, id, snapshot, step, false)
		case workflow.StepKindVerify:
			driveErr = execution.driveStep(ctx, id, snapshot, step, true)
		default:
			return WorkflowOutcome{}, fmt.Errorf("%w: workflow step %q has unknown kind %q", ErrStateCorrupt, step.ID, snapshot.Kinds[step.ID])
		}
		if driveErr != nil {
			// 步骤可能已经写入终态或不确定状态：返回前重新重放，保证对外视图
			// 与链上事实一致，不返回推进前的旧快照。
			refreshed, reloadErr := execution.reload(ctx, id)
			if reloadErr != nil {
				return workflowOutcomeFor(snapshot, ""), errors.Join(driveErr, reloadErr)
			}
			return workflowOutcomeFor(refreshed, ""), driveErr
		}
	}
}

// driveStep 启动或续推单个执行/验证步骤；启动前确认上下文未取消且工作区
// 没有非终态运行，二者都不写任何步骤事件。
func (execution workflowExecution) driveStep(ctx context.Context, id workflow.ID, snapshot *workflow.Snapshot, step workflow.Step, verify bool) error {
	if step.State == workflow.StepRunning {
		return execution.resumeStepRun(ctx, id, snapshot, step, verify)
	}
	if err := ctx.Err(); err != nil {
		if cancelErr := execution.cancelWorkflow(ctx, id); cancelErr != nil {
			return errors.Join(err, cancelErr)
		}
		return err
	}
	if err := execution.requireIdleWorkspace(ctx); err != nil {
		return err
	}
	runID := domain.NewRunID()
	if _, err := execution.appendDraft(ctx, id, workflow.EventStepStarted, workflow.StepStartedPayload{
		StepID: step.ID, Input: execution.taskIO, RunID: string(runID),
	}); err != nil {
		return err
	}
	if verify {
		outcome, runErr := execution.service.startWorkflowVerifyRun(ctx, execution, runID, id, step.ID)
		return execution.settleStepRun(ctx, id, step, runID, outcome, runErr, true)
	}
	outcome, runErr := execution.service.Run(ctx, RunOptions{
		Task: execution.task, Workspace: execution.workspacePath, StateDir: execution.stateDir,
		Config: execution.configuration, OnEvent: execution.onEvent, PreassignedRunID: runID,
	})
	return execution.settleStepRun(ctx, id, step, runID, outcome, runErr, false)
}

// settleStepRun 把步骤 run 的执行结果映射为步骤终态：完成映射成功（验证步
// 骤还要求验证证据为 OK）、失败映射失败并立即收束工作流、取消按取消事实收
// 束全部剩余步骤；等待审批时不写步骤终态，等待对账时记 uncertain 并拒绝
// 推进。其余错误原样透传，步骤保持 running 供恢复入口回填。
func (execution workflowExecution) settleStepRun(ctx context.Context, id workflow.ID, step workflow.Step, runID domain.RunID, outcome RunResult, runErr error, verify bool) error {
	var waiting *ports.WaitingApprovalError
	if errors.As(runErr, &waiting) {
		return fmt.Errorf("workflow step %q paused for approval in run %s; resolve with 'cairn resume %s --approve %s' (or --deny), then 'cairn workflow resume %s': %w", step.ID, runID, runID, waiting.Fingerprint, id, waiting)
	}
	if errors.Is(runErr, ports.ErrWaitingReconcile) {
		if _, err := execution.appendDraft(ctx, id, workflow.EventStepFinished, workflow.StepFinishedPayload{
			StepID: step.ID, State: workflow.StepUncertain, ErrorClass: "waiting_reconcile",
		}); err != nil {
			return err
		}
		return fmt.Errorf("%w: workflow step %q hit an unresolved side effect in run %s; reconcile with 'cairn resume %s --resolve <operation>=<verdict>' before resuming the workflow", ErrWaitingReconcile, step.ID, runID, runID)
	}
	if errors.Is(runErr, context.Canceled) {
		if cancelErr := execution.cancelWorkflow(ctx, id); cancelErr != nil {
			return errors.Join(runErr, cancelErr)
		}
		return runErr
	}
	if outcome.ID == "" && outcome.Result == nil {
		// run 从未创建（装配、准入或活跃运行检查失败）：步骤立即失败收束，
		// 不留下指向不存在 run 的运行中步骤。
		if runErr == nil {
			runErr = errors.New("step run did not start")
		}
		if err := execution.failStep(ctx, id, step.ID, "startup_error"); err != nil {
			return errors.Join(runErr, err)
		}
		return runErr
	}
	if outcome.Result != nil {
		switch outcome.Result.State {
		case string(domain.RunCompleted):
			if verify && !outcome.Result.Evidence.VerificationOK {
				return execution.failStep(ctx, id, step.ID, "verification_failed")
			}
			_, err := execution.appendDraft(ctx, id, workflow.EventStepFinished, workflow.StepFinishedPayload{
				StepID: step.ID, State: workflow.StepSucceeded,
				Output: workflow.StepIO{Summary: "run " + string(runID) + " completed"},
			})
			return err
		case string(domain.RunFailed):
			failureClass := outcome.Result.FailureClass
			if failureClass == "" {
				failureClass = "failed"
			}
			return execution.failStep(ctx, id, step.ID, failureClass)
		case string(domain.RunCancelled):
			if err := execution.cancelWorkflow(ctx, id); err != nil {
				return err
			}
			return context.Canceled
		}
	}
	return runErr
}

// failStep 记录步骤失败并立即收束工作流为 failed；失败的工作流不再恢复，
// 重试通过创建新工作流表达。
func (execution workflowExecution) failStep(ctx context.Context, id workflow.ID, stepID, errorClass string) error {
	snapshot, err := execution.reload(ctx, id)
	if err != nil {
		return err
	}
	if _, err := execution.appendDraft(ctx, id, workflow.EventStepFinished, workflow.StepFinishedPayload{
		StepID: stepID, State: workflow.StepFailed, ErrorClass: errorClass,
	}); err != nil {
		return err
	}
	return execution.appendRunFinished(ctx, id, workflow.RunStateFailed, snapshot.ResumeIndex)
}

// cancelWorkflow 按取消事实收束：执行中与未开始的步骤逐个记 cancelled，再
// 写 workflow_run_finished(cancelled, resume_index)；已收束步骤保持原状。
func (execution workflowExecution) cancelWorkflow(ctx context.Context, id workflow.ID) error {
	// 取消事实必须落链：调用方的上下文此时通常已取消，直接用它写事件会连同
	// 取消一起失败，取消事实将无处可查。改用脱离取消的短时提交上下文，与
	// 运行层 persistCancellation 的提交语义保持一致。
	commitContext, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), workflowCommitTimeout)
	defer cancelCommit()
	snapshot, err := execution.reload(commitContext, id)
	if err != nil {
		return err
	}
	if snapshot.Finished {
		return nil
	}
	resumeIndex := -1
	for index := range snapshot.Steps {
		step := snapshot.Steps[index]
		if step.State != workflow.StepPending && step.State != workflow.StepRunning {
			continue
		}
		if resumeIndex < 0 {
			resumeIndex = index
		}
		if _, err := execution.appendDraft(commitContext, id, workflow.EventStepFinished, workflow.StepFinishedPayload{
			StepID: step.ID, State: workflow.StepCancelled, ErrorClass: "cancelled",
		}); err != nil {
			return err
		}
	}
	return execution.appendRunFinished(commitContext, id, workflow.RunStateCancelled, resumeIndex)
}

// resumeStepRun 处理执行中步骤的中断窗口：按绑定 run 的投影唯一决定回填、
// 续跑或等待；run 缺失或链不可回放时失败关闭，绝不盲目重跑。
func (execution workflowExecution) resumeStepRun(ctx context.Context, id workflow.ID, snapshot *workflow.Snapshot, step workflow.Step, verify bool) error {
	bound := snapshot.StepRuns[step.ID]
	if bound == "" {
		return fmt.Errorf("%w: workflow step %q is running without a bound run; legacy or corrupt chain cannot be resumed", ErrStateCorrupt, step.ID)
	}
	runID := domain.RunID(bound)
	journal, err := openReadOnlyJournal(execution.stateDir, execution.workspaceHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return missingBoundRunError(step.ID, bound)
		}
		return err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		// 步骤已开始但绑定的 run 不存在：中断窗口落在「记步骤开始」与「创建
		// run」之间，副作用从未发生，但状态机不允许把运行中的步骤重新驱动。
		// 失败关闭并指明唯一出路，不猜测旧内容也不自动重试。
		return missingBoundRunError(step.ID, bound)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return fmt.Errorf("%w: run %s: %v", ErrStateCorrupt, runID, err)
	}
	switch phase := projection.Phase(); phase {
	case "waiting_approval":
		pending := projection.PendingApproval
		if pending == nil {
			return fmt.Errorf("%w: run %s reports waiting_approval without a pending decision", ErrStateCorrupt, runID)
		}
		waiting := &ports.WaitingApprovalError{RunID: runID, OperationID: pending.OperationID, Fingerprint: pending.Fingerprint, Summary: pending.Summary}
		return fmt.Errorf("workflow step %q is waiting for approval in run %s; resolve with 'cairn resume %s --approve %s' (or --deny), then 'cairn workflow resume %s': %w", step.ID, runID, runID, pending.Fingerprint, id, waiting)
	case "waiting_reconcile":
		if _, err := execution.appendDraft(ctx, id, workflow.EventStepFinished, workflow.StepFinishedPayload{
			StepID: step.ID, State: workflow.StepUncertain, ErrorClass: "waiting_reconcile",
		}); err != nil {
			return err
		}
		return fmt.Errorf("%w: workflow step %q hit an unresolved side effect in run %s; reconcile with 'cairn resume %s --resolve <operation>=<verdict>' before resuming the workflow", ErrWaitingReconcile, step.ID, runID, runID)
	case "created", "cancelling":
		return fmt.Errorf("%w: run %s is %s after an interrupted lifecycle; finish it with 'cairn resume %s --abort-run --reason <text>' before resuming the workflow", ErrActiveRun, runID, phase, runID)
	}
	if projection.Terminal() {
		result, err := runResultFromEvents(events, projection, execution.stateDir, execution.workspaceHash)
		if err != nil {
			return err
		}
		return execution.settleStepRun(ctx, id, step, runID, RunResult{ID: runID, Result: &result}, nil, verify)
	}
	outcome, runErr := execution.service.Resume(ctx, ResumeOptions{
		RunID: runID, Workspace: execution.workspacePath, StateDir: execution.stateDir,
		Config: execution.configuration, OnEvent: execution.onEvent,
	})
	return execution.settleStepRun(ctx, id, step, runID, outcome, runErr, verify)
}

// reconcileUncertainStep 在 uncertain 步骤的绑定 run 进入终态后回填步骤终
// 态；run 仍未收束时返回 settled=false，由调用方拒绝推进并指向 run 级对账。
func (execution workflowExecution) reconcileUncertainStep(ctx context.Context, id workflow.ID, snapshot *workflow.Snapshot, stepID string) (bool, error) {
	bound := snapshot.StepRuns[stepID]
	if bound == "" {
		return false, fmt.Errorf("%w: uncertain step %q has no bound run to reconcile", ErrStateCorrupt, stepID)
	}
	runID := domain.RunID(bound)
	journal, err := openReadOnlyJournal(execution.stateDir, execution.workspaceHash)
	if err != nil {
		return false, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return false, err
	}
	if len(events) == 0 {
		return false, fmt.Errorf("%w: uncertain step %q references missing run %s", ErrStateCorrupt, stepID, runID)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return false, fmt.Errorf("%w: run %s: %v", ErrStateCorrupt, runID, err)
	}
	if !projection.Terminal() {
		return false, nil
	}
	result, err := runResultFromEvents(events, projection, execution.stateDir, execution.workspaceHash)
	if err != nil {
		return false, err
	}
	verify := snapshot.Kinds[stepID] == workflow.StepKindVerify
	if result.State == string(domain.RunCompleted) && (!verify || result.Evidence.VerificationOK) {
		if _, err := execution.appendDraft(ctx, id, workflow.EventStepFinished, workflow.StepFinishedPayload{
			StepID: stepID, State: workflow.StepSucceeded,
			Output: workflow.StepIO{Summary: "reconciled after run " + string(runID)},
		}); err != nil {
			return false, err
		}
		return true, nil
	}
	// run 失败、取消或验证未通过：uncertain 只能收束为失败，保持失败关闭。
	errorClass := result.FailureClass
	switch {
	case result.State == string(domain.RunCompleted):
		errorClass = "verification_failed"
	case result.State == string(domain.RunCancelled):
		errorClass = "cancelled"
	case errorClass == "":
		errorClass = "failed"
	}
	if err := execution.failStep(ctx, id, stepID, errorClass); err != nil {
		return false, err
	}
	return true, nil
}

// startWorkflowVerifyRun 装配 Boot、手工创建验证步骤 run（task_ref 绑定验
// 证计划制品）并以零调用脚本化计划强制执行冻结验证；Boot 生命周期由本函数
// 持有，等待审批时如实返回供步骤保持运行中。
func (service Service) startWorkflowVerifyRun(ctx context.Context, execution workflowExecution, runID domain.RunID, id workflow.ID, stepID string) (outcome RunResult, err error) {
	boot, err := service.assembleBoot(execution.workspacePath, execution.stateDir, execution.configuration, execution.onEvent)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	active, err := boot.Journal.NonTerminalRuns(ctx, boot.WorkspaceHash)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	if len(active) != 0 {
		return RunResult{ID: runID}, activeRunError(active[0])
	}
	if err := enforceAdmissionGate(execution.stateDir, boot.WorkspaceHash, execution.configuration); err != nil {
		return RunResult{ID: runID}, err
	}
	build, err := currentBuildIdentity()
	if err != nil {
		return RunResult{ID: runID}, err
	}
	frozenConfig, err := marshalFrozenRunConfig(execution.configuration, execution.configuration.Fingerprint())
	if err != nil {
		return RunResult{ID: runID}, err
	}
	configRef, err := boot.Artifacts.Put(frozenConfig, "application/json")
	if err != nil {
		return RunResult{ID: runID}, err
	}
	planRef, err := boot.Artifacts.Put(artifact.MustEncodeRedactedJSON(workflowVerifyPlanArtifact{
		Version: workflowVerifyPlanVersion, Kind: workflowVerifyPlanKind, WorkflowID: string(id), StepID: stepID,
	}), "application/json")
	if err != nil {
		return RunResult{ID: runID}, err
	}
	task := fmt.Sprintf("工作流验证门：%s 步骤 %s", id, stepID)
	createdPayload := struct {
		domain.RunCreatedPayload
		ConfigRef artifact.Ref `json:"config_ref"`
		TaskRef   artifact.Ref `json:"task_ref"`
	}{RunCreatedPayload: domain.RunCreatedPayload{
		Task: task, Workspace: boot.Workspace.Root, ConfigFingerprint: execution.configuration.Fingerprint(), Build: build,
	}, ConfigRef: configRef, TaskRef: planRef}
	if _, err := boot.Journal.Append(ctx, 0, domain.EventDraft{
		RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunCreated,
		Payload: artifact.MustEncodeRedactedJSON(createdPayload),
	}); err != nil {
		return RunResult{ID: runID}, err
	}
	if _, err := boot.Journal.Append(ctx, 1, domain.EventDraft{
		RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunStarted,
		Payload: artifact.MustEncodeRedactedJSON(struct {
			Invocation string `json:"invocation"`
		}{Invocation: domain.NewID("invoke")}),
	}); err != nil {
		return RunResult{ID: runID}, err
	}
	return service.driveWorkflowVerifyRun(ctx, boot, runID, ResumeOptions{
		Workspace: execution.workspacePath, StateDir: execution.stateDir,
		Config: execution.configuration, OnEvent: execution.onEvent,
	})
}

// driveWorkflowVerifyRun 以零调用脚本化计划驱动既有验证 run 收束冻结验证；
// 它不触碰模型，审批与对账暂停原样上抛，由 run 级 Resume/Reconcile 续跑。
func (service Service) driveWorkflowVerifyRun(ctx context.Context, boot *Boot, runID domain.RunID, options ResumeOptions) (RunResult, error) {
	events, err := boot.Journal.Load(ctx, runID)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	if len(events) == 0 {
		return RunResult{ID: runID}, fmt.Errorf("workflow verify run %s not found", runID)
	}
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	defer cancelRun()
	input := agent.RunInput{RunID: runID, WorkspaceHash: boot.WorkspaceHash}
	result, err := boot.Loop.RunScriptedToolPlan(runContext, input, options.Approval, nil)
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = boot.Loop.RunScriptedToolPlan(expiredContext, input, ports.Approval{}, nil)
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, runID, result.Response, err)
}

// requireIdleWorkspace 在启动新步骤 run 前确认工作区没有非终态运行；日志尚
// 不存在视为空闲，活跃运行返回与其恢复指引一致的错误。
func (execution workflowExecution) requireIdleWorkspace(ctx context.Context) error {
	runsPath := filepath.Join(execution.stateRoot(), "workspaces", execution.workspaceHash, "runs.db")
	if _, statErr := os.Stat(runsPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil
		}
		return statErr
	}
	journal, err := openReadOnlyJournal(execution.stateDir, execution.workspaceHash)
	if err != nil {
		return err
	}
	defer journal.Close()
	active, err := journal.NonTerminalRuns(ctx, execution.workspaceHash)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return nil
	}
	return activeRunError(active[0])
}

// missingBoundRunError 描述「步骤已记开始、绑定的 run 却不存在」的中断窗口：
// 副作用从未发生，但状态机不允许重新驱动运行中的步骤，因此失败关闭并指明
// 唯一出路，不猜测旧内容也不自动重试。
func missingBoundRunError(stepID, runID string) error {
	return fmt.Errorf("%w: workflow step %q references missing run %s; the step never started and cannot be resumed, create a new workflow", ErrStateCorrupt, stepID, runID)
}

// reload 重放工作流链并返回唯一权威投影；存储缺失与链损坏分别失败关闭。
func (execution workflowExecution) reload(ctx context.Context, id workflow.ID) (*workflow.Snapshot, error) {
	events, err := execution.store.Load(ctx, id)
	if err != nil {
		if workflowsqlite.IsStateCorruptError(err) {
			return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
		}
		return nil, err
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrWorkflowNotFound, id)
	}
	snapshot, err := workflow.Project(events)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return snapshot, nil
}

// appendDraft 以链头 CAS 追加一个工作流事件；本执行器是链的唯一写入方，
// 冲突即失败关闭，不重试以免掩盖并发写入。
func (execution workflowExecution) appendDraft(ctx context.Context, id workflow.ID, eventType workflow.EventType, payload any) (workflow.Event, error) {
	events, err := execution.store.Load(ctx, id)
	if err != nil {
		return workflow.Event{}, err
	}
	if len(events) == 0 {
		return workflow.Event{}, fmt.Errorf("%w: %s", ErrWorkflowNotFound, id)
	}
	draft := workflow.EventDraft{
		WorkflowID:    id,
		WorkspaceHash: execution.workspaceHash,
		Type:          eventType,
		Payload:       artifact.MustEncodeRedactedJSON(payload),
		OccurredAt:    time.Now().UTC(),
	}
	return execution.store.Append(ctx, events[len(events)-1].Seq, draft)
}

// appendRunFinished 收束工作流终态；resumeIndex 记录首个未成功步骤下标，
// 全部成功时为 -1。
func (execution workflowExecution) appendRunFinished(ctx context.Context, id workflow.ID, state string, resumeIndex int) error {
	_, err := execution.appendDraft(ctx, id, workflow.EventRunFinished, workflow.RunFinishedPayload{State: state, ResumeIndex: resumeIndex})
	return err
}

// stateRoot 解析状态目录的统一根；空值回退平台默认，与装配路径一致。
func (execution workflowExecution) stateRoot() string {
	if execution.stateDir == "" {
		return DefaultStateDir()
	}
	return execution.stateDir
}

// openWorkflowStoreExisting 打开既有工作流存储供追加；库文件不存在时以未
// 找到失败关闭，不隐式建库。
func openWorkflowStoreExisting(stateDir, workspaceHash string) (*workflowsqlite.Store, error) {
	path := workflowDBPath(stateDir, workspaceHash)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: no workflow store in this workspace", ErrWorkflowNotFound)
		}
		return nil, err
	}
	return workflowsqlite.Open(path)
}

// readWorkflowTask 从制品库读回创建事件绑定的任务文本；缺少绑定或制品不
// 可读时失败关闭，不猜测任务正文。
func readWorkflowTask(artifacts *artifact.Store, binding workflow.StepIO) (string, error) {
	if binding.Ref == "" {
		return "", errors.New("workflow created event has no task artifact binding; chains created before the execution slice cannot be resumed")
	}
	content, err := artifacts.Open(artifact.Ref{SHA256: binding.Ref})
	if err != nil {
		return "", fmt.Errorf("open workflow task artifact: %w", err)
	}
	return string(content), nil
}

// workflowOutcomeFor 由投影构造统一返回；hint 仅是人工下一步指引。
func workflowOutcomeFor(snapshot *workflow.Snapshot, hint string) WorkflowOutcome {
	return WorkflowOutcome{View: workflowViewFromSnapshot(snapshot), NextAction: hint}
}

// workflowVerifyRunOfRun 判定 run 是否为工作流验证步骤的脚本化 run：以
// task_ref 制品的 kind 标记识别，已识别制品损坏时失败关闭。
func workflowVerifyRunOfRun(ctx context.Context, journal *sqlite.Journal, artifacts *artifact.Store, runID domain.RunID) (bool, error) {
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return false, err
	}
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return false, fmt.Errorf("run %s is missing run_created event", runID)
	}
	var payload struct {
		TaskRef *artifact.Ref `json:"task_ref"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		return false, err
	}
	if payload.TaskRef == nil {
		return false, nil
	}
	content, err := artifacts.Open(*payload.TaskRef)
	if err != nil {
		var previewMarker struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(payload.TaskRef.TruncatedPreview), &previewMarker) == nil && previewMarker.Kind == workflowVerifyPlanKind {
			return false, fmt.Errorf("open workflow verify plan artifact: %w", err)
		}
		return false, nil
	}
	var marker struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(content, &marker) != nil || marker.Kind != workflowVerifyPlanKind {
		return false, nil
	}
	return true, nil
}
