// Package app 装配面向 CLI 的 Cairn 运行、恢复与诊断用例。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/adapters/provider/openaicompat"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/lock"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

var (
	// ErrActiveRun 表示同一工作区已有尚未终态的运行。
	ErrActiveRun = errors.New("workspace already has an active run")
	// ErrWaitingReconcile 表示悬而未决的副作用必须由人工先对账。
	ErrWaitingReconcile = agent.ErrWaitingReconcile
	// ErrConfigDrift 表示当前配置与运行创建时冻结的配置不一致。
	ErrConfigDrift = errors.New("frozen config fingerprint does not match current configuration")
	// ErrStateCorrupt 表示事件日志或制品引用无法通过完整性校验。
	ErrStateCorrupt = errors.New("run state is corrupt")
)

// EventSink 接收已成功持久化、可供 CLI 流式输出的领域事件。
type EventSink func(domain.Event)

// Service 组合应用层用例所需的可替换 Provider 构造器。
type Service struct {
	NewProvider func(config.Config) (ports.Provider, error)
}

// RunOptions 描述启动一次全新 Agent 运行所需的输入与依赖。
type RunOptions struct {
	Task      string
	Workspace string
	StateDir  string
	Config    config.Config
	OnEvent   EventSink
}

// RunResult 返回运行标识、可呈现的最终回复，以及运行结束后从事件投影的
// 版本化产品合同 Result；投影失败时 Result 为 nil，但不掩盖运行本身的错误。
type RunResult struct {
	ID       domain.RunID
	Response string
	Result   *contract.Result
}

// ResumeOptions 描述恢复既有运行与可选审批决定所需的输入。
type ResumeOptions struct {
	RunID     domain.RunID
	Workspace string
	StateDir  string
	Config    config.Config
	Approval  agent.Approval
	OnEvent   EventSink
}

// ReconcileOptions 描述人工记录悬而未决副作用判定的输入。
type ReconcileOptions struct {
	RunID       domain.RunID
	OperationID domain.OperationID
	Verdict     string
	Note        string
	Evidence    string
	Workspace   string
	StateDir    string
	Config      config.Config
	OnEvent     EventSink
}

// AbortOptions 用于停止不存在不确定外部操作的运行；悬而未决的工作区或进程
// 操作必须以显式 abort_run 判定对账，保留证据边界。
type AbortOptions struct {
	RunID     domain.RunID
	Reason    string
	Workspace string
	StateDir  string
	Config    config.Config
	OnEvent   EventSink
}

// InspectResult 包含当前投影、诊断摘要、运行身份 manifest 以及按需附带的
// 完整事件列表；人类视图与 --json 机器视图来自同一投影。
type InspectResult struct {
	Projection domain.RunProjection  `json:"projection"`
	Summary    RunSummary            `json:"summary"`
	Manifest   *contract.RunManifest `json:"manifest,omitempty"`
	Events     []domain.Event        `json:"events,omitempty"`
}

