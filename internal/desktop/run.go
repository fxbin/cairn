package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// 本文件实现桌面端异步运行控制器：初始化快照 + 增量事件，以 runID 会话隔离，
// 旧回调不得污染新会话。Event Journal 仍是事实源，这里只做通知与唤醒。

// Emitter 向前端推送 StreamEvent；由 Wails 层实现。
type Emitter func(StreamEvent)

// PrefsStore 读写 UI 偏好（最近项目、会话元数据）。偏好不进入 Journal。
type PrefsStore struct {
	mu      sync.RWMutex
	path    string
	data    Preferences
	loadErr error
}

// Preferences 是 GUI 本地偏好。
type Preferences struct {
	RecentProjects []RecentProject      `json:"recent_projects"`
	SessionMeta    []SessionMeta        `json:"session_meta"`
	RunOverrides   []RunOverride        `json:"run_overrides,omitempty"`
	Hooks          []HookRule           `json:"hooks,omitempty"`
	Workflows      []WorkflowDefinition `json:"workflows,omitempty"`
	ReviewNotes    []ReviewNote         `json:"review_notes,omitempty"`
	Theme          string               `json:"theme,omitempty"`
	FontSize       int                  `json:"font_size,omitempty"`
}

// RunOverride 保存启动时的运行时覆盖，使审批恢复可复现同一冻结指纹。
type RunOverride struct {
	RunID string `json:"run_id"`
	Model string `json:"model,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

// HookAction 是钩子允许的有限动作之一。
type HookAction struct {
	// Kind 仅支持 queue_followup（入队后续任务）。
	Kind    string `json:"kind"`
	Content string `json:"content,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// HookRule 绑定工作区与事件到有限动作；不能提升权限。
type HookRule struct {
	ID        string       `json:"id"`
	Workspace string       `json:"workspace"`
	Event     string       `json:"event"`
	Enabled   bool         `json:"enabled"`
	Actions   []HookAction `json:"actions"`
	// FailStreak 连续失败次数；达到阈值自动停用。
	FailStreak int `json:"fail_streak,omitempty"`
}

// SaveRunOverride 记录 run 的运行时覆盖。
func (store *PrefsStore) SaveRunOverride(override RunOverride) error {
	if strings.TrimSpace(override.RunID) == "" {
		return errInvalid("run_id is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.RunOverrides {
		if store.data.RunOverrides[i].RunID == override.RunID {
			store.data.RunOverrides[i] = override
			return store.save()
		}
	}
	store.data.RunOverrides = append(store.data.RunOverrides, override)
	const maxOverrides = 200
	if len(store.data.RunOverrides) > maxOverrides {
		store.data.RunOverrides = store.data.RunOverrides[len(store.data.RunOverrides)-maxOverrides:]
	}
	return store.save()
}

// LoadRunOverride 读取 run 的运行时覆盖；不存在时返回空值。
func (store *PrefsStore) LoadRunOverride(runID string) (RunOverride, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, item := range store.data.RunOverrides {
		if item.RunID == runID {
			return item, true
		}
	}
	return RunOverride{}, false
}

// NewPrefsStore 在用户配置根下打开偏好文件（不存在则空）。
func NewPrefsStore() (*PrefsStore, error) {
	root, err := userConfigRoot()
	if err != nil {
		return nil, err
	}
	store := &PrefsStore{path: filepath.Join(root, preferencesFile)}
	store.loadErr = store.load()
	return store, nil
}

// NewPrefsStoreAt 供测试指定偏好文件路径。
func NewPrefsStoreAt(path string) *PrefsStore {
	store := &PrefsStore{path: path}
	// 损坏文件 load 返回错误但不阻止进程启动；后续保存会再次失败关闭，
	// 且损坏内容已旁路保全。测试与调用方可读取 store 状态。
	store.loadErr = store.load()
	return store
}

func (store *PrefsStore) load() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	raw, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			store.data = Preferences{RecentProjects: []RecentProject{}, SessionMeta: []SessionMeta{}}
			return nil
		}
		return err
	}
	var decoded Preferences
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// 损坏偏好保全到旁路文件，避免后续保存静默覆盖用户数据。
		store.data = Preferences{RecentProjects: []RecentProject{}, SessionMeta: []SessionMeta{}}
		backup, backupErr := os.OpenFile(store.path+".corrupt", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(backupErr, os.ErrExist) {
			backup, backupErr = os.CreateTemp(filepath.Dir(store.path), filepath.Base(store.path)+".corrupt-*")
		}
		if backupErr != nil {
			return errInvalid("preferences file is corrupt; backup unavailable; original retained")
		}
		_, writeErr := backup.Write(raw)
		if err := errors.Join(writeErr, backup.Sync(), backup.Close()); err != nil {
			return errInvalid("preferences file is corrupt; backup incomplete; original retained")
		}
		return errInvalid("preferences file is corrupt; preserved at " + backup.Name())
	}
	if decoded.RecentProjects == nil {
		decoded.RecentProjects = []RecentProject{}
	}
	if decoded.SessionMeta == nil {
		decoded.SessionMeta = []SessionMeta{}
	}
	store.data = decoded
	return nil
}

