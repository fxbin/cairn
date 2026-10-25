// Package app 装配面向 CLI 的 Cairn 运行、恢复与诊断用例。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

var (
	// ErrActiveRun 表示同一工作区已有尚未终态的运行。
	ErrActiveRun = errors.New("workspace already has an active run")
	// ErrWaitingReconcile 表示悬而未决的副作用必须由人工先对账。
	ErrWaitingReconcile = ports.ErrWaitingReconcile
	// ErrConfigDrift 表示当前配置与运行创建时冻结的配置不一致。
	ErrConfigDrift = errors.New("frozen config fingerprint does not match current configuration")
	// ErrStateCorrupt 表示事件日志或制品引用无法通过完整性校验。
	ErrStateCorrupt = errors.New("run state is corrupt")
	// ErrSessionContinuation 表示所选历史运行不满足安全续链条件。
	ErrSessionContinuation = errors.New("historical run is not a safe session continuation point")
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
	// DisableUserInstructions 供受控评测排除宿主用户规约；项目规约与历史快照仍保持原合同。
	DisableUserInstructions bool
	// 会话链关系（ADR-0018）：显式携带时写入创建事件并注入前情；空值时
	// 行为与无会话场景完全一致。前情文本与制品引用由调用方成对提供，或
	// 由应用层从上一 run 生成（前情卡服务）。
	SessionID       string
	PrevRunID       domain.RunID
	PriorContext    string
	PriorContextRef *artifact.Ref
	// ForkSource 指定从历史终态 run 创建新会话分支；与 PrevRunID 互斥。
	// 设置时生成（或沿用未占用的）新 SessionID，根运行写入独立来源锚点。
	ForkSource *domain.SessionForkSource
	// Steering 是安全点 steering 投递桥；nil 表示本次运行不接受补充输入。
	Steering *agent.SteeringBridge
	// PreassignedRunID 可选预分配 run 标识；队列确认用它保证 consumed{linked}
	// 与 run_created 的跨崩溃一致性。空值时由系统生成。
	PreassignedRunID domain.RunID
	// RequireSessionTail 要求 PrevRunID 在创建事件前仍是其完整历史会话的
	// 唯一终态链尾；历史选择器用它封闭“展示后状态已变化”的竞态。
	RequireSessionTail bool
	// ParentRunID 标记本次运行为另一 run 的子运行；写入 run_created 的
	// parent_run_id，不改变会话续链、审批或预算语义。
	ParentRunID domain.RunID
	// ParentTools 是父运行模型可见工具名集合；设置 ParentRunID 时用于强制
	// 子工具面收窄（child ⊆ parent）。空值时回退为按父冻结 Preset 重建的
	// 基础工具面（不含 opt-in 扩展），保持失败关闭。
	ParentTools []string
}

// RunResult 返回运行标识、可呈现的最终回复，以及运行结束后从事件投影的
// 版本化产品合同 Result；投影失败时 Result 为 nil，但不掩盖运行本身的错误。
type RunResult struct {
	ID       domain.RunID
	Response string
	// SessionID 是本次运行的会话标识；空值表示无会话（如队列任务），
	// 调用方不得将其接入当前会话续链。
	SessionID string
	Result    *contract.Result
}

