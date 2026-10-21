package desktop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
)

// 本文件实现桌面端异步运行控制器：初始化快照 + 增量事件，以 runID 会话隔离，
// 旧回调不得污染新会话。Event Journal 仍是事实源，这里只做通知与唤醒。

// Emitter 向前端推送 StreamEvent；由 Wails 层实现。
type Emitter func(StreamEvent)

// PrefsStore 读写 UI 偏好（最近项目、会话元数据）。偏好不进入 Journal。
type PrefsStore struct {
	mu   sync.RWMutex
	path string
	data Preferences
}

// Preferences 是 GUI 本地偏好。
type Preferences struct {
	RecentProjects []RecentProject `json:"recent_projects"`
	SessionMeta    []SessionMeta   `json:"session_meta"`
	Theme          string          `json:"theme,omitempty"`
	FontSize       int             `json:"font_size,omitempty"`
}

// NewPrefsStore 在用户配置根下打开偏好文件（不存在则空）。
func NewPrefsStore() (*PrefsStore, error) {
	root, err := userConfigRoot()
	if err != nil {
		return nil, err
	}
	store := &PrefsStore{path: filepath.Join(root, preferencesFile)}
	_ = store.load()
	return store, nil
}

// NewPrefsStoreAt 供测试指定偏好文件路径。
func NewPrefsStoreAt(path string) *PrefsStore {
	store := &PrefsStore{path: path}
	_ = store.load()
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
		store.data = Preferences{RecentProjects: []RecentProject{}, SessionMeta: []SessionMeta{}}
		return nil
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

func (store *PrefsStore) save() error {
	raw, err := json.MarshalIndent(store.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := store.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, store.path)
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
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return StartRunAck{}, err
	}
	if request.Model != "" {
		configuration.Provider.Model = request.Model
	}
	if err := applyMode(&configuration, request.Mode); err != nil {
		return StartRunAck{}, err
	}
	options := app.RunOptions{
		Task:      request.Task,
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
	}
	if request.SessionID != "" {
		options.SessionID = request.SessionID
	}
	if request.PrevRunID != "" {
		options.PrevRunID = domain.RunID(request.PrevRunID)
	}
	runCtx, cancel, err := controller.runContext()
	if err != nil {
		return StartRunAck{}, err
	}
	if controller.prefs != nil {
		if touchErr := controller.prefs.TouchProject(workspace); touchErr != nil {
			cancel()
			return StartRunAck{}, touchErr
		}
	}
	ack := StartRunAck{SessionID: request.SessionID, State: "starting"}
	controller.wg.Add(1)
	go controller.execute(runCtx, cancel, options, request.SessionID)
	return ack, nil
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
	controller.mu.Lock()
	if controller.running[request.RunID] {
		controller.mu.Unlock()
		cancel()
		return StartRunAck{}, errInvalid("run already active in this window")
	}
	controller.cancel[request.RunID] = cancel
	controller.running[request.RunID] = true
	controller.mu.Unlock()

	controller.wg.Add(1)
	go func() {
		defer controller.wg.Done()
		defer func() {
			controller.mu.Lock()
			delete(controller.cancel, request.RunID)
			delete(controller.running, request.RunID)
			controller.mu.Unlock()
			cancel()
		}()
		// Resume 路径必须持续推送工具/审批事件，与 Start 共用 eventSink。
		options.OnEvent = controller.eventSink(request.RunID, "")
		outcome, runErr := controller.service.Resume(runCtx, options)
		controller.finish(request.RunID, outcome, runErr)
	}()
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
		OnEvent:   controller.eventSink(request.RunID, ""),
	})
	view := controller.statusFromOutcome(request.RunID, outcome, runErr)
	if runErr != nil {
		body := MapError(runErr)
		controller.push(StreamEvent{Kind: "error", RunID: request.RunID, Error: &body, Status: &view})
		return view, runErr
	}
	controller.push(StreamEvent{Kind: "run.finished", RunID: request.RunID, Status: &view})
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
		OnEvent:     controller.eventSink(request.RunID, ""),
	})
	view := controller.statusFromOutcome(request.RunID, outcome, runErr)
	if runErr != nil {
		body := MapError(runErr)
		controller.push(StreamEvent{Kind: "error", RunID: request.RunID, Error: &body, Status: &view})
		return view, runErr
	}
	controller.push(StreamEvent{Kind: "run.finished", RunID: request.RunID, Status: &view})
	return view, nil
}

func (controller *RunController) execute(ctx context.Context, cancel context.CancelFunc, options app.RunOptions, sessionHint string) {
	defer controller.wg.Done()
	var registerMu sync.Mutex
	registered := false
	cleanup := func(runID string) {
		controller.mu.Lock()
		if runID != "" {
			delete(controller.cancel, runID)
			delete(controller.running, runID)
		}
		controller.mu.Unlock()
	}
	onEvent := func(event domain.Event) {
		registerMu.Lock()
		runID := string(event.RunID)
		if runID != "" && !registered {
			registered = true
			controller.mu.Lock()
			controller.cancel[runID] = cancel
			controller.running[runID] = true
			if sessionHint != "" {
				controller.session[runID] = sessionHint
			}
			controller.mu.Unlock()
		}
		seq := event.Seq
		controller.mu.Lock()
		if runID != "" {
			if seq <= controller.lastSeq[runID] {
				controller.mu.Unlock()
				registerMu.Unlock()
				return
			}
			controller.lastSeq[runID] = seq
		}
		sessionID := controller.session[runID]
		controller.mu.Unlock()
		payload := event
		controller.push(StreamEvent{
			Kind:       "run.event",
			RunID:      runID,
			SessionID:  sessionID,
			Seq:        seq,
			OccurredAt: event.OccurredAt,
			Event:      &payload,
		})
		registerMu.Unlock()
	}
	options.OnEvent = onEvent
	outcome, err := controller.service.Run(ctx, options)
	runID := string(outcome.ID)
	registerMu.Lock()
	if runID != "" && !registered {
		registered = true
		controller.mu.Lock()
		controller.cancel[runID] = cancel
		controller.running[runID] = true
		controller.mu.Unlock()
	}
	registerMu.Unlock()
	defer func() {
		cleanup(runID)
		cancel()
	}()
	controller.finish(runID, outcome, err)
}

func (controller *RunController) finish(runID string, outcome app.RunResult, err error) {
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
		controller.push(StreamEvent{Kind: kind, RunID: runID, SessionID: outcome.SessionID, Status: &view, Error: &body})
		return
	}
	controller.push(StreamEvent{Kind: "run.finished", RunID: runID, SessionID: outcome.SessionID, Status: &view})
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
func (controller *RunController) eventSink(runID, sessionID string) app.EventSink {
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
		if sessionID == "" {
			sessionID = controller.session[target]
		}
		controller.mu.Unlock()
		payload := event
		controller.push(StreamEvent{
			Kind:       "run.event",
			RunID:      target,
			SessionID:  sessionID,
			Seq:        event.Seq,
			OccurredAt: event.OccurredAt,
			Event:      &payload,
		})
	}
}