// Run 初始化事件日志、锁、工具和策略后启动一次新的 Agent Loop；返回前关闭
// Boot，关闭错误与执行错误一并合并上报，绝不静默丢弃。
func (service Service) Run(ctx context.Context, options RunOptions) (outcome RunResult, err error) {
	if options.Task == "" {
		return RunResult{}, errors.New("task is required")
	}
	boot, err := service.assembleBoot(options.Workspace, options.StateDir, options.Config, options.OnEvent)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	active, err := boot.Journal.NonTerminalRuns(ctx, boot.WorkspaceHash)
	if err != nil {
		return RunResult{}, err
	}
	if len(active) != 0 {
		switch active[0].Phase() {
		case "waiting_approval":
			pending := active[0].PendingApproval
			if pending != nil {
				return RunResult{}, fmt.Errorf("%w: %w", ErrActiveRun, &agent.WaitingApprovalError{RunID: active[0].ID, OperationID: pending.OperationID, Fingerprint: pending.Fingerprint, Summary: pending.Summary})
			}
		case "waiting_reconcile":
			return RunResult{}, fmt.Errorf("%w: %w", ErrActiveRun, ErrWaitingReconcile)
		case "created", "cancelling":
			// 进程在 run_created/run_started 之间或取消序列中途崩溃留下的非终态运行：
			// 此状态下没有可恢复的执行语义，唯一出口是补写终态解锁工作区。
			return RunResult{}, fmt.Errorf("%w: run %s is %s after an interrupted lifecycle; finish it with resume %s --abort-run --reason <text>", ErrActiveRun, active[0].ID, active[0].Phase(), active[0].ID)
		}
		return RunResult{}, fmt.Errorf("%w: resume %s", ErrActiveRun, active[0].ID)
	}
	frozenConfig, err := json.Marshal(options.Config)
	if err != nil {
		return RunResult{}, fmt.Errorf("encode frozen run config: %w", err)
	}
	configRef, err := boot.Artifacts.Put(artifact.RedactJSON(frozenConfig), "application/json")
	if err != nil {
		return RunResult{}, fmt.Errorf("store frozen run config: %w", err)
	}
	runID := domain.NewRunID()
	first, err := boot.Journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunCreated, Payload: encode(struct {
		domain.RunCreatedPayload
		ConfigRef artifact.Ref `json:"config_ref"`
	}{RunCreatedPayload: domain.RunCreatedPayload{Task: options.Task, Workspace: boot.Workspace.Root, ConfigFingerprint: options.Config.Fingerprint(), Build: buildIdentityPtr()}, ConfigRef: configRef})})
	if err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(first)
	}
	second, err := boot.Journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunStarted, Payload: encode(struct {
		Invocation string `json:"invocation"`
	}{Invocation: domain.NewID("invoke")})})
	if err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(second)
	}
	runContext, cancelRun, err := runBudgetContext(ctx, first.OccurredAt, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	defer cancelRun()
	result, err := boot.Loop.Run(runContext, agent.RunInput{RunID: runID, Task: options.Task, WorkspaceHash: boot.WorkspaceHash})
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = boot.Loop.Run(expiredContext, agent.RunInput{RunID: runID, Task: options.Task, WorkspaceHash: boot.WorkspaceHash})
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, runID, result.Response, err)
}

// contractResultOrNil 在投影失败时返回 nil 而不是零值，避免把半成品合同
// 误当有效结果；调用方仍以原始运行错误为首要上报事实。
func contractResultOrNil(result contract.Result, err error) *contract.Result {
	if err != nil {
		return nil
	}
	return &result
}

// projectedRunOutcome 从当前事件日志投影一次产品合同，并把它附加到 Run、
// Resume、Reconcile 与 Abort 的统一返回值。执行错误永远优先返回；若运行
// 已成功但投影本身失败，则保留成功事实并省略 Result，供 inspect/evidence
// 继续诊断，避免把投影层故障伪装成执行失败。
func projectedRunOutcome(journal *sqlite.Journal, stateDir, workspaceHash string, runID domain.RunID, response string, runErr error) (RunResult, error) {
	result, projectionErr := projectRunResult(context.Background(), journal, stateDir, workspaceHash, runID)
	outcome := RunResult{ID: runID, Response: response, Result: contractResultOrNil(result, projectionErr)}
	if runErr != nil {
		return outcome, runErr
	}
	return outcome, nil
}

// Inspect 只读加载指定运行，验证完整性后返回投影与可选事件历史。
func (service Service) Inspect(ctx context.Context, workspacePath, stateDir string, config config.Config, runID domain.RunID, includeEvents bool) (InspectResult, error) {
	_, hash, err := workspaceFor(workspacePath, config)
	if err != nil {
		return InspectResult{}, err
	}
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		return InspectResult{}, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return InspectResult{}, err
	}
	if len(events) == 0 {
		return InspectResult{}, fmt.Errorf("run %s not found", runID)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkArtifactReferences(ctx, journal, stateDir, hash); err != nil {
		return InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	result := InspectResult{Projection: projection, Summary: SummarizeRun(events, projection)}
	if manifest, manifestErr := runManifest(events, projection, stateDir, hash); manifestErr == nil {
		result.Manifest = &manifest
	} else {
		return InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, manifestErr)
	}
	if includeEvents {
		result.Events = events
	}
	return result, nil
}

