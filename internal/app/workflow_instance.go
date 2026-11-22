// [INPUT]: 依赖版本化定义、工作流 SQLite 与既有 Run 应用入口。
// [OUTPUT]: 提供普通实例创建、任务实例追加、人工门决策与实例推进锁。
// [POS]: 普通实例读取保存的修订。任务实例使用任务先行持久化的身份。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件装配版本化工作流实例。
// Author: fxbin
package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	workflowsqlite "github.com/fxbin/cairn/internal/capabilities/workflow/storage/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/runtime/lock"
)

const (
	workflowDefinitionAuthorization = "user:workflow-start"
	workflowDecisionMaxBytes        = 1024
)

// StartWorkflowOptions 绑定调用者选定的已保存定义修订。
type StartWorkflowOptions struct {
	Scope        WorkflowScope
	DefinitionID WorkflowDefinitionID
	Revision     uint64
	OnEvent      EventSink
}

// WorkflowDecisionOptions 将人工决定绑定到调用者实际看到的实例链头和步骤。
type WorkflowDecisionOptions struct {
	Scope       WorkflowScope
	WorkflowID  workflow.ID
	StepID      string
	Sequence    uint64
	Reason      string
	OnEvent     EventSink
	RunID       domain.RunID
	OperationID domain.OperationID
	Fingerprint string
}

// CreateWorkflowInstance 保存不可变的实例快照，返回实例 ID 后才允许驱动。
func (Service) CreateWorkflowInstance(ctx context.Context, options StartWorkflowOptions) (WorkflowStatusView, error) {
	scope, hash, definition, ref, err := prepareWorkflowStart(ctx, options)
	if err != nil {
		return WorkflowStatusView{}, err
	}
	return createWorkflowInstanceAt(ctx, scope, hash, definition, ref, workflow.NewID(), "")
}

// createWorkflowInstanceAt 以预期空链追加创建事件。任务负责先持久化指定实例身份。
func createWorkflowInstanceAt(ctx context.Context, scope WorkflowScope, hash string, definition WorkflowDefinition, ref WorkflowDefinitionRef, id workflow.ID, taskID string) (WorkflowStatusView, error) {
	if strings.HasPrefix(string(ref.ID), taskWorkflowDefinitionPrefix) != (taskID != "") ||
		taskID != "" && ref.ID != taskWorkflowDefinitionID(TaskID(taskID)) {
		return WorkflowStatusView{}, errors.New("task workflow definitions require their owning task")
	}
	store, err := workflowsqlite.Open(workflowDBPath(scope.StateDir, hash))
	if err != nil {
		return WorkflowStatusView{}, err
	}
	defer store.Close()
	authorization := workflowDefinitionAuthorization
	if taskID != "" {
		authorization = workflow.TaskWorkflowAuthorization
	}
	fingerprint := scope.Config.Fingerprint()
	specs := workflowStepSpecs(definition)
	created, err := store.Append(ctx, 0, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventRunCreated,
		Payload: contract.MustEncodeRedactedJSON(workflow.RunCreatedPayload{
			Steps: specs, PlanSHA256: workflowPlanDigest(specs, ref.Artifact.SHA256, fingerprint),
			ConfigFingerprint: fingerprint, AuthorizationSource: authorization,
			Definition: &ref, SessionID: domain.NewID("session"), TaskID: taskID,
		}),
	})
	if err != nil {
		return WorkflowStatusView{}, err
	}
	snapshot, err := workflow.Project([]workflow.Event{created})
	if err != nil {
		return WorkflowStatusView{}, err
	}
	execution := workflowExecution{stateDir: scope.StateDir, workspaceHash: hash, definition: &definition}
	return execution.workflowStatusView(ctx, snapshot)
}

// CheckWorkflowDefinition 对已明确选择的持久修订只读预演，不启动实例。
func (Service) CheckWorkflowDefinition(ctx context.Context, options StartWorkflowOptions) error {
	_, _, _, _, err := prepareWorkflowStart(ctx, options)
	return err
}