// save 使用独占临时文件同步并替换偏好，避免固定临时路径跟随链接。
func (store *PrefsStore) save() error {
	if store.loadErr != nil {
		return store.loadErr
	}
	raw, err := json.MarshalIndent(store.data, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".preferences-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), store.path); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

// ListRecentProjects 返回最近项目（默认不含已归档）。
func (store *PrefsStore) ListRecentProjects(includeArchived bool) []RecentProject {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]RecentProject, 0, len(store.data.RecentProjects))
	for _, item := range store.data.RecentProjects {
		if item.Archived && !includeArchived {
			continue
		}
		out = append(out, item)
	}
	return out
}

// TouchProject 记录项目使用并置顶到最近列表。
func (store *PrefsStore) TouchProject(path string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	found := -1
	for i := range store.data.RecentProjects {
		if store.data.RecentProjects[i].Path == abs {
			found = i
			break
		}
	}
	item := RecentProject{Path: abs, Name: filepath.Base(abs), LastUsed: time.Now().UTC()}
	if found >= 0 {
		item.Pinned = store.data.RecentProjects[found].Pinned
		item.Archived = store.data.RecentProjects[found].Archived
		store.data.RecentProjects = append(store.data.RecentProjects[:found], store.data.RecentProjects[found+1:]...)
	}
	store.data.RecentProjects = append([]RecentProject{item}, store.data.RecentProjects...)
	const maxRecent = 30
	if len(store.data.RecentProjects) > maxRecent {
		store.data.RecentProjects = store.data.RecentProjects[:maxRecent]
	}
	return store.save()
}

// SetProjectArchived 归档或恢复最近项目展示（不删除运行历史）。
func (store *PrefsStore) SetProjectArchived(path string, archived bool) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for i := range store.data.RecentProjects {
		if store.data.RecentProjects[i].Path == abs {
			store.data.RecentProjects[i].Archived = archived
			return store.save()
		}
	}
	return os.ErrNotExist
}

// SessionPreferences 返回会话展示元数据。
func (store *PrefsStore) SessionPreferences() []SessionMeta {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]SessionMeta, len(store.data.SessionMeta))
	copy(out, store.data.SessionMeta)
	return out
}

// UpsertSessionMeta 写入会话标题/置顶/归档偏好。
func (store *PrefsStore) UpsertSessionMeta(meta SessionMeta) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.SessionMeta {
		if store.data.SessionMeta[i].SessionID == meta.SessionID {
			store.data.SessionMeta[i] = meta
			return store.save()
		}
	}
	store.data.SessionMeta = append(store.data.SessionMeta, meta)
	return store.save()
}

// defaultShutdownTimeout 是退出时等待运行收尾的上限。
const defaultShutdownTimeout = 8 * time.Second

// RunController 管理异步运行与事件推送。
// 运行上下文绑定应用生命周期，而不是单次 binding 调用寿命；
// 用户取消与退出收尾仍走显式 cancel/Shutdown，不丢失 Journal 合同。
type RunController struct {
	mu       sync.Mutex
	service  app.Service
	emit     Emitter
	prefs    *PrefsStore
	cancel   map[string]context.CancelFunc
	session  map[string]string
	lastSeq  map[string]uint64
	running  map[string]bool
	steering map[string]*agent.SteeringBridge
	baseCtx  context.Context
	baseStop context.CancelFunc
	wg       sync.WaitGroup
	closing  bool
}

// NewRunController 创建控制器；emit 可为 nil。
// 控制器自带应用级上下文，后台运行不再绑定 binding ctx。
func NewRunController(service app.Service, prefs *PrefsStore, emit Emitter) *RunController {
	baseCtx, baseStop := context.WithCancel(context.Background())
	return &RunController{
		service:  service,
		emit:     emit,
		prefs:    prefs,
		cancel:   map[string]context.CancelFunc{},
		session:  map[string]string{},
		lastSeq:  map[string]uint64{},
		running:  map[string]bool{},
		steering: map[string]*agent.SteeringBridge{},
		baseCtx:  baseCtx,
		baseStop: baseStop,
	}
}

// runContext 返回绑定应用生命周期的运行上下文。
// 不传入 binding ctx，避免 Wails 在方法返回时取消后台任务。
func (controller *RunController) runContext() (context.Context, context.CancelFunc, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.closing {
		return nil, nil, errInvalid("desktop controller is shutting down")
	}
	ctx, cancel := context.WithCancel(controller.baseCtx)
	return ctx, cancel, nil
}