// ResumeOptions 描述恢复既有运行与可选审批决定所需的输入。
type ResumeOptions struct {
	RunID     domain.RunID
	Workspace string
	StateDir  string
	Config    config.Config
	Approval  ports.Approval
	OnEvent   EventSink
	// Steering 在恢复执行时继续接受安全点补充输入，不继承历史未消费输入。
	Steering *agent.SteeringBridge
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

// RunHistoryItem 是会话选择器使用的只读运行摘要，不暴露工作区路径、审批
// 指纹或操作细节；时间与会话关系均来自事件回放。
type RunHistoryItem struct {
	ID        domain.RunID `json:"run_id"`
	SessionID string       `json:"session_id,omitempty"`
	PrevRunID domain.RunID `json:"prev_run_id,omitempty"`
	// ForkSource 是新会话分支根指向的历史终态 run 锚点；仅根运行可携带。
	ForkSource *domain.SessionForkSource `json:"fork_source,omitempty"`
	Task       string                    `json:"task"`
	State      domain.RunState           `json:"state"`
	Phase      string                    `json:"phase"`
	CreatedAt  time.Time                 `json:"created_at"`
	UpdatedAt  time.Time                 `json:"updated_at"`
}

// SessionContinuation 是历史会话选择器返回的下一次运行锚点；它只恢复会话
// 标识和唯一终态链尾，不授予历史回退能力。
type SessionContinuation struct {
	SessionID string       `json:"session_id"`
	PrevRunID domain.RunID `json:"prev_run_id"`
}

// SessionForkAnchor 是已校验的分支创建锚点：只提供创建新会话根运行所需的
// 来源引用，不授予历史回退能力，也不在选择阶段留下幽灵运行。
type SessionForkAnchor struct {
	Source          domain.SessionForkSource `json:"source"`
	SourceSessionID string                   `json:"source_session_id"`
	SourceState     domain.RunState          `json:"source_state"`
	SourceTask      string                   `json:"source_task"`
}

// SessionTreeNode 是会话树投影中的单个 run 节点；Children 只含同 session 内
// 经 prev_run_id 的直接后继，跨 session 分支由 ForkSource 边表达。
type SessionTreeNode struct {
	ID         domain.RunID              `json:"run_id"`
	SessionID  string                    `json:"session_id,omitempty"`
	PrevRunID  domain.RunID              `json:"prev_run_id,omitempty"`
	ForkSource *domain.SessionForkSource `json:"fork_source,omitempty"`
	Task       string                    `json:"task"`
	State      domain.RunState           `json:"state"`
	Phase      string                    `json:"phase"`
	CreatedAt  time.Time                 `json:"created_at"`
	UpdatedAt  time.Time                 `json:"updated_at"`
	Children   []domain.RunID            `json:"children,omitempty"`
}

// SessionTree 是当前工作区的只读会话树投影；节点按 run 标识索引，Roots 按
// 创建时间升序排列各 session 根。索引可重建，事实源仍是 Event Journal。
type SessionTree struct {
	Nodes map[domain.RunID]SessionTreeNode `json:"nodes"`
	Roots []domain.RunID                   `json:"roots"`
}

// Run 初始化事件日志、锁、工具和策略后启动一次新的 Agent Loop；返回前关闭
func (service Service) Run(ctx context.Context, options RunOptions) (outcome RunResult, err error) {
	if options.Task == "" {
		return RunResult{}, errors.New("task is required")
	}
	if options.ForkSource != nil && options.PrevRunID != "" {
		return RunResult{}, fmt.Errorf("%w: fork_source is mutually exclusive with prev_run_id", ErrSessionContinuation)
	}
	build, err := currentBuildIdentity()
	if err != nil {
		return RunResult{}, err
	}
	var boot *Boot
	if options.ParentRunID != "" {
		// 子运行：家族共享锁路径，父 Boot 在同进程持锁时不再占第二把排他锁。
		boot, err = service.assembleBootWithLock(options.Workspace, options.StateDir, options.Config, options.OnEvent, true)
	} else {
		boot, err = service.assembleBoot(options.Workspace, options.StateDir, options.Config, options.OnEvent)
	}
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	// 嵌套运行恢复：委派回调会在本 Run 内嵌套子 Run，子 Run 会覆盖进程级
	// 回调绑定；先保存外层绑定并在返回时恢复，保证父 Loop 后续 send_message
	// 与 memory_recall 仍指向父 Boot 的存活资源。
	previousDeliver := tools.CurrentMessageDeliver()
	previousExplain := tools.CurrentMemoryExplain()
	defer tools.BindMessageDeliver(previousDeliver)
	defer tools.BindMemoryExplain(previousExplain)
	// send_message 投递：写入对端 run Journal（父/子跟进）。
	tools.BindMessageDeliver(func(ctx context.Context, toRunID, content, reason string) error {
		return deliverRunMessage(ctx, boot.Journal, boot.Artifacts, boot.WorkspaceHash, domain.RunID(toRunID), content, reason)
	})
	// memory_recall：只读证据账本；服务端强制工作区，模型无法写入或跨区检索。
	tools.BindMemoryExplain(func(ctx context.Context, request tools.MemoryRecallRequest) (json.RawMessage, error) {
		ledger, err := service.ExplainMemoryRecall(ctx, MemoryExplainOptions{
			Query: memory.RecallQuery{
				TaskPromptDigest:    request.TaskPromptDigest,
				Outcomes:            request.Outcomes,
				FailureClasses:      request.FailureClasses,
				TrustedVerification: request.TrustedVerification,
				CreatedFrom:         request.CreatedFrom,
				CreatedUntil:        request.CreatedUntil,
				Limit:               request.Limit,
				Precision:           request.Precision,
			},
			Workspace: options.Workspace,
			StateDir:  options.StateDir,
			Config:    options.Config,
			Now:       time.Now().UTC(),
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(ledger)
	})
	if err := enforceAdmissionGate(options.StateDir, boot.WorkspaceHash, options.Config); err != nil {
		return RunResult{}, err
	}
	active, err := boot.Journal.NonTerminalRuns(ctx, boot.WorkspaceHash)
	if err != nil {
		return RunResult{}, err
	}
	if len(active) != 0 {
		if options.ParentRunID != "" {
			// 子运行家族豁免：活跃运行必须是本子运行的祖先或同父兄弟；
			// 无关运行仍按排他合同拒绝。
			if err := tolerateFamilyActiveRuns(ctx, boot.Journal, boot.WorkspaceHash, active, options.ParentRunID); err != nil {
				return RunResult{}, err
			}
		} else {
			switch active[0].Phase() {
			case "waiting_approval":
				pending := active[0].PendingApproval
				if pending != nil {
					return RunResult{}, fmt.Errorf("%w: %w", ErrActiveRun, &ports.WaitingApprovalError{RunID: active[0].ID, OperationID: pending.OperationID, Fingerprint: pending.Fingerprint, Summary: pending.Summary})
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
	}
	if err := validateSessionLink(ctx, boot.Journal, options.StateDir, boot.WorkspaceHash, options); err != nil {
		return RunResult{}, err
	}
	if options.RequireSessionTail {
		// RequireSessionTail 只服务同 session 续链：缺少前驱时失败关闭，
		// 不因 fork_source 或空值静默跳过唯一链尾门禁。
		continuation, err := resolveSessionContinuation(ctx, boot.Journal, options.StateDir, boot.WorkspaceHash, options.PrevRunID)
		if err != nil {
			return RunResult{}, err
		}
		if options.SessionID != continuation.SessionID {
			return RunResult{}, fmt.Errorf("%w: run %s belongs to session %s", ErrSessionContinuation, options.PrevRunID, continuation.SessionID)
		}
	}
	runID := options.PreassignedRunID
	if runID == "" {
		runID = domain.NewRunID()
	}
	if err := validateSubagentRunParent(ctx, boot.Journal, boot.WorkspaceHash, runID, options); err != nil {
		return RunResult{}, err
	}
	if options.ParentRunID != "" {
		if err := enforceSubagentToolNarrowing(ctx, boot.Journal, boot.Artifacts, boot.WorkspaceHash, boot.Registry, options); err != nil {
			return RunResult{}, err
		}
	} else {
		// 模型侧委派仅绑定根运行（子运行不嵌套委派）；回调复用本次装配的
		// Journal 与配置，父 Loop 在工具执行期间阻塞等待子终态或审批暂停。
		boot.Loop.Delegator = service.delegatorFor(boot, options)
	}
	configFingerprint := options.Config.Fingerprint()
	frozenConfig, err := marshalFrozenRunConfig(options.Config, configFingerprint)
	if err != nil {
		return RunResult{}, fmt.Errorf("encode frozen run config: %w", err)
	}
	configRef, err := boot.Artifacts.Put(frozenConfig, "application/json")
	if err != nil {
		return RunResult{}, fmt.Errorf("store frozen run config: %w", err)
	}
	sessionID, priorContext, priorContextRef, err := service.resolveSessionContext(ctx, boot.Journal, boot.Artifacts, options)
	if err != nil {
		return RunResult{}, err
	}
	projectInstructionsRef, err := freezeProjectInstructions(boot.Workspace, boot.Artifacts, options.DisableUserInstructions)
	if err != nil {
		return RunResult{}, err
	}
	first, err := boot.Journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
		domain.RunCreatedPayload
		ConfigRef              artifact.Ref  `json:"config_ref"`
		PriorContextRef        *artifact.Ref `json:"prior_context_ref,omitempty"`
		ProjectInstructionsRef *artifact.Ref `json:"project_instructions_ref,omitempty"`
	}{RunCreatedPayload: domain.RunCreatedPayload{Task: options.Task, Workspace: boot.Workspace.Root, ConfigFingerprint: configFingerprint, Build: build, SessionID: sessionID, PrevRunID: options.PrevRunID, ForkSource: options.ForkSource, ParentRunID: options.ParentRunID}, ConfigRef: configRef, PriorContextRef: priorContextRef, ProjectInstructionsRef: projectInstructionsRef})})
	if err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(first)
	}
	// 子运行身份：向父 run 追加 delegated（幂等，已委派则返回既有事实）。
	if options.ParentRunID != "" {
		if _, delegatedErr := RecordSubagentDelegated(ctx, boot.Journal, options.ParentRunID, boot.WorkspaceHash, DelegateOptions{
			ParentRunID: options.ParentRunID,
			ChildRunID:  runID,
			Task:        options.Task,
		}, first.OccurredAt); delegatedErr != nil {
			return RunResult{}, delegatedErr
		}
	}
	second, err := boot.Journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunStarted, Payload: artifact.MustEncodeRedactedJSON(struct {
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
	boot.Loop.AdoptSteering(options.Steering)
	result, err := boot.Loop.Run(runContext, agent.RunInput{RunID: runID, Task: options.Task, WorkspaceHash: boot.WorkspaceHash, SessionID: sessionID, PrevRunID: options.PrevRunID, PriorContext: priorContext, PriorContextRef: priorContextRef})
	if wallBudgetExpired(runContext) && !runReachedTerminal(boot.Journal, runID) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = boot.Loop.Run(expiredContext, agent.RunInput{RunID: runID, Task: options.Task, WorkspaceHash: boot.WorkspaceHash, SessionID: sessionID, PrevRunID: options.PrevRunID, PriorContext: priorContext, PriorContextRef: priorContextRef})
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, runID, result.Response, err)
}

// contractResultOrNil 在投影失败时返回 nil 而不是零值，避免把半成品合同误当
// 有效结果；projectedRunOutcome 仍会把投影失败作为状态损坏返回。
func contractResultOrNil(result contract.Result, err error) *contract.Result {
	if err != nil {
		return nil
	}
	return &result
}

// projectedRunOutcome 从当前事件日志投影一次产品合同，并把它附加到 Run、
// Resume、Reconcile 与 Abort 的统一返回值。执行错误优先返回；执行成功但投影
// 失败说明持久化状态无法形成有效产品合同，必须以 ErrStateCorrupt 失败关闭。
func projectedRunOutcome(journal *sqlite.Journal, stateDir, workspaceHash string, runID domain.RunID, response string, runErr error) (RunResult, error) {
	result, projectionErr := projectRunResult(context.Background(), journal, stateDir, workspaceHash, runID)
	sessionID := ""
	var notificationErr error
	if projectionErr == nil {
		events, loadErr := journal.Load(context.Background(), runID)
		if loadErr != nil {
			projectionErr = loadErr
		} else if projection, replayErr := domain.Replay(events); replayErr != nil {
			projectionErr = replayErr
		} else {
			sessionID = projection.SessionID
			if projection.Terminal() && projection.ParentRunID != "" {
				notificationContext, cancel := context.WithTimeout(context.Background(), subagentCompletionTimeout)
				notificationErr = NotifySubagentCompleted(notificationContext, journal, workspaceHash, RunOptions{ParentRunID: projection.ParentRunID}, runID, subagentRunOutcome(projection.State), nil, time.Now().UTC())
				cancel()
			}
		}
	}
	outcome := RunResult{ID: runID, Response: response, SessionID: sessionID, Result: contractResultOrNil(result, projectionErr)}
	if projectionErr != nil {
		return outcome, errors.Join(runErr, fmt.Errorf("%w: %v", ErrStateCorrupt, projectionErr), notificationErr)
	}
	return outcome, errors.Join(runErr, notificationErr)
}

// Inspect 只读加载指定运行，验证完整性后返回投影与可选事件历史。
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

// wallBudgetExpired 只在墙钟预算上下文自身到期时为真。HTTP 客户端的单请求
// 超时在当前 Go 版本同样满足 errors.Is(err, context.DeadlineExceeded)，因此
// 判断预算是否耗尽必须依据预算上下文而非错误链，避免把可重试的瞬态请求
// 超时误终态化为 wall_time 预算失败。
func wallBudgetExpired(runContext context.Context) bool {
	return errors.Is(runContext.Err(), context.DeadlineExceeded)
}

// runReachedTerminal 只读回放日志，判断运行是否已持久化终态。墙钟收尾必须
// 以 Journal 为准：回调或返回调度跨过截止时间时，已完成的运行不能被再次按
// 超时收尾，调用方已拿到的成功响应与既有失败分类都必须原样保留。
func runReachedTerminal(journal *sqlite.Journal, runID domain.RunID) bool {
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		return false
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return false
	}
	return projection.Terminal()
}

// checkAdmissionGate 以只读方式计算 Provider 准入状态；即使 optional