// CheckWorkflowDraft 对尚未保存的宿主草案预演，且不创建任何状态。
func (Service) CheckWorkflowDraft(ctx context.Context, scope WorkflowScope, draft WorkflowDefinitionDraft) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scope, hash, err := scope.resolve()
	if err != nil {
		return err
	}
	if err := validateHookConfiguration(scope.Config); err != nil {
		return err
	}
	definition, err := checkedWorkflowDraft(hash, draft)
	if err != nil {
		return err
	}
	return validateWorkflowRuntime(definition, scope.Config)
}

// CheckWorkflowDraftStructure 只验证旧预演入口展示的草案结构。
func (Service) CheckWorkflowDraftStructure(ctx context.Context, workspace string, draft WorkflowDefinitionDraft) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, hash, err := workspaceFor(workspace, config.Defaults())
	if err != nil {
		return err
	}
	_, err = checkedWorkflowDraft(hash, draft)
	return err
}

// checkedWorkflowDraft 复用定义规范化和制品大小限制。
func checkedWorkflowDraft(hash string, draft WorkflowDefinitionDraft) (WorkflowDefinition, error) {
	definition, err := workflow.NewDefinition(hash, draft, 1)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	content, err := json.Marshal(definition)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	if len(content) > workflow.MaxDefinitionArtifactBytes {
		return WorkflowDefinition{}, errors.New("workflow draft exceeds artifact limit")
	}
	return definition, nil
}