// launch 在关闭屏障内登记后台任务；Shutdown 开始后禁止新增 WaitGroup 计数。
func (controller *RunController) launch(work func()) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.closing {
		return errInvalid("desktop controller is shutting down")
	}
	controller.wg.Add(1)
	go func() { defer controller.wg.Done(); work() }()
	return nil
}

// registerRun 在 ACK 前登记取消、补充输入和会话身份，拒绝重复登记。
func (controller *RunController) registerRun(runID, sessionID string, cancel context.CancelFunc, steering *agent.SteeringBridge) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.closing {
		return errInvalid("desktop controller is shutting down")
	}
	if controller.running[runID] {
		return errInvalid("run already active in this window")
	}
	controller.cancel[runID] = cancel
	controller.running[runID] = true
	if sessionID != "" {
		controller.session[runID] = sessionID
	}
	if steering != nil {
		controller.steering[runID] = steering
	}
	return nil
}

// unregisterRun 在执行退出时移除瞬态入口，Journal 中的暂停或终态保持不变。
func (controller *RunController) unregisterRun(runID string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	delete(controller.cancel, runID)
	delete(controller.running, runID)
	delete(controller.steering, runID)
}

// runScope 只在当前执行中传递钩子的原始工作区、状态目录与冻结配置，不成为运行事实源。
type runScope struct {
	Workspace string
	StateDir  string
	Config    config.Config
}

// Shutdown 有界停止全部运行：先合作式取消，再等待退出或超时。
func (controller *RunController) Shutdown(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	controller.mu.Lock()
	controller.closing = true
	cancels := make([]context.CancelFunc, 0, len(controller.cancel))
	for _, cancel := range controller.cancel {
		cancels = append(cancels, cancel)
	}
	controller.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	controller.baseStop()

	done := make(chan struct{})
	go func() {
		controller.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return errInvalid("shutdown timed out waiting for runs")
	}
}

// SetEmitter 热更新事件出口。
func (controller *RunController) SetEmitter(emit Emitter) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.emit = emit
}

func (controller *RunController) push(event StreamEvent) {
	controller.mu.Lock()
	emit := controller.emit
	controller.mu.Unlock()
	if emit != nil {
		emit(event)
	}
}

// IsRunning 报告 runID 是否仍在本进程执行。
func (controller *RunController) IsRunning(runID string) bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.running[runID]
}

// Cancel 合作式取消运行。
func (controller *RunController) Cancel(runID, reason string) error {
	controller.mu.Lock()
	cancel, ok := controller.cancel[runID]
	controller.mu.Unlock()
	if !ok {
		return os.ErrNotExist
	}
	cancel()
	_ = reason
	return nil
}

// StartRun 异步启动新任务。
func (controller *RunController) StartRun(ctx context.Context, request StartRunRequest) (StartRunAck, error) {
	if strings.TrimSpace(request.Task) == "" {
		return StartRunAck{}, errInvalid("task is required")
	}
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return StartRunAck{}, err
	}
	// 先校验工作模式，未知值失败关闭，不进入配置装载。
	if err := validateMode(request.Mode); err != nil {
		return StartRunAck{}, err
	}
	forkSourceID := strings.TrimSpace(request.ForkSourceRunID)
	if forkSourceID == "" {
		forkSourceID = strings.TrimSpace(request.ForkSource)
	}
	if forkSourceID != "" && strings.TrimSpace(request.PrevRunID) != "" {
		return StartRunAck{}, errInvalid("fork_source is mutually exclusive with prev_run_id")
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return StartRunAck{}, err
	}
	if err := applyRuntimeOverrides(&configuration, request.Model, request.Mode); err != nil {
		return StartRunAck{}, err
	}
	options := app.RunOptions{
		Task:      request.Task,
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
	}
	options.SessionID = request.SessionID
	if options.SessionID == "" {
		options.SessionID = domain.NewID("session")
	}
	if request.PrevRunID != "" {
		options.PrevRunID = domain.RunID(request.PrevRunID)
	}
	options.RequireSessionTail = request.RequireSessionTail
	if forkSourceID != "" {
		anchor, resolveErr := controller.service.ResolveSessionFork(context.Background(), workspace, resolveStateDir(request.StateDir), configuration, domain.RunID(forkSourceID))
		if resolveErr != nil {
			return StartRunAck{}, resolveErr
		}
		source := anchor.Source
		options.ForkSource = &source
	}
	// 预分配 runID：ACK 立即返回真实身份，覆盖可按 run 复现冻结指纹。
	runID := string(domain.NewRunID())
	options.PreassignedRunID = domain.RunID(runID)
	runCtx, cancel, err := controller.runContext()
	if err != nil {
		return StartRunAck{}, err
	}
	if controller.prefs != nil {
		if touchErr := controller.prefs.TouchProject(workspace); touchErr != nil {
			cancel()
			return StartRunAck{}, touchErr
		}
		if saveErr := controller.prefs.SaveRunOverride(RunOverride{
			RunID: runID,
			Model: request.Model,
			Mode:  request.Mode,
		}); saveErr != nil {
			cancel()
			return StartRunAck{}, saveErr
		}
	}
	bridge := agent.NewSteeringBridge()
	options.Steering = bridge
	if err := controller.registerRun(runID, options.SessionID, cancel, bridge); err != nil {
		cancel()
		return StartRunAck{}, err
	}
	if err := controller.launch(func() { controller.execute(runCtx, cancel, options, options.SessionID) }); err != nil {
		controller.unregisterRun(runID)
		cancel()
		return StartRunAck{}, err
	}
	return StartRunAck{RunID: runID, SessionID: options.SessionID, State: "starting"}, nil
}