// Evidence 返回一次运行的统一证据视图（V1.5）：以 contract.Result 为唯一
// 载体，从事件与冻结配置制品回放任务身份、运行身份、Provider 卡、终局、
// 失败分类与完成证据。只读，不依赖当前配置漂移与否。
func (service Service) Evidence(ctx context.Context, workspacePath, stateDir string, configuration config.Config, runID domain.RunID) (contract.Result, error) {
	_, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return contract.Result{}, err
	}
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		return contract.Result{}, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return contract.Result{}, err
	}
	if len(events) == 0 {
		return contract.Result{}, fmt.Errorf("run %s not found", runID)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return contract.Result{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkRunArtifactReferences(events, stateDir, hash); err != nil {
		return contract.Result{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	frozenConfig, err := readFrozenRunConfig(events, stateDir, hash)
	if err != nil {
		return contract.Result{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	result, err := contract.ResultFromRun(events, projection, contract.FreeformTaskSpec(projection.Task), frozenConfig)
	if err != nil {
		return contract.Result{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return result, nil
}

// Resume 在校验冻结配置、日志和制品后恢复等待审批或普通运行；返回前关闭
// Boot，关闭错误与执行错误一并合并上报，绝不静默丢弃。
func (service Service) Resume(ctx context.Context, options ResumeOptions) (outcome RunResult, err error) {
	legacyCapability, err := legacyProviderCapabilityResume(options.Workspace, options.StateDir, options.Config, options.RunID)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	boot, err := service.assembleBootWithLegacyProviderCapability(options.Workspace, options.StateDir, options.Config, options.OnEvent, legacyCapability)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	events, err := boot.Journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.State != domain.RunRunning {
		return RunResult{ID: options.RunID}, errors.New("run is not resumable")
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkArtifactReferences(ctx, boot.Journal, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: use --resolve %s=<verdict>", ErrWaitingReconcile, projection.PendingOperation.ID)
	}
	observation, err := boot.Workspace.ObservationView(options.Config.Verification.VolatilePaths).Observation()
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	defer cancelRun()
	resumeInput := agent.RunInput{RunID: options.RunID, WorkspaceHash: boot.WorkspaceHash, ResumeInvocation: domain.NewID("invoke"), ResumeFromSeq: projection.LastSeq, ResumeObservation: observation}
	result, err := boot.Loop.Resume(runContext, resumeInput, options.Approval)
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		resumeInput.ResumeInvocation = ""
		result, err = boot.Loop.Resume(expiredContext, resumeInput, options.Approval)
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, result.Response, err)
}

// Reconcile 为已开始但未持久化终态的外部操作写入人工判定；它绝不重新执行原命令或 patch。
// 返回前关闭 Boot，关闭错误与执行错误一并合并上报，绝不静默丢弃。
func (service Service) Reconcile(ctx context.Context, options ReconcileOptions) (outcome RunResult, err error) {
	legacyCapability, err := legacyProviderCapabilityResume(options.Workspace, options.StateDir, options.Config, options.RunID)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	boot, err := service.assembleBootWithLegacyProviderCapability(options.Workspace, options.StateDir, options.Config, options.OnEvent, legacyCapability)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	events, err := boot.Journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkArtifactReferences(ctx, boot.Journal, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.State != domain.RunRunning || projection.PendingOperation == nil || projection.PendingOperation.ID != options.OperationID {
		return RunResult{}, ErrWaitingReconcile
	}
	if projection.PendingOperation.SideEffectClass != "workspace" && projection.PendingOperation.SideEffectClass != "process" {
		return RunResult{}, errors.New("only workspace and process operations require reconciliation")
	}
	switch options.Verdict {
	case "applied_success", "applied_failure", "not_applied", "abort_run":
	default:
		return RunResult{}, errors.New("verdict must be applied_success, applied_failure, not_applied, or abort_run")
	}
	if strings.TrimSpace(options.Note) == "" {
		return RunResult{}, errors.New("reconciliation note is required")
	}
	if projection.PendingOperation.SideEffectClass == "process" && strings.TrimSpace(options.Evidence) == "" {
		return RunResult{}, errors.New("process reconciliation requires evidence")
	}
	if projection.PendingOperation.ToolName == "apply_patch" && projection.PendingOperation.PlannedPath != "" {
		observation, err := patchObservation(boot.Workspace, projection.PendingOperation.PlannedPath)
		if err != nil {
			return RunResult{}, err
		}
		switch options.Verdict {
		case "applied_success":
			if observation != projection.PendingOperation.PlannedAfter {
				return RunResult{}, fmt.Errorf("patch observation %s does not match planned after hash %s", observation, projection.PendingOperation.PlannedAfter)
			}
		case "not_applied":
			if observation != projection.PendingOperation.PlannedBefore {
				return RunResult{}, fmt.Errorf("patch observation %s does not match planned before hash %s", observation, projection.PendingOperation.PlannedBefore)
			}
		}
	}
	operationID := options.OperationID
	turnID := projection.CurrentTurn
	if turnID == nil {
		return RunResult{}, errors.New("dangling operation has no open turn")
	}
	// 对账事件的观察值与 resume 一致采用真实工作区摘要，而不是误导性的根路径。
	observation, err := boot.Workspace.ObservationView(options.Config.Verification.VolatilePaths).Observation()
	if err != nil {
		return RunResult{}, err
	}
	appendEvent := func(eventType domain.EventType, payload any) error {
		draft := domain.EventDraft{RunID: options.RunID, WorkspaceHash: boot.WorkspaceHash, Type: eventType, TurnID: turnID, OperationID: &operationID, Payload: encode(payload)}
		// 写入前先在克隆投影上模拟迁移，避免把不可回放的事件永久写入日志。
		if err := projection.ValidateDraft(draft); err != nil {
			return fmt.Errorf("validate %s after sequence %d: %w", eventType, projection.LastSeq, err)
		}
		event, err := boot.Journal.Append(ctx, projection.LastSeq, draft)
		if err != nil {
			return err
		}
		if err := projection.Apply(event); err != nil {
			return err
		}
		if options.OnEvent != nil {
			options.OnEvent(event)
		}
		return nil
	}
	if err := appendEvent(domain.EventOperationReconciled, struct {
		Verdict              string `json:"verdict"`
		Actor                string `json:"actor"`
		Note                 string `json:"note"`
		Evidence             string `json:"evidence"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Verdict: options.Verdict, Actor: "human", Note: options.Note, Evidence: options.Evidence, WorkspaceObservation: observation}); err != nil {
		return RunResult{}, err
	}
	if options.Verdict == "abort_run" {
		if err := appendEvent(domain.EventTurnClosed, domain.TurnClosedPayload{Outcome: "failed"}); err != nil {
			return RunResult{}, err
		}
		if err := appendEvent(domain.EventRunFailed, struct {
			Error string `json:"error"`
		}{Error: "reconciliation aborted"}); err != nil {
			return RunResult{}, err
		}
		return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, "", nil)
	}
	if err := appendEvent(domain.EventToolResultAppended, tools.Result{OK: options.Verdict == "applied_success", Code: "reconciled_" + options.Verdict, Preview: options.Note}); err != nil {
		return RunResult{}, err
	}
	if err := appendEvent(domain.EventTurnClosed, domain.TurnClosedPayload{Outcome: "continue"}); err != nil {
		return RunResult{}, err
	}
	// 对账本身不执行任何副作用，重算观察值以绑定恢复时刻的真实工作区状态。
	observation, err = boot.Workspace.ObservationView(options.Config.Verification.VolatilePaths).Observation()
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	resumedDraft := domain.EventDraft{RunID: options.RunID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunResumed, Payload: encode(struct {
		Invocation           string `json:"invocation"`
		FromSeq              uint64 `json:"from_seq"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Invocation: domain.NewID("invoke"), FromSeq: projection.LastSeq, WorkspaceObservation: observation})}
	if err := projection.ValidateDraft(resumedDraft); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("validate run_resumed after sequence %d: %w", projection.LastSeq, err)
	}
	resumed, err := boot.Journal.Append(ctx, projection.LastSeq, resumedDraft)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := projection.Apply(resumed); err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(resumed)
	}
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	defer cancelRun()
	result, err := boot.Loop.Run(runContext, agent.RunInput{RunID: options.RunID, Task: projection.Task, WorkspaceHash: boot.WorkspaceHash})
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = boot.Loop.Run(expiredContext, agent.RunInput{RunID: options.RunID, Task: projection.Task, WorkspaceHash: boot.WorkspaceHash})
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, result.Response, err)
}

// Abort 在不启动模型、工具或 verifier 的情况下终止非悬挂运行；已经开始的副作用
// 所具有的不确定性只能通过 Reconcile 的判定解决。除 running 外，它也是 created 与
// cancelling 运行的唯一恢复出口：进程在 run_created/run_started 之间或取消序列中途
// 崩溃后，工作区会被非终态 run 永久阻塞，此处按状态机允许的起点补写终态。
func (service Service) Abort(ctx context.Context, options AbortOptions) (RunResult, error) {
	if strings.TrimSpace(options.Reason) == "" {
		return RunResult{}, errors.New("abort reason is required")
	}
	_, hash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	journal, err := openJournal(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer journal.Close()
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer workspaceLock.Release()
	events, err := journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	switch projection.State {
	case domain.RunCreated, domain.RunRunning, domain.RunCancelling:
	default:
		return RunResult{ID: options.RunID}, errors.New("run is not abortable")
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkArtifactReferences(ctx, journal, options.StateDir, hash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.PendingOperation != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: use --resolve %s=abort_run", ErrWaitingReconcile, projection.PendingOperation.ID)
	}
	appendEvent := func(eventType domain.EventType, turnID *domain.TurnID, operationID *domain.OperationID, payload any) error {
		draft := domain.EventDraft{RunID: options.RunID, WorkspaceHash: hash, Type: eventType, TurnID: turnID, OperationID: operationID, Payload: encode(payload)}
		// 与 Reconcile 相同：先在克隆投影上模拟迁移再写入，防止不可回放事件落盘。
		if err := projection.ValidateDraft(draft); err != nil {
			return fmt.Errorf("validate %s after sequence %d: %w", eventType, projection.LastSeq, err)
		}
		event, err := journal.Append(ctx, projection.LastSeq, draft)
		if err != nil {
			return err
		}
		if err := projection.Apply(event); err != nil {
			return err
		}
		if options.OnEvent != nil {
			options.OnEvent(event)
		}
		return nil
	}
	if pending := projection.PendingApproval; pending != nil {
		if projection.CurrentTurn == nil {
			return RunResult{}, errors.New("pending approval has no open turn")
		}
		turnID, operationID := *projection.CurrentTurn, pending.OperationID
		if err := appendEvent(domain.EventApprovalDenied, &turnID, &operationID, struct {
			Fingerprint string `json:"fingerprint"`
			Actor       string `json:"actor"`
			Reason      string `json:"reason"`
		}{Fingerprint: pending.Fingerprint, Actor: "system", Reason: options.Reason}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
	}
	if projection.CurrentTurn != nil {
		turnID := *projection.CurrentTurn
		if err := appendEvent(domain.EventTurnClosed, &turnID, nil, domain.TurnClosedPayload{Outcome: "failed"}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
	}
	// cancelling 运行的取消意图已持久化，补写 run_cancelled 完成该序列；
	// created/running 运行按中止语义写 run_failed（两类起点均为状态机允许）。
	if projection.State == domain.RunCancelling {
		if err := appendEvent(domain.EventRunCancelled, nil, nil, struct {
			Cause string `json:"cause"`
		}{Cause: "user_interrupt"}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
		return projectedRunOutcome(journal, options.StateDir, hash, options.RunID, "", nil)
	}
	if err := appendEvent(domain.EventRunFailed, nil, nil, struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}{Error: "user_aborted", Reason: options.Reason}); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	return projectedRunOutcome(journal, options.StateDir, hash, options.RunID, "", nil)
}

// provider 使用注入构造器或默认 OpenAI-compatible 适配器创建模型端口。能力
// 卡声明的 usage 与 stop reason 合同随适配器下发，由 Provider 层在每次流结束
// 时失败关闭校验；显式生成参数原样透传，不在此层引入默认值。
func (service Service) provider(configuration config.Config) (ports.Provider, error) {
	if service.NewProvider != nil {
		return service.NewProvider(configuration)
	}
	if configuration.Provider.BaseURL == "" || configuration.Provider.Model == "" {
		return nil, errors.New("provider base URL and model are required")
	}
	timeout, err := configuration.RequestTimeout()
	if err != nil {
		return nil, err
	}
	card, err := contract.ResolveProviderCard(contract.ProviderKindOpenAICompatible, configuration.Provider.Model, config.ProviderCapabilityDeclaration(configuration))
	if err != nil {
		return nil, fmt.Errorf("resolve provider card for stream requirements: %w", err)
	}
	client := openaicompat.Client{
		BaseURL: configuration.Provider.BaseURL, APIKey: os.Getenv(configuration.Provider.APIKeyEnv),
		HTTPClient: &http.Client{Timeout: timeout},
		Requirements: openaicompat.Requirements{
			RequireUsage:       card.StreamingUsage,
			AllowedStopReasons: append([]string(nil), card.StopReasons...),
		},
	}
	if generation := configuration.Provider.Generation; generation != nil {
		client.Generation = &openaicompat.GenerationOptions{
			TemperatureMilli: generation.TemperatureMilli,
			TopPMilli:        generation.TopPMilli, ReasoningEffort: generation.ReasoningEffort, ToolChoice: generation.ToolChoice,
		}
		if generation.MaxOutputTokens != nil {
			client.Generation.MaxOutputTokens = *generation.MaxOutputTokens
		}
	}
	return client, nil
}

// workspaceFor 创建受限工作区视图，并以其规范根路径计算稳定工作区哈希。
func workspaceFor(root string, configuration config.Config) (tools.Workspace, string, error) {
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return tools.Workspace{}, "", err
		}
		root = current
	}
	workspace, err := tools.NewWorkspace(root, configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
	if err != nil {
		return tools.Workspace{}, "", err
	}
	digest := sha256.Sum256([]byte(workspace.Root))
	return workspace, "sha256:" + hex.EncodeToString(digest[:]), nil
}

// execToolFor 以同一份冻结配置装配 Run、Resume、Reconcile 与 Doctor 使用的
// 命令工具；Docker 后端只携带显式镜像和有限 PID 配额。
func execToolFor(workspace tools.Workspace, configuration config.Config) (tools.ExecCommand, error) {
	if err := config.Validate(configuration); err != nil {
		return tools.ExecCommand{}, err
	}
	environmentAllow := make(map[string]struct{}, len(configuration.Policy.EnvAllow))
	for _, key := range configuration.Policy.EnvAllow {
		environmentAllow[key] = struct{}{}
	}
	grace, err := configuration.TerminateGrace()
	if err != nil {
		return tools.ExecCommand{}, err
	}
	return tools.ExecCommand{
		Workspace:     workspace,
		EnvAllow:      environmentAllow,
		Grace:         grace,
		Backend:       tools.ExecBackend(configuration.Sandbox.Backend),
		DockerImage:   configuration.Sandbox.DockerImage,
		DockerMaxPIDs: configuration.Sandbox.MaxPIDs,
	}, nil
}

// openJournal 打开指定工作区的可写 SQLite 事件日志。
func openJournal(stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.Open(filepath.Join(directory, "runs.db"))
}

// openArtifactStore 打开指定工作区的脱敏内容寻址制品库。
func openArtifactStore(stateDir, workspaceHash string, output config.OutputConfig) (*artifact.Store, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	store, err := artifact.New(filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts"), output.MaxPreviewBytes)
	if err != nil {
		return nil, err
	}
	store.MaxBytes = output.MaxArtifactBytes
	return store, nil
}

// checkArtifactReferences 验证日志 payload 引用的每一个制品文件及其摘要。
func checkArtifactReferences(ctx context.Context, journal *sqlite.Journal, stateDir, workspaceHash string) error {
	runIDs, err := journal.RunIDs(ctx)
	if err != nil {
		return err
	}
	for _, runID := range runIDs {
		events, err := journal.Load(ctx, runID)
		if err != nil {
			return err
		}
		if err := checkRunArtifactReferences(events, stateDir, workspaceHash); err != nil {
			return err
		}
	}
	return nil
}

// checkRunArtifactReferences 校验单次 Run 的所有制品引用。Evidence 只能读取
// 指定 Run，不能因其他 Run 的损坏而失去可用性；诊断则复用本函数遍历全部 Run。
func checkRunArtifactReferences(events []domain.Event, stateDir, workspaceHash string) error {
	root := manifestArtifactRoot(stateDir, workspaceHash)
	for _, event := range events {
		var payload struct {
			ResultRef   *artifact.Ref `json:"result_ref"`
			PlanRef     *artifact.Ref `json:"plan_ref"`
			ConfigRef   *artifact.Ref `json:"config_ref"`
			EvidenceRef *artifact.Ref `json:"evidence_ref"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		// 验证计划与冻结配置是审批绑定的信任根，与结果制品同等纳入
		// 存在性与摘要校验，防止被删除或篡改后静默通过诊断。
		for _, reference := range []*artifact.Ref{payload.ResultRef, payload.PlanRef, payload.ConfigRef, payload.EvidenceRef} {
			if reference == nil {
				continue
			}
			if err := checkArtifactReference(root, *reference); err != nil {
				return fmt.Errorf("run %s event %d: %w", event.RunID, event.Seq, err)
			}
		}
	}
	return nil
}

// manifestArtifactRoot 返回指定工作区制品库的根目录；与 openArtifactStore
// 使用同一路径布局，保证投影读取与写入视角一致。
func manifestArtifactRoot(stateDir, workspaceHash string) string {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts")
}

// checkArtifactReference 验证单个制品引用的格式、存在性和内容摘要。
func checkArtifactReference(root string, reference artifact.Ref) error {
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("invalid artifact digest %q", reference.SHA256)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("decode artifact digest: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil {
		return fmt.Errorf("read referenced artifact: %w", err)
	}
	actual := sha256.Sum256(content)
	if hex.EncodeToString(actual[:]) != digest {
		return errors.New("referenced artifact hash mismatch")
	}
	return nil
}

// openReadOnlyJournal 打开指定工作区的不可写 SQLite 事件日志。
func openReadOnlyJournal(stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.OpenReadOnly(filepath.Join(directory, "runs.db"))
}

// acquireWorkspaceLock 为指定工作区获取进程级排他锁。
func acquireWorkspaceLock(stateDir, workspaceHash string) (*lock.Lock, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return lock.Acquire(filepath.Join(stateDir, "workspaces", workspaceHash, "run.lock"))
}

// DefaultStateDir 返回平台默认的 Cairn 状态目录；空 stateDir 参数的装配路径
// 统一回退到此值，导出供 CLI 命令复用同一解析。
func DefaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".cairn"
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Cairn")
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "cairn")
	}
	return filepath.Join(home, ".local", "state", "cairn")
}

// encode 将事件 payload 序列化并在写入前执行 JSON 脱敏。
func encode(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return append(json.RawMessage(nil), artifact.RedactJSON(encoded)...)
}

// requireMatchingConfig 确保当前运行配置与创建事件中冻结的指纹一致。
func requireMatchingConfig(events []domain.Event, configuration config.Config) error {
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return errors.New("run is missing run_created event")
	}
	var payload struct {
		ConfigFingerprint string `json:"config_fingerprint"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		return fmt.Errorf("decode frozen run config: %w", err)
	}
	if payload.ConfigFingerprint == "" || payload.ConfigFingerprint != configuration.Fingerprint() {
		return ErrConfigDrift
	}
	return nil
}

// legacyProviderCapabilityResume 只识别一类可逆兼容情形：当前配置与已冻结
// 配置都没有 provider.capability，且冻结制品通过完整性校验。能力卡是在这类
// 历史 Run 创建之后才引入的合同，Resume/Reconcile 可在不伪造新卡的前提下按
// 原语义收尾；新 Run、未知制品、篡改制品或任何显式能力配置都绝不走此路径。
func legacyProviderCapabilityResume(workspacePath, stateDir string, configuration config.Config, runID domain.RunID) (bool, error) {
	if configuration.Provider.Capability != nil {
		return false, nil
	}
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return false, err
	}
	journal, err := openReadOnlyJournal(stateDir, workspaceHash)
	if err != nil {
		return false, nil
	}
	defer journal.Close()
	events, err := journal.Load(context.Background(), runID)
	if err != nil || len(events) == 0 {
		return false, nil
	}
	frozenConfig, err := readFrozenRunConfig(events, stateDir, workspaceHash)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if len(frozenConfig) == 0 {
		return false, nil
	}
	var frozen struct {
		Provider struct {
			Capability *json.RawMessage `json:"Capability"`
		} `json:"Provider"`
	}
	if err := json.Unmarshal(frozenConfig, &frozen); err != nil {
		return false, fmt.Errorf("%w: decode frozen provider capability: %v", ErrStateCorrupt, err)
	}
	return frozen.Provider.Capability == nil, nil
}

// patchObservation 返回目标文件当前摘要或 absent，用于人工 patch 对账。
func patchObservation(workspace tools.Workspace, relativePath string) (string, error) {
	path, _, err := workspace.Resolve(relativePath)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// verificationCommands 将配置中的固定验收命令转换为 Agent 可执行计划。
func verificationCommands(configuration config.VerificationConfig) ([]agent.VerificationCommand, error) {
	commands := make([]agent.VerificationCommand, 0, len(configuration.Commands))
	for index, command := range configuration.Commands {
		timeout := 120 * time.Second
		if command.Timeout != "" {
			parsed, err := time.ParseDuration(command.Timeout)
			if err != nil {
				return nil, err
			}
			timeout = parsed
		}
		maximumOutput := command.MaxOutputBytes
		if maximumOutput == 0 {
			maximumOutput = 256 * 1024
		}
		arguments, err := json.Marshal(tools.ExecArguments{Argv: command.Argv, CWD: command.CWD, TimeoutMS: int(timeout.Milliseconds()), MaxOutputBytes: maximumOutput, Env: command.Env})
		if err != nil {
			return nil, err
		}
		commands = append(commands, agent.VerificationCommand{ID: fmt.Sprintf("verification-%d", index+1), Arguments: arguments})
	}
	return commands, nil
}

// configuredLoop 将应用配置与基础设施依赖装配为一个 Agent Loop。
func configuredLoop(journal ports.Journal, provider ports.Provider, registry *tools.Registry, gate *policy.Gate, verification []agent.VerificationCommand, artifacts *artifact.Store, configuration config.Config, onEvent EventSink) agent.Loop {
	return agent.Loop{
		Journal: journal, Provider: provider, Tools: registry, Gate: gate,
		Model: configuration.Provider.Model, ConfigFingerprint: configuration.Fingerprint(),
		MaxTurns: configuration.Budget.MaxTurns, MaxToolCalls: configuration.Budget.MaxToolCalls,
		MaxInputTokens: configuration.Budget.MaxInputTokens, MaxOutputTokens: configuration.Budget.MaxOutputTokens,
		MaxCostUSD: configuration.Budget.MaxCostUSD, InputTokenUSD: configuration.Pricing.InputTokenUSD,
		OutputTokenUSD:     configuration.Pricing.OutputTokenUSD,
		ContextAutoCompact: configuration.Context.AutoCompact, ContextMaxPromptTokens: configuration.Context.MaxPromptTokens,
		ContextMaxSummaryBytes:         configuration.Context.MaxSummaryBytes,
		ContextInventoryBudgetPercent:  configuration.Context.InventoryBudgetPercent,
		ContextThresholdReservePercent: configuration.Context.ThresholdReservePercent,
		VerificationAutoAfterPatch:     configuration.Verification.AutoAfterPatch,
		Verification:                   verification, VerificationWorkspace: gate.Patch.Workspace.ObservationView(configuration.Verification.VolatilePaths),
		Artifacts: artifacts, OnEvent: agent.EventSink(onEvent),
	}
}

// runBudgetContext 将运行创建时刻与墙钟预算组合为统一截止上下文。
func runBudgetContext(ctx context.Context, createdAt time.Time, configuration config.Config) (context.Context, context.CancelFunc, error) {
	wallTime, err := configuration.WallTime()
	if err != nil {
		return nil, nil, err
	}
	budgetContext, cancel := context.WithDeadline(ctx, createdAt.Add(wallTime))
	return budgetContext, cancel, nil
}

// expiredWallBudgetContext 返回已过期上下文，供持久化墙钟预算耗尽事件使用。
func expiredWallBudgetContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, time.Now().Add(-time.Nanosecond))
}