// prepareWorkflowStart 校验选中的修订、宿主运行配置和节点执行条件。
func prepareWorkflowStart(ctx context.Context, options StartWorkflowOptions) (WorkflowScope, string, WorkflowDefinition, WorkflowDefinitionRef, error) {
	scope, hash, err := options.Scope.resolve()
	if err != nil {
		return WorkflowScope{}, "", WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return WorkflowScope{}, "", WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	if options.Revision == 0 {
		return WorkflowScope{}, "", WorkflowDefinition{}, WorkflowDefinitionRef{}, errors.New("workflow start requires an explicitly selected definition revision")
	}
	if strings.HasPrefix(string(options.DefinitionID), taskWorkflowDefinitionPrefix) {
		return WorkflowScope{}, "", WorkflowDefinition{}, WorkflowDefinitionRef{}, errors.New("task workflow definitions require their owning task")
	}
	if err := validateHookConfiguration(scope.Config); err != nil {
		return WorkflowScope{}, "", WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	definition, ref, err := loadWorkflowDefinition(ctx, scope, hash, options.DefinitionID, options.Revision)
	if err != nil {
		return WorkflowScope{}, "", WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	if definition.Verification != nil {
		if err := requireIsolatedVerificationState(scope.Workspace, scope.StateDir); err != nil {
			return WorkflowScope{}, "", WorkflowDefinition{}, WorkflowDefinitionRef{}, err
		}
	}
	if err := validateWorkflowRuntime(definition, scope.Config); err != nil {
		return WorkflowScope{}, "", WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	return scope, hash, definition, ref, nil
}

// StartWorkflow 创建实例后通过通用恢复入口推进，错误时也返回已创建实例身份。
func (service Service) StartWorkflow(ctx context.Context, options StartWorkflowOptions) (WorkflowOutcome, error) {
	view, err := service.CreateWorkflowInstance(ctx, options)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	scope := options.Scope
	outcome, err := service.WorkflowResume(ctx, WorkflowResumeOptions{
		WorkflowID: WorkflowID(view.WorkflowID), Workspace: scope.Workspace, StateDir: scope.StateDir, Config: scope.Config, OnEvent: options.OnEvent,
	})
	if outcome.View.WorkflowID == "" {
		outcome.View = view
	}
	return outcome, err
}

// ApproveStep 保存显式人工门决定并推进后续步骤。
func (service Service) ApproveStep(ctx context.Context, options WorkflowDecisionOptions) (WorkflowOutcome, error) {
	return service.decideWorkflowGate(ctx, options, workflow.GateApproved)
}

// RejectStep 保存显式拒绝，保留已结算步骤及本次拒绝的尝试。
func (service Service) RejectStep(ctx context.Context, options WorkflowDecisionOptions) (WorkflowOutcome, error) {
	return service.decideWorkflowGate(ctx, options, workflow.GateRejected)
}

// decideWorkflowGate 重验期望链头、当前步骤及产物后，以一个事件绑定人工决定。
func (service Service) decideWorkflowGate(ctx context.Context, options WorkflowDecisionOptions, decision string) (WorkflowOutcome, error) {
	reason, err := validWorkflowReason(options.Reason)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	execution, snapshot, release, err := service.openWorkflowMutation(ctx, options.Scope, options.WorkflowID, options.OnEvent)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	defer release()
	if snapshot.LastSeq != options.Sequence {
		return WorkflowOutcome{}, workflow.ErrConflict
	}
	if snapshot.Paused {
		return WorkflowOutcome{}, errors.New("workflow is paused")
	}
	index := snapshot.ResumeIndex
	if index < 0 || index >= len(snapshot.Steps) || snapshot.Steps[index].ID != options.StepID {
		return WorkflowOutcome{}, errors.New("workflow decision requires the current step")
	}
	if snapshot.Specs[index].Kind != workflow.StepKindAccept {
		return execution.decideWorkflowRunApproval(ctx, snapshot, options, decision == workflow.GateApproved, reason)
	}
	if snapshot.Steps[index].State != workflow.StepPending || options.RunID != "" || options.OperationID != "" || options.Fingerprint != "" {
		return WorkflowOutcome{}, errors.New("workflow is not waiting for this human decision")
	}
	dependencies, err := snapshot.Dependencies(index)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	if _, err := execution.readWorkflowDependencies(dependencies); err != nil {
		return WorkflowOutcome{}, err
	}
	if err := execution.recordWorkflowHumanDecision(ctx, snapshot, workflow.GateDecidedPayload{
		StepID: options.StepID, Attempt: len(snapshot.Attempts[options.StepID]) + 1,
		Decision: decision, Reason: reason, Dependencies: dependencies,
	}); err != nil {
		return WorkflowOutcome{}, err
	}
	return execution.advance(ctx, options.WorkflowID)
}

// recordWorkflowHumanDecision 将验证工作流的批准绑定到当前工作区及报告版本。
func (execution workflowExecution) recordWorkflowHumanDecision(ctx context.Context, snapshot *workflow.Snapshot, payload workflow.GateDecidedPayload) error {
	if execution.definition.Verification != nil && payload.Decision == workflow.GateApproved {
		lease, err := acquireWorkspaceLock(execution.stateDir, execution.workspaceHash)
		if err != nil {
			return err
		}
		defer lease.Release()
		view, err := execution.workflowStatusView(ctx, snapshot)
		if err != nil {
			return err
		}
		if err := requireCurrentWorkflowVerification(execution.workspacePath, execution.configuration, view.Verification); err != nil {
			return err
		}
	}
	_, err := execution.appendDraft(ctx, snapshot.WorkflowID, workflow.EventGateDecided, payload)
	return err
}

// openWorkflowMutation 取得实例锁并校验冻结的定义及有效配置。
func (service Service) openWorkflowMutation(ctx context.Context, scope WorkflowScope, id workflow.ID, onEvent EventSink) (workflowExecution, *workflow.Snapshot, func(), error) {
	scope, hash, err := scope.resolve()
	if err != nil {
		return workflowExecution{}, nil, nil, err
	}
	if err := validateHookConfiguration(scope.Config); err != nil {
		return workflowExecution{}, nil, nil, err
	}
	store, err := openWorkflowStoreExisting(scope.StateDir, hash)
	if err != nil {
		return workflowExecution{}, nil, nil, err
	}
	lease, err := lockWorkflowInstance(scope.StateDir, hash, id)
	if err != nil {
		_ = store.Close()
		return workflowExecution{}, nil, nil, err
	}
	release := func() {
		_ = lease.Release()
		_ = store.Close()
	}
	execution := workflowExecution{
		service: service, store: store, stateDir: scope.StateDir, workspacePath: scope.Workspace, workspaceHash: hash,
		configuration: scope.Config, onEvent: onEvent,
	}
	snapshot, err := execution.reload(ctx, id)
	if err == nil {
		err = execution.prepareVersionedWorkflow(ctx, snapshot)
	}
	if err != nil {
		release()
		return workflowExecution{}, nil, nil, err
	}
	return execution, snapshot, release, nil
}

// prepareVersionedWorkflow 解引用创建事件的定义，核对节点和运行配置。
func (execution *workflowExecution) prepareVersionedWorkflow(ctx context.Context, snapshot *workflow.Snapshot) error {
	if snapshot.Finished {
		return fmt.Errorf("%w: %s is %s", ErrWorkflowFinished, snapshot.WorkflowID, snapshot.FinishState)
	}
	if snapshot.Definition == nil {
		return errors.New("workflow action requires a versioned instance")
	}
	if err := config.Validate(execution.configuration); err != nil {
		return err
	}
	if snapshot.ConfigFingerprint != execution.configuration.Fingerprint() {
		return fmt.Errorf("%w: workflow %s was created under a different configuration", ErrConfigDrift, snapshot.WorkflowID)
	}
	definition, err := verificationBoundDefinition(execution.stateDir, execution.workspaceHash, snapshot)
	if err != nil {
		return err
	}
	if definition.Verification != nil {
		if err := requireIsolatedVerificationState(execution.workspacePath, execution.stateDir); err != nil {
			return err
		}
	}
	if err := validateWorkflowRuntime(definition, execution.configuration); err != nil {
		return err
	}
	execution.definition = &definition
	if err := execution.requireActiveTaskWorkflow(ctx, snapshot); err != nil {
		return err
	}
	_, err = execution.workflowStatusView(ctx, snapshot)
	return err
}

// lockWorkflowInstance 排斥同一实例的推进或控制，路径不采用客户端标识原文。
func lockWorkflowInstance(stateDir, hash string, id workflow.ID) (*lock.Lock, error) {
	digest := sha256.Sum256([]byte(id))
	directory := filepath.Join(filepath.Dir(workflowDBPath(stateDir, hash)), "workflow-locks")
	return lock.Acquire(filepath.Join(directory, fmt.Sprintf("%x.lock", digest)))
}

// workflowStepSpecs 复制定义中影响回放、依赖和返工的节点合同。
func workflowStepSpecs(definition WorkflowDefinition) []workflow.StepSpec {
	specs := make([]workflow.StepSpec, len(definition.Nodes))
	for index, node := range definition.Nodes {
		specs[index] = workflow.StepSpec{
			ID: node.ID, Index: index, Kind: node.Kind, MaxAttempts: node.MaxAttempts, Inputs: node.Inputs, RetryFrom: node.RetryFrom,
		}
	}
	return specs
}

// verificationBoundDefinition 重新校验定义制品及快照中的计划摘要。
func verificationBoundDefinition(stateDir, hash string, snapshot *workflow.Snapshot) (WorkflowDefinition, error) {
	definition, err := readWorkflowDefinitionArtifact(stateDir, hash, *snapshot.Definition)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	digest := snapshot.Definition.Artifact.SHA256
	fingerprint := snapshot.ConfigFingerprint
	if workflowPlanDigest(workflowStepSpecs(definition), digest, fingerprint) != snapshot.PlanSHA256 ||
		workflowPlanDigest(snapshot.Specs, digest, fingerprint) != snapshot.PlanSHA256 {
		return WorkflowDefinition{}, fmt.Errorf("%w: workflow instance plan mismatch", ErrStateCorrupt)
	}
	return definition, nil
}