// SubmitSteering 向运行中的任务投递立即补充输入（与后续队列语义分离）。
func (controller *RunController) SubmitSteering(runID, inputID, content string) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(content) == "" {
		return errInvalid("run_id and content are required")
	}
	controller.mu.Lock()
	bridge := controller.steering[runID]
	running := controller.running[runID]
	controller.mu.Unlock()
	if !running || bridge == nil {
		return errInvalid("run is not active for steering")
	}
	if strings.TrimSpace(inputID) == "" {
		inputID = domain.NewID("in")
	}
	return bridge.Submit(inputID, content)
}

// ResumeRun 异步恢复并携带一次性审批决定。
func (controller *RunController) ResumeRun(ctx context.Context, request ResumeRequest) (StartRunAck, error) {
	if request.RunID == "" || request.Workspace == "" {
		return StartRunAck{}, errInvalid("run_id and workspace are required")
	}
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return StartRunAck{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return StartRunAck{}, err
	}
	// 与启动相同的运行时覆盖优先级：请求显式值 > 已记录覆盖。
	model, mode := request.Model, request.Mode
	if controller.prefs != nil {
		if override, ok := controller.prefs.LoadRunOverride(request.RunID); ok {
			if model == "" {
				model = override.Model
			}
			if mode == "" {
				mode = override.Mode
			}
		}
	}
	if err := applyRuntimeOverrides(&configuration, model, mode); err != nil {
		return StartRunAck{}, err
	}
	options := app.ResumeOptions{
		RunID:     domain.RunID(request.RunID),
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
		Approval:  portsApproval(request),
	}
	runCtx, cancel, err := controller.runContext()
	if err != nil {
		return StartRunAck{}, err
	}
	bridge := agent.NewSteeringBridge()
	options.Steering = bridge
	if err := controller.registerRun(request.RunID, "", cancel, bridge); err != nil {
		cancel()
		return StartRunAck{}, err
	}
	if err := controller.launch(func() {
		defer controller.unregisterRun(request.RunID)
		defer cancel()
		options.OnEvent = controller.eventSink(request.RunID, "", workspace)
		outcome, runErr := controller.service.Resume(runCtx, options)
		controller.finish(request.RunID, outcome, runErr, runScope{workspace, options.StateDir, configuration})
	}); err != nil {
		controller.unregisterRun(request.RunID)
		cancel()
		return StartRunAck{}, err
	}
	return StartRunAck{RunID: request.RunID, State: "resuming"}, nil
}

// AbortRun 收尾运行。
func (controller *RunController) AbortRun(ctx context.Context, request AbortRequest) (RunStatusView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return RunStatusView{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return RunStatusView{}, err
	}
	outcome, runErr := controller.service.Abort(ctx, app.AbortOptions{
		RunID:     domain.RunID(request.RunID),
		Reason:    request.Reason,
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
		OnEvent:   controller.eventSink(request.RunID, "", workspace),
	})
	view := controller.statusFromOutcome(request.RunID, outcome, runErr)
	if runErr != nil {
		body := MapError(runErr)
		controller.push(StreamEvent{Workspace: workspace, Kind: "error", RunID: request.RunID, Error: &body, Status: &view})
		return view, runErr
	}
	controller.push(StreamEvent{Workspace: workspace, Kind: "run.finished", RunID: request.RunID, Status: &view})
	return view, nil
}

