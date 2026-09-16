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
	"sort"
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
	"github.com/fxbin/cairn/internal/provider"
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
	// ErrSessionContinuation 表示所选历史运行不满足安全续链条件。
	ErrSessionContinuation = errors.New("historical run is not a safe session continuation point")
)

// EventSink 接收已成功持久化、可供 CLI 流式输出的领域事件。
type EventSink func(domain.Event)

// Service 组合应用层用例所需的可替换 Provider 构造器。
type Service struct {
	NewProvider func(config.Config) (ports.Provider, error)
}

// derefRef 解引用可选制品引用；nil 返回零值（配合 omitempty 不写入事件）。
func derefRef(ref *artifact.Ref) artifact.Ref {
	if ref == nil {
		return artifact.Ref{}
	}
	return *ref
}

// RunOptions 描述启动一次全新 Agent 运行所需的输入与依赖。
type RunOptions struct {
	Task      string
	Workspace string
	StateDir  string
	Config    config.Config
	OnEvent   EventSink
	// 会话链关系（ADR-0018）：显式携带时写入创建事件并注入前情；空值时
	// 行为与无会话场景完全一致。前情文本与制品引用由调用方成对提供，或
	// 由应用层从上一 run 生成（前情卡服务）。
	SessionID       string
	PrevRunID       domain.RunID
	PriorContext    string
	PriorContextRef *artifact.Ref
	// RequireSessionTail 要求 PrevRunID 在创建事件前仍是其完整历史会话的
	// 唯一终态链尾；历史选择器用它封闭“展示后状态已变化”的竞态。
	RequireSessionTail bool
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

// RunHistoryItem 是会话选择器使用的只读运行摘要，不暴露工作区路径、审批
// 指纹或操作细节；时间与会话关系均来自事件回放。
type RunHistoryItem struct {
	ID        domain.RunID    `json:"run_id"`
	SessionID string          `json:"session_id,omitempty"`
	PrevRunID domain.RunID    `json:"prev_run_id,omitempty"`
	Task      string          `json:"task"`
	State     domain.RunState `json:"state"`
	Phase     string          `json:"phase"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// SessionContinuation 是历史会话选择器返回的下一次运行锚点；它只恢复会话
// 标识和唯一终态链尾，不授予历史回退能力。
type SessionContinuation struct {
	SessionID string       `json:"session_id"`
	PrevRunID domain.RunID `json:"prev_run_id"`
}

// Run 初始化事件日志、锁、工具和策略后启动一次新的 Agent Loop；返回前关闭
// Boot，关闭错误与执行错误一并合并上报，绝不静默丢弃。
func (service Service) Run(ctx context.Context, options RunOptions) (outcome RunResult, err error) {
	if options.Task == "" {
		return RunResult{}, errors.New("task is required")
	}
	build, err := currentBuildIdentity()
	if err != nil {
		return RunResult{}, err
	}
	boot, err := service.assembleBoot(options.Workspace, options.StateDir, options.Config, options.OnEvent)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	if err := enforceAdmissionGate(options.StateDir, boot.WorkspaceHash, options.Config); err != nil {
		return RunResult{}, err
	}
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
	if err := validateSessionLink(ctx, boot.Journal, boot.WorkspaceHash, options); err != nil {
		return RunResult{}, err
	}
	if options.RequireSessionTail {
		continuation, err := resolveSessionContinuation(ctx, boot.Journal, boot.WorkspaceHash, options.PrevRunID)
		if err != nil {
			return RunResult{}, err
		}
		if options.SessionID != continuation.SessionID {
			return RunResult{}, fmt.Errorf("%w: run %s belongs to session %s", ErrSessionContinuation, options.PrevRunID, continuation.SessionID)
		}
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
	runID := domain.NewRunID()
	first, err := boot.Journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunCreated, Payload: encode(struct {
		domain.RunCreatedPayload
		ConfigRef       artifact.Ref  `json:"config_ref"`
		PriorContextRef *artifact.Ref `json:"prior_context_ref,omitempty"`
	}{RunCreatedPayload: domain.RunCreatedPayload{Task: options.Task, Workspace: boot.Workspace.Root, ConfigFingerprint: configFingerprint, Build: build, SessionID: sessionID, PrevRunID: options.PrevRunID}, ConfigRef: configRef, PriorContextRef: priorContextRef})})
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
	result, err := boot.Loop.Run(runContext, agent.RunInput{RunID: runID, Task: options.Task, WorkspaceHash: boot.WorkspaceHash, SessionID: sessionID, PrevRunID: options.PrevRunID, PriorContext: priorContext, PriorContextRef: priorContextRef})
	if errors.Is(err, context.DeadlineExceeded) {
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
	outcome := RunResult{ID: runID, Response: response, Result: contractResultOrNil(result, projectionErr)}
	if runErr != nil {
		return outcome, runErr
	}
	if projectionErr != nil {
		return outcome, fmt.Errorf("%w: %v", ErrStateCorrupt, projectionErr)
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

// ListRuns 只读列出当前工作区的历史运行；每条记录均经过完整事件回放，按
// 创建时间降序、同时间 RunID 升序返回。首次使用尚无日志时返回空列表。
func (service Service) ListRuns(ctx context.Context, workspacePath, stateDir string, configuration config.Config) ([]RunHistoryItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return nil, err
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []RunHistoryItem{}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sqlite.IsStateCorruptError(err) {
			return nil, fmt.Errorf("%w: open run journal: %w", ErrStateCorrupt, err)
		}
		return nil, err
	}
	defer journal.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items, err := listRunHistory(ctx, journal, workspaceHash)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].CreatedAt.Equal(items[right].CreatedAt) {
			return items[left].ID < items[right].ID
		}
		return items[left].CreatedAt.After(items[right].CreatedAt)
	})
	return items, nil
}

// listRunHistory 从已打开的日志完整回放当前工作区运行；调用方决定排序与
// 生命周期。日志内容或跨工作区身份不一致时失败关闭。
func listRunHistory(ctx context.Context, journal interface {
	ports.Journal
	ports.RunLister
}, workspaceHash string) ([]RunHistoryItem, error) {
	runIDs, err := journal.RunIDs(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sqlite.IsStateCorruptError(err) {
			return nil, fmt.Errorf("%w: list runs: %w", ErrStateCorrupt, err)
		}
		return nil, err
	}
	items := make([]RunHistoryItem, 0, len(runIDs))
	for _, runID := range runIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		events, loadErr := journal.Load(ctx, runID)
		if loadErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if sqlite.IsStateCorruptError(loadErr) {
				return nil, fmt.Errorf("%w: load run %s: %w", ErrStateCorrupt, runID, loadErr)
			}
			return nil, loadErr
		}
		if len(events) == 0 {
			return nil, fmt.Errorf("%w: run %s has no events", ErrStateCorrupt, runID)
		}
		projection, replayErr := domain.Replay(events)
		if replayErr != nil {
			return nil, fmt.Errorf("%w: replay run %s: %v", ErrStateCorrupt, runID, replayErr)
		}
		if projection.WorkspaceHash != workspaceHash {
			return nil, fmt.Errorf("%w: run %s belongs to workspace %s", ErrStateCorrupt, runID, projection.WorkspaceHash)
		}
		items = append(items, RunHistoryItem{ID: projection.ID, SessionID: projection.SessionID, PrevRunID: projection.PrevRunID, Task: projection.Task, State: projection.State, Phase: projection.Phase(), CreatedAt: projection.CreatedAt, UpdatedAt: projection.UpdatedAt})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// validateSessionLink 校验所有新 Run 的会话边：前驱必须存在于当前工作区、
// 已终结且会话标识一致；同一会话不得创建第二个根。显式回退仍可从合法的
// 历史中间节点分叉，不受唯一链尾门禁约束。
func validateSessionLink(ctx context.Context, journal interface {
	ports.Journal
	ports.RunLister
}, workspaceHash string, options RunOptions) error {
	if options.PrevRunID != "" {
		events, err := journal.Load(ctx, options.PrevRunID)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return fmt.Errorf("%w: prior run %s not found", ErrSessionContinuation, options.PrevRunID)
		}
		projection, err := domain.Replay(events)
		if err != nil {
			return fmt.Errorf("%w: replay prior run %s: %v", ErrStateCorrupt, options.PrevRunID, err)
		}
		if projection.WorkspaceHash != workspaceHash {
			return fmt.Errorf("%w: prior run %s belongs to another workspace", ErrSessionContinuation, options.PrevRunID)
		}
		if !projection.Terminal() {
			return fmt.Errorf("%w: prior run %s is not terminal", ErrSessionContinuation, options.PrevRunID)
		}
		if projection.SessionID == "" {
			return fmt.Errorf("%w: prior run %s has no session identity", ErrSessionContinuation, options.PrevRunID)
		}
		if options.SessionID != "" && projection.SessionID != "" && options.SessionID != projection.SessionID {
			return fmt.Errorf("%w: prior run %s belongs to session %s", ErrSessionContinuation, options.PrevRunID, projection.SessionID)
		}
		return nil
	}
	if options.SessionID == "" {
		return nil
	}
	items, err := listRunHistory(ctx, journal, workspaceHash)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.SessionID == options.SessionID {
			return fmt.Errorf("%w: session %s already has a root", ErrSessionContinuation, options.SessionID)
		}
	}
	return nil
}

// ResolveSessionContinuation 只读验证指定历史运行是否为当前工作区某条完整
// 会话链的唯一终态链尾。返回值仅供下一次 Run 续链，不恢复历史回退列表。
func (service Service) ResolveSessionContinuation(ctx context.Context, workspacePath, stateDir string, configuration config.Config, runID domain.RunID) (SessionContinuation, error) {
	if runID == "" {
		return SessionContinuation{}, fmt.Errorf("%w: run ID is required", ErrSessionContinuation)
	}
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return SessionContinuation{}, err
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionContinuation{}, fmt.Errorf("%w: run %s not found", ErrSessionContinuation, runID)
		}
		if ctx.Err() != nil {
			return SessionContinuation{}, ctx.Err()
		}
		if sqlite.IsStateCorruptError(err) {
			return SessionContinuation{}, fmt.Errorf("%w: open run journal: %w", ErrStateCorrupt, err)
		}
		return SessionContinuation{}, err
	}
	defer journal.Close()
	return resolveSessionContinuation(ctx, journal, workspaceHash, runID)
}

// resolveSessionContinuation 在已打开日志上重建并验证会话链；整条会话必须
// 连通、无环、无分叉且均已终结，所选运行必须是唯一链尾。
func resolveSessionContinuation(ctx context.Context, journal interface {
	ports.Journal
	ports.RunLister
}, workspaceHash string, runID domain.RunID) (SessionContinuation, error) {
	items, err := listRunHistory(ctx, journal, workspaceHash)
	if err != nil {
		return SessionContinuation{}, err
	}
	byID := make(map[domain.RunID]RunHistoryItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	selected, exists := byID[runID]
	if !exists {
		return SessionContinuation{}, fmt.Errorf("%w: run %s not found", ErrSessionContinuation, runID)
	}
	if selected.SessionID == "" {
		return SessionContinuation{}, fmt.Errorf("%w: run %s has no session identity", ErrSessionContinuation, runID)
	}
	members := make(map[domain.RunID]RunHistoryItem)
	children := make(map[domain.RunID]int)
	for _, item := range items {
		if item.PrevRunID != "" {
			parent, parentExists := byID[item.PrevRunID]
			if parentExists && parent.SessionID != item.SessionID {
				return SessionContinuation{}, fmt.Errorf("%w: run %s crosses session boundary from %s", ErrStateCorrupt, item.ID, item.PrevRunID)
			}
			children[item.PrevRunID]++
		}
		if item.SessionID != selected.SessionID {
			continue
		}
		if item.State != domain.RunCompleted && item.State != domain.RunFailed && item.State != domain.RunCancelled {
			return SessionContinuation{}, fmt.Errorf("%w: session %s contains non-terminal run %s", ErrSessionContinuation, selected.SessionID, item.ID)
		}
		members[item.ID] = item
	}
	for parent, count := range children {
		if _, selectedMember := members[parent]; !selectedMember {
			continue
		}
		if count > 1 {
			return SessionContinuation{}, fmt.Errorf("%w: session %s forks after run %s", ErrSessionContinuation, selected.SessionID, parent)
		}
	}
	for _, item := range members {
		if item.PrevRunID == "" {
			continue
		}
		if _, exists := members[item.PrevRunID]; !exists {
			return SessionContinuation{}, fmt.Errorf("%w: session %s references missing run %s", ErrStateCorrupt, selected.SessionID, item.PrevRunID)
		}
	}
	if children[runID] != 0 {
		return SessionContinuation{}, fmt.Errorf("%w: run %s is not the session tail", ErrSessionContinuation, runID)
	}
	visited := make(map[domain.RunID]struct{}, len(members))
	current := selected
	for {
		if _, duplicate := visited[current.ID]; duplicate {
			return SessionContinuation{}, fmt.Errorf("%w: session %s contains a cycle", ErrSessionContinuation, selected.SessionID)
		}
		visited[current.ID] = struct{}{}
		if current.PrevRunID == "" {
			break
		}
		current = members[current.PrevRunID]
	}
	if len(visited) != len(members) {
		return SessionContinuation{}, fmt.Errorf("%w: run %s is not the unique session tail", ErrSessionContinuation, runID)
	}
	return SessionContinuation{SessionID: selected.SessionID, PrevRunID: selected.ID}, nil
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
	if err := checkRunArtifactReferences(events, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: use --resolve %s=<verdict>", ErrWaitingReconcile, projection.PendingOperation.ID)
	}
	// 回退 run（task_ref 承载计划制品）不进入模型续跑：按同一计划重推导反放
	// 调用后交给脚本化执行器收束，普通 run 行为不变。
	if plan, isRewind, planErr := rewindPlanOfRun(ctx, boot.Journal, boot.Artifacts, options.RunID); planErr != nil {
		return RunResult{ID: options.RunID}, planErr
	} else if isRewind {
		return service.driveRewindRun(ctx, boot, options.RunID, plan.Runs, RewindOptions{Workspace: options.Workspace, StateDir: options.StateDir, Config: options.Config, Approval: options.Approval, OnEvent: options.OnEvent})
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
	var boot *Boot
	if options.Verdict == "abort_run" {
		boot, err = assembleRecoveryBoot(options.Workspace, options.StateDir, options.Config)
	} else {
		var legacyCapability bool
		legacyCapability, err = legacyProviderCapabilityResume(options.Workspace, options.StateDir, options.Config, options.RunID)
		if err == nil {
			boot, err = service.assembleBootWithLegacyProviderCapability(options.Workspace, options.StateDir, options.Config, options.OnEvent, legacyCapability)
		}
	}
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
	if err := checkRunArtifactReferences(events, options.StateDir, boot.WorkspaceHash); err != nil {
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
	// 回退 run 对账后仍由脚本化执行器续跑，禁止落入模型 Loop 产生计划外操作。
	if plan, isRewind, planErr := rewindPlanOfRun(ctx, boot.Journal, boot.Artifacts, options.RunID); planErr != nil {
		return RunResult{ID: options.RunID}, planErr
	} else if isRewind {
		return service.driveRewindRun(ctx, boot, options.RunID, plan.Runs, RewindOptions{Workspace: options.Workspace, StateDir: options.StateDir, Config: options.Config, OnEvent: options.OnEvent})
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
func (service Service) Abort(ctx context.Context, options AbortOptions) (outcome RunResult, err error) {
	if strings.TrimSpace(options.Reason) == "" {
		return RunResult{}, errors.New("abort reason is required")
	}
	boot, err := assembleRecoveryBoot(options.Workspace, options.StateDir, options.Config)
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
	switch projection.State {
	case domain.RunCreated, domain.RunRunning, domain.RunCancelling:
	default:
		return RunResult{ID: options.RunID}, errors.New("run is not abortable")
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkRunArtifactReferences(events, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.PendingOperation != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: use --resolve %s=abort_run", ErrWaitingReconcile, projection.PendingOperation.ID)
	}
	terminal := abortTerminal{
		eventType:   domain.EventRunFailed,
		turnOutcome: "failed",
		payload: struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}{Error: "user_aborted", Reason: options.Reason},
		approvalReason: options.Reason,
	}
	if projection.State == domain.RunCancelling {
		request, found, requestErr := latestAbortCancellationRequest(events)
		if requestErr != nil {
			return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, requestErr)
		}
		if !found {
			return RunResult{ID: options.RunID}, fmt.Errorf("%w: cancelling run has no persisted cancellation request", ErrStateCorrupt)
		}
		terminal = abortTerminalFromCancellation(request)
	}
	appendEvent := func(eventType domain.EventType, turnID *domain.TurnID, operationID *domain.OperationID, payload any) error {
		draft := domain.EventDraft{RunID: options.RunID, WorkspaceHash: boot.WorkspaceHash, Type: eventType, TurnID: turnID, OperationID: operationID, Payload: encode(payload)}
		// 与 Reconcile 相同：先在克隆投影上模拟迁移再写入，防止不可回放事件落盘。
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
	if pending := projection.PendingApproval; pending != nil {
		if projection.CurrentTurn == nil {
			return RunResult{}, errors.New("pending approval has no open turn")
		}
		turnID, operationID := *projection.CurrentTurn, pending.OperationID
		if err := appendEvent(domain.EventApprovalDenied, &turnID, &operationID, struct {
			Fingerprint string `json:"fingerprint"`
			Actor       string `json:"actor"`
			Reason      string `json:"reason"`
		}{Fingerprint: pending.Fingerprint, Actor: "system", Reason: terminal.approvalReason}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
	}
	if projection.CurrentTurn != nil {
		turnID := *projection.CurrentTurn
		if err := appendEvent(domain.EventTurnClosed, &turnID, nil, domain.TurnClosedPayload{Outcome: terminal.turnOutcome}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
	}
	if err := appendEvent(terminal.eventType, nil, nil, terminal.payload); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, "", nil)
}

// abortCancellationRequest 是应用层恢复已持久化取消意图所需的最小投影。
type abortCancellationRequest struct {
	Cause            string `json:"cause"`
	ExpectedTerminal string `json:"expected_terminal,omitempty"`
	BudgetDimension  string `json:"budget_dimension,omitempty"`
}

// abortTerminal 描述 Abort 补写开放轮次与运行终态时使用的一致决定。
type abortTerminal struct {
	eventType      domain.EventType
	turnOutcome    string
	payload        any
	approvalReason string
}

// latestAbortCancellationRequest 返回最近一次取消请求，并兼容未记录预期终态
// 的旧事件：预算原因或预算维度映射为 failed，其余原因映射为 cancelled。
func latestAbortCancellationRequest(events []domain.Event) (abortCancellationRequest, bool, error) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != domain.EventRunCancelRequested {
			continue
		}
		var request abortCancellationRequest
		if err := json.Unmarshal(events[index].Payload, &request); err != nil {
			return abortCancellationRequest{}, false, fmt.Errorf("decode run_cancel_requested payload: %w", err)
		}
		if request.ExpectedTerminal == "" {
			if request.Cause == "budget_exhausted" || request.BudgetDimension != "" {
				request.ExpectedTerminal = "failed"
			} else {
				request.ExpectedTerminal = "cancelled"
			}
		}
		switch request.ExpectedTerminal {
		case "failed":
			if request.Cause == "" {
				request.Cause = "budget_exhausted"
			}
		case "cancelled":
			if request.Cause == "" {
				request.Cause = "user_interrupt"
			}
		default:
			return abortCancellationRequest{}, false, fmt.Errorf("run_cancel_requested has invalid expected terminal %q", request.ExpectedTerminal)
		}
		return request, true, nil
	}
	return abortCancellationRequest{}, false, nil
}

// abortTerminalFromCancellation 把持久化取消决定转换为终态事件，保留预算
// 失败维度；错误文本复用 Agent 的稳定预算错误格式。
func abortTerminalFromCancellation(request abortCancellationRequest) abortTerminal {
	if request.ExpectedTerminal == "cancelled" {
		return abortTerminal{
			eventType: domain.EventRunCancelled, turnOutcome: "cancelled",
			payload: struct {
				Cause string `json:"cause"`
			}{Cause: request.Cause},
			approvalReason: request.Cause,
		}
	}
	errorText := request.Cause
	if request.Cause == "budget_exhausted" {
		errorText = "budget exhausted"
		if request.BudgetDimension != "" {
			errorText = (&agent.BudgetExceededError{Dimension: request.BudgetDimension}).Error()
		}
	}
	return abortTerminal{
		eventType: domain.EventRunFailed, turnOutcome: "failed",
		payload: struct {
			Error           string `json:"error"`
			BudgetDimension string `json:"budget_dimension,omitempty"`
		}{Error: errorText, BudgetDimension: request.BudgetDimension},
		approvalReason: request.Cause,
	}
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
			ResultRef        *artifact.Ref   `json:"result_ref"`
			PlanRef          *artifact.Ref   `json:"plan_ref"`
			ConfigRef        *artifact.Ref   `json:"config_ref"`
			EvidenceRef      *artifact.Ref   `json:"evidence_ref"`
			PlannedBeforeRef *artifact.Ref   `json:"planned_before_ref"`
			PriorContextRef  json.RawMessage `json:"prior_context_ref"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		if len(payload.PriorContextRef) > 0 {
			var reference artifact.Ref
			if string(payload.PriorContextRef) == "null" || json.Unmarshal(payload.PriorContextRef, &reference) != nil {
				return fmt.Errorf("run %s event %d: invalid prior context ref", event.RunID, event.Seq)
			}
			if err := checkArtifactReference(root, reference); err != nil {
				return fmt.Errorf("run %s event %d: %w", event.RunID, event.Seq, err)
			}
		}
		// 验证计划与冻结配置是审批绑定的信任根，与结果制品同等纳入
		// 存在性与摘要校验，防止被删除或篡改后静默通过诊断；before 快照
		// 是工作区回退的恢复源，同样必须完整在档。
		for _, reference := range []*artifact.Ref{payload.ResultRef, payload.PlanRef, payload.ConfigRef, payload.EvidenceRef, payload.PlannedBeforeRef} {
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
	_, err := readArtifactContent(root, reference)
	return err
}

// readArtifactContent 读取并校验制品内容：摘要格式合法、文件在档且内容哈
// 希与引用一致；失败关闭，供诊断校验与展示层只读解引用共用。
func readArtifactContent(root string, reference artifact.Ref) ([]byte, error) {
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	if len(digest) != sha256.Size*2 {
		return nil, fmt.Errorf("invalid artifact digest %q", reference.SHA256)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, fmt.Errorf("decode artifact digest: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil {
		return nil, fmt.Errorf("read referenced artifact: %w", err)
	}
	actual := sha256.Sum256(content)
	if hex.EncodeToString(actual[:]) != digest {
		return nil, errors.New("referenced artifact hash mismatch")
	}
	return content, nil
}

// ReadRunArtifact 只读解引用当前工作区制品库中的一个制品：内容哈希必须与
// 引用一致，供展示层（如补丁差异视图）消费已持久化证据而不触碰任何状态。
func (service Service) ReadRunArtifact(workspacePath, stateDir string, configuration config.Config, reference artifact.Ref) ([]byte, error) {
	_, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return nil, err
	}
	return readArtifactContent(manifestArtifactRoot(stateDir, hash), reference)
}

// openReadOnlyJournal 打开指定工作区的不可写 SQLite 事件日志。
func openReadOnlyJournal(stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.OpenReadOnly(filepath.Join(directory, "runs.db"))
}

// openReadOnlyJournalContext 在调用方上下文约束下打开指定工作区的不可写
// SQLite 事件日志，供需要在连接探测阶段响应取消的只读用例使用。
func openReadOnlyJournalContext(ctx context.Context, stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.OpenReadOnlyContext(ctx, filepath.Join(directory, "runs.db"))
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
	var envelope struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(frozenConfig, &envelope); err != nil {
		return false, fmt.Errorf("%w: decode frozen provider capability: %v", ErrStateCorrupt, err)
	}
	if envelope.SchemaVersion != nil {
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

// checkAdmissionGate 以只读方式计算版本化 Provider 准入状态；即使 optional
// 不拦截，也验证现有 admitted 决定及其证据，供 Run 与 Doctor 共享同一事实。
func checkAdmissionGate(stateDir, workspaceHash string, configuration config.Config) (AdmissionHealth, error) {
	status := AdmissionHealth{Mode: "disabled"}
	if configuration.Version != 2 {
		return status, nil
	}
	status.Mode = configuration.Provider.Admission
	profile, err := provider.ResolveResolvedProfile(provider.ResolvedFacts{
		Selection:     provider.SelectionFacts{ProviderID: configuration.Provider.ID, Model: configuration.Provider.Model, BaseURL: configuration.Provider.BaseURL},
		Capability:    config.ProviderCapabilityDeclaration(configuration),
		Generation:    config.ProviderGenerationDeclaration(configuration),
		CredentialRef: configuration.Provider.APIKeyEnv,
	})
	if err != nil {
		status.Error = admissionErrorSummary(err)
		return status, err
	}
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	decisions, err := provider.OpenDecisionStoreReadOnly(stateDir, workspaceHash)
	if err != nil {
		status.Error = admissionErrorSummary(err)
		return status, err
	}
	evidence, err := provider.OpenEvidenceStoreReadOnly(stateDir, workspaceHash)
	if err != nil {
		status.Error = admissionErrorSummary(err)
		return status, err
	}
	outcome, err := provider.EvaluateAdmissionGate(decisions, evidence, profile, config.ProviderAdmissionRequired)
	if err != nil {
		status.Error = admissionErrorSummary(err)
		return status, err
	}
	status.Admitted = outcome.Head != nil && outcome.Head.Decision == provider.DecisionAdmitted
	if configuration.Provider.Admission == config.ProviderAdmissionRequired {
		status.Admitted = outcome.Allowed
	}
	return status, nil
}

// admissionErrorSummary 把准入失败归一为不含路径和凭据的稳定诊断原因。
func admissionErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "no admission decision"):
		return "no admission decision on file for this binding"
	case strings.Contains(message, "admission is rejected"):
		return "provider admission is rejected for this binding"
	case strings.Contains(message, "selection does not match"):
		return "admission evidence does not match the resolved provider"
	default:
		return "admission evidence or decision state is invalid"
	}
}

// enforceAdmissionGate 执行版本 2 配置的准入门（D4 优先级链的最后一环）：
// version 1 配置无准入语义直接通过；optional 只展示检查结果而不拦截；
// required 要求唯一 admitted 链头、在档证据与解析身份一致。
func enforceAdmissionGate(stateDir, workspaceHash string, configuration config.Config) error {
	status, err := checkAdmissionGate(stateDir, workspaceHash, configuration)
	if configuration.Version != 2 || configuration.Provider.Admission == config.ProviderAdmissionOptional {
		return nil
	}
	if err != nil {
		return fmt.Errorf("admission gate: %w", err)
	}
	if !status.Admitted {
		return errors.New("admission gate: provider is not admitted for this binding")
	}
	return nil
}