// ResolveReconcile 记录悬而未决副作用的对账判定。
func (controller *RunController) ResolveReconcile(ctx context.Context, request ResolveRequest) (RunStatusView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return RunStatusView{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return RunStatusView{}, err
	}
	outcome, runErr := controller.service.Reconcile(ctx, app.ReconcileOptions{
		RunID:       domain.RunID(request.RunID),
		OperationID: domainOperationID(request.OperationID),
		Verdict:     request.Verdict,
		Note:        request.Note,
		Evidence:    request.Evidence,
		Workspace:   workspace,
		StateDir:    resolveStateDir(request.StateDir),
		Config:      configuration,
		OnEvent:     controller.eventSink(request.RunID, "", workspace),
	})
	view := controller.statusFromOutcome(request.RunID, outcome, runErr)
	if runErr != nil {
		body := MapError(runErr)
		controller.push(StreamEvent{Workspace: workspace, Kind: "error", RunID: request.RunID, Error: &body, Status: &view})
		return view, runErr
	}
	controller.push(StreamEvent{Workspace: workspace, Kind: "run.finished", RunID: request.RunID, Status: &view})
	return view, nil
}

// execute 执行已登记的新运行，所有退出路径都移除瞬态入口并推送真实结果。
func (controller *RunController) execute(ctx context.Context, cancel context.CancelFunc, options app.RunOptions, sessionHint string) {
	runID := string(options.PreassignedRunID)
	defer controller.unregisterRun(runID)
	defer cancel()
	options.OnEvent = controller.eventSink(runID, sessionHint, options.Workspace)
	outcome, err := controller.service.Run(ctx, options)
	controller.finish(runID, outcome, err, runScope{options.Workspace, options.StateDir, options.Config})
}

// finish 发送当前调用的结果，仅真实执行成功时触发该工作区的有限钩子。
func (controller *RunController) finish(runID string, outcome app.RunResult, err error, scope runScope) {
	if outcome.SessionID != "" {
		controller.mu.Lock()
		controller.session[runID] = outcome.SessionID
		controller.mu.Unlock()
	}
	view := controller.statusFromOutcome(runID, outcome, err)
	if err != nil {
		body := MapError(err)
		kind := "run.finished"
		switch body.Code {
		case CodeWaitingApproval:
			kind = "run.waiting_approval"
			view.WaitingApproval = true
		case CodeWaitingReconcile:
			kind = "run.waiting_reconcile"
			view.WaitingReconcile = true
		}
		if body.RunID == "" {
			body.RunID = runID
		}
		controller.push(StreamEvent{Workspace: scope.Workspace, Kind: kind, RunID: runID, SessionID: outcome.SessionID, Status: &view, Error: &body})
		return
	}
	controller.push(StreamEvent{Workspace: scope.Workspace, Kind: "run.finished", RunID: runID, SessionID: outcome.SessionID, Status: &view})
	if outcome.Result != nil && outcome.Result.State == string(domain.RunCompleted) {
		controller.dispatchHooks("run_finished", runID, scope)
	}
}

func (controller *RunController) statusFromOutcome(runID string, outcome app.RunResult, err error) RunStatusView {
	view := RunStatusView{
		RunID:     runID,
		SessionID: outcome.SessionID,
		State:     resultState(outcome, err),
	}
	if outcome.Response != "" {
		view.ResponsePreview = previewResponse(outcome.Response)
	}
	if err != nil {
		body := MapError(err)
		view.ErrorCode = body.Code
		view.ErrorMessage = body.Message
	}
	return view
}

// eventSink 将应用层事件映射为 run.event 流，并按 run 隔离丢弃重复/迟到序号。
func (controller *RunController) eventSink(runID, sessionID string, workspace ...string) app.EventSink {
	sourceWorkspace := ""
	if len(workspace) > 0 {
		sourceWorkspace = workspace[0]
	}
	return func(event domain.Event) {
		target := runID
		if target == "" {
			target = string(event.RunID)
		}
		controller.mu.Lock()
		if target != "" {
			if event.Seq <= controller.lastSeq[target] {
				controller.mu.Unlock()
				return
			}
			controller.lastSeq[target] = event.Seq
		}
		if event.Type == domain.EventRunCreated {
			var created domain.RunCreatedPayload
			if json.Unmarshal(event.Payload, &created) == nil && created.SessionID != "" {
				controller.session[target] = created.SessionID
			}
		}
		targetSession := sessionID
		if targetSession == "" {
			targetSession = controller.session[target]
		}
		controller.mu.Unlock()
		payload := event
		controller.push(StreamEvent{
			Workspace:  sourceWorkspace,
			Kind:       "run.event",
			RunID:      target,
			SessionID:  targetSession,
			Seq:        event.Seq,
			OccurredAt: event.OccurredAt,
			Event:      &payload,
		})
	}
}

// ConfirmQueue 确认启动队列项：复用应用层幂等关联，事件接控制器，
// 运行上下文绑定应用生命周期。重复确认不重复执行。
func (controller *RunController) ConfirmQueue(ctx context.Context, request QueueActionRequest) (StartRunAck, error) {
	if err := ctx.Err(); err != nil {
		return StartRunAck{}, err
	}
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return StartRunAck{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return StartRunAck{}, err
	}
	if err := applyRuntimeOverrides(&configuration, request.Model, request.Mode); err != nil {
		return StartRunAck{}, err
	}
	runCtx, cancel, err := controller.runContext()
	if err != nil {
		return StartRunAck{}, err
	}
	ackCh := make(chan StartRunAck, 1)
	resultCh := make(chan queueConfirmResult, 1)
	bridge := agent.NewSteeringBridge()
	options := app.QueueConfirmOptions{
		Workspace: workspace, StateDir: resolveStateDir(request.StateDir), Config: configuration, InputID: request.InputID,
	}
	if err := controller.launch(func() {
		defer cancel()
		registeredID := ""
		defer func() {
			if registeredID != "" {
				controller.unregisterRun(registeredID)
			}
		}()
		options.OnReserved = func(runID domain.RunID, sessionID string) error {
			if err := runCtx.Err(); err != nil {
				return err
			}
			if controller.prefs != nil {
				if err := controller.prefs.SaveRunOverride(RunOverride{RunID: string(runID), Model: request.Model, Mode: request.Mode}); err != nil {
					return err
				}
			}
			if err := controller.registerRun(string(runID), sessionID, cancel, bridge); err != nil {
				return err
			}
			registeredID = string(runID)
			ackCh <- StartRunAck{RunID: registeredID, SessionID: sessionID, State: "queued_starting"}
			return nil
		}
		outcome, runErr := controller.service.ConfirmQueueInputWithEvents(runCtx, options, controller.eventSink("", "", workspace), bridge)
		if registeredID != "" {
			controller.finish(registeredID, outcome, runErr, runScope{workspace, options.StateDir, configuration})
		}
		resultCh <- queueConfirmResult{Outcome: outcome, Err: runErr}
	}); err != nil {
		cancel()
		return StartRunAck{}, err
	}
	select {
	case ack := <-ackCh:
		return ack, nil
	case done := <-resultCh:
		select {
		case ack := <-ackCh:
			return ack, nil
		default:
		}
		if done.Err != nil {
			return StartRunAck{}, done.Err
		}
		// 已存在的目标只回传 Journal 状态，不再发送一次假的完成事件。
		view, err := controller.service.Inspect(ctx, workspace, options.StateDir, configuration, done.Outcome.ID, false)
		if err != nil {
			return StartRunAck{}, err
		}
		return StartRunAck{RunID: string(done.Outcome.ID), SessionID: view.Projection.SessionID, State: view.Projection.Phase()}, nil
	case <-ctx.Done():
		cancel()
		return StartRunAck{}, ctx.Err()
	case <-controller.baseCtx.Done():
		return StartRunAck{}, errInvalid("queue confirm cancelled before start")
	}
}

// queueConfirmResult 将预留失败或幂等命中的真实结果送回调用方，不依赖定时猜测。
type queueConfirmResult struct {
	Outcome app.RunResult
	Err     error
}

// ListHooks 返回工作区启用的钩子规则。
func (store *PrefsStore) ListHooks(workspace string) []HookRule {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]HookRule, 0)
	for _, rule := range store.data.Hooks {
		if rule.Workspace == workspace {
			rule.Actions = append([]HookAction(nil), rule.Actions...)
			out = append(out, rule)
		}
	}
	return out
}

// UpsertHook 写入钩子规则；仅允许有限动作类型。
func (store *PrefsStore) UpsertHook(rule HookRule) error {
	if strings.TrimSpace(rule.ID) == "" || strings.TrimSpace(rule.Workspace) == "" {
		return errInvalid("hook id and workspace are required")
	}
	if rule.Event != "run_finished" || len(rule.Actions) == 0 {
		return errInvalid("hook requires run_finished and at least one action")
	}
	rule.Actions = append([]HookAction(nil), rule.Actions...)
	for _, action := range rule.Actions {
		if action.Kind != "queue_followup" || strings.TrimSpace(action.Content) == "" {
			return errInvalid("unsupported hook action")
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.Hooks {
		if store.data.Hooks[i].ID == rule.ID {
			store.data.Hooks[i] = rule
			return store.save()
		}
	}
	store.data.Hooks = append(store.data.Hooks, rule)
	return store.save()
}

// DisableHook 停用钩子（熔断或用户操作）。
func (store *PrefsStore) DisableHook(id string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.Hooks {
		if store.data.Hooks[i].ID == id {
			store.data.Hooks[i].Enabled = false
			return store.save()
		}
	}
	return os.ErrNotExist
}

// dispatchHooks 在真实事件后执行有限钩子动作；失败累计熔断，不扩大权限。
func (controller *RunController) dispatchHooks(eventName, runID string, scope runScope) {
	if controller.prefs == nil || runID == "" || scope.Workspace == "" {
		return
	}
	for _, rule := range controller.prefs.ListHooks(scope.Workspace) {
		if !rule.Enabled || rule.Event != eventName {
			continue
		}
		failed := false
		for _, action := range rule.Actions {
			if action.Kind != "queue_followup" {
				failed = true
				break
			}
			if _, err := controller.service.SubmitQueueInput(controller.baseCtx, app.QueueSubmitOptions{
				Workspace: scope.Workspace, StateDir: scope.StateDir, Config: scope.Config,
				Content: action.Content, InputID: domain.NewID("in"),
			}); err != nil {
				failed = true
				break
			}
		}
		if failed {
			_ = controller.prefs.BumpHookFailure(rule.ID)
		} else {
			_ = controller.prefs.ResetHookFailure(rule.ID)
		}
	}
}

// ListAllHooks 返回全部钩子规则。
func (store *PrefsStore) ListAllHooks() []HookRule {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]HookRule, len(store.data.Hooks))
	copy(out, store.data.Hooks)
	return out
}

// BumpHookFailure 累计失败并在达到阈值时停用（熔断）。
func (store *PrefsStore) BumpHookFailure(id string) error {
	const hookFailThreshold = 3
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.Hooks {
		if store.data.Hooks[i].ID == id {
			store.data.Hooks[i].FailStreak++
			if store.data.Hooks[i].FailStreak >= hookFailThreshold {
				store.data.Hooks[i].Enabled = false
			}
			return store.save()
		}
	}
	return os.ErrNotExist
}

// ResetHookFailure 清零失败计数。
func (store *PrefsStore) ResetHookFailure(id string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.Hooks {
		if store.data.Hooks[i].ID == id {
			store.data.Hooks[i].FailStreak = 0
			return store.save()
		}
	}
	return os.ErrNotExist
}

// WorkflowStep 是有限工作流步骤；仅支持 agent_run。
type WorkflowStep struct {
	Task  string `json:"task"`
	Model string `json:"model,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

// WorkflowDefinition 是声明式工作流模板；编辑不改变已运行实例。
type WorkflowDefinition struct {
	ID        string         `json:"id"`
	Workspace string         `json:"workspace"`
	Name      string         `json:"name"`
	Steps     []WorkflowStep `json:"steps"`
	// MaxSteps 硬上限，防止无界循环。
	MaxSteps int `json:"max_steps,omitempty"`
}

// validateWorkflow 锁定有限步骤数及已知覆盖模式；预演仅证明计划结构合法。
func validateWorkflow(def WorkflowDefinition) error {
	if strings.TrimSpace(def.ID) == "" || strings.TrimSpace(def.Workspace) == "" || strings.TrimSpace(def.Name) == "" {
		return errInvalid("workflow id/workspace/name required")
	}
	const maxWorkflowSteps = 8
	if len(def.Steps) == 0 {
		return errInvalid("workflow requires steps")
	}
	if def.MaxSteps < 0 || def.MaxSteps > maxWorkflowSteps || len(def.Steps) > maxWorkflowSteps || def.MaxSteps > 0 && len(def.Steps) > def.MaxSteps {
		return errInvalid("workflow steps exceed limit")
	}
	for _, step := range def.Steps {
		if strings.TrimSpace(step.Task) == "" {
			return errInvalid("workflow step task required")
		}
		if err := validateMode(step.Mode); err != nil {
			return err
		}
	}
	return nil
}

// SaveWorkflow 保存结构合法的定义，并复制切片防止后续编辑改变已启动实例。
func (store *PrefsStore) SaveWorkflow(def WorkflowDefinition) error {
	if err := validateWorkflow(def); err != nil {
		return err
	}
	def.Steps = append([]WorkflowStep(nil), def.Steps...)
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.Workflows {
		if store.data.Workflows[i].ID == def.ID {
			store.data.Workflows[i] = def
			return store.save()
		}
	}
	store.data.Workflows = append(store.data.Workflows, def)
	return store.save()
}

// ListWorkflows 列出工作区工作流。
func (store *PrefsStore) ListWorkflows(workspace string) []WorkflowDefinition {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]WorkflowDefinition, 0)
	for _, def := range store.data.Workflows {
		if def.Workspace == workspace {
			def.Steps = append([]WorkflowStep(nil), def.Steps...)
			out = append(out, def)
		}
	}
	return out
}

// WorkflowRunAck 是工作流启动回执。
type WorkflowRunAck struct {
	WorkflowID string   `json:"workflow_id"`
	RunIDs     []string `json:"run_ids,omitempty"`
	State      string   `json:"state"`
}

// RunWorkflow 按顺序执行工作流中的 agent 步骤；每步走真实 Run 授权链。
// 预演模式只校验计划，不启动执行。
func (controller *RunController) RunWorkflow(ctx context.Context, workspace, workflowID string, dryRun bool) (WorkflowRunAck, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return WorkflowRunAck{}, err
	}
	if controller.prefs == nil {
		return WorkflowRunAck{}, errInvalid("preferences store unavailable")
	}
	var def *WorkflowDefinition
	for _, item := range controller.prefs.ListWorkflows(abs) {
		if item.ID == workflowID {
			copied := item
			def = &copied
			break
		}
	}
	if def == nil {
		return WorkflowRunAck{}, errInvalid("workflow not found")
	}
	if err := validateWorkflow(*def); err != nil {
		return WorkflowRunAck{}, err
	}
	if err := ctx.Err(); err != nil {
		return WorkflowRunAck{}, err
	}
	if dryRun {
		return WorkflowRunAck{WorkflowID: def.ID, State: "dry_run_ok"}, nil
	}
	// 启动前冻结各步骤的有效配置，后续编辑配置或模板不改变当前实例。
	base, err := loadConfiguration(abs, "")
	if err != nil {
		return WorkflowRunAck{}, err
	}
	prepared := make([]app.RunOptions, 0, len(def.Steps))
	sessionID := domain.NewID("session")
	for _, step := range def.Steps {
		configuration := base
		if err := applyRuntimeOverrides(&configuration, step.Model, step.Mode); err != nil {
			return WorkflowRunAck{}, err
		}
		if err := config.Validate(configuration); err != nil {
			return WorkflowRunAck{}, err
		}
		prepared = append(prepared, app.RunOptions{
			Task: step.Task, Workspace: abs, StateDir: resolveStateDir(""), Config: configuration,
			PreassignedRunID: domain.NewRunID(), SessionID: sessionID, Steering: agent.NewSteeringBridge(),
		})
	}
	runCtx, cancel, err := controller.runContext()
	if err != nil {
		return WorkflowRunAck{}, err
	}
	firstID := string(prepared[0].PreassignedRunID)
	if err := controller.registerRun(firstID, sessionID, cancel, prepared[0].Steering); err != nil {
		cancel()
		return WorkflowRunAck{}, err
	}
	if err := controller.launch(func() {
		defer cancel()
		defer controller.unregisterRun(firstID)
		var previous domain.RunID
		for index, options := range prepared {
			if err := runCtx.Err(); err != nil {
				return
			}
			runID := string(options.PreassignedRunID)
			if index > 0 {
				options.PrevRunID = previous
				options.RequireSessionTail = true
				if err := controller.registerRun(runID, sessionID, cancel, options.Steering); err != nil {
					controller.push(StreamEvent{Workspace: abs, Kind: "error", RunID: runID, Error: mapErrorBody(err)})
					return
				}
			}
			step := def.Steps[index]
			if err := controller.prefs.SaveRunOverride(RunOverride{RunID: runID, Model: step.Model, Mode: step.Mode}); err != nil {
				controller.unregisterRun(runID)
				controller.push(StreamEvent{Workspace: abs, Kind: "error", RunID: runID, Error: mapErrorBody(err)})
				return
			}
			options.OnEvent = controller.eventSink(runID, sessionID, abs)
			outcome, runErr := controller.service.Run(runCtx, options)
			controller.finish(runID, outcome, runErr, runScope{abs, options.StateDir, options.Config})
			controller.unregisterRun(runID)
			if runErr != nil || outcome.Result == nil || outcome.Result.State != string(domain.RunCompleted) {
				return
			}
			previous = outcome.ID
		}
	}); err != nil {
		controller.unregisterRun(firstID)
		cancel()
		return WorkflowRunAck{}, err
	}
	return WorkflowRunAck{WorkflowID: def.ID, RunIDs: []string{firstID}, State: "running"}, nil
}

// mapErrorBody 将错误映射为流错误体。
func mapErrorBody(err error) *ErrorBody {
	body := MapError(err)
	return &body
}

// ReviewNote 是行级审查反馈（本地展示元数据，不改写 Journal）。
type ReviewNote struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Relative  string `json:"relative"`
	Line      string `json:"line,omitempty"`
	Body      string `json:"body"`
}

// SaveReviewNote 写入审查备注。
func (store *PrefsStore) SaveReviewNote(note ReviewNote) error {
	if strings.TrimSpace(note.Workspace) == "" || strings.TrimSpace(note.Relative) == "" || strings.TrimSpace(note.Body) == "" {
		return errInvalid("review note requires workspace/relative/body")
	}
	if note.ID == "" {
		note.ID = domain.NewID("note")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.data.ReviewNotes = append(store.data.ReviewNotes, note)
	return store.save()
}

// ListReviewNotes 列出文件审查备注。
func (store *PrefsStore) ListReviewNotes(workspace, relative string) []ReviewNote {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]ReviewNote, 0)
	for _, note := range store.data.ReviewNotes {
		if note.Workspace == workspace && (relative == "" || note.Relative == relative) {
			out = append(out, note)
		}
	}
	return out
}
