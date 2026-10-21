package desktop

import (
	"context"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// DirectoryPicker 由桌面入口注入原生目录选择对话框；desktop 包不依赖 Wails。
type DirectoryPicker func(title, defaultPath string) (string, error)

// API 是绑定到 Wails 的桌面服务：全部入参校验，只读与运行动作均经 app.Service。
// 前端通过统一 bindings 调用，不直接访问 Journal 或宿主 shell。
// 注意：事件出口与目录对话框由桌面入口注入，不导出为前端可调用方法。
type API struct {
	service    app.Service
	controller *RunController
	prefs      *PrefsStore
	picker     DirectoryPicker
}

// NewAPI 组装桌面服务；prefs 为 nil 时偏好读写返回错误。
func NewAPI(service app.Service, prefs *PrefsStore, emit Emitter) *API {
	return &API{
		service:    service,
		controller: NewRunController(service, prefs, emit),
		prefs:      prefs,
	}
}

// AttachEmitter 由桌面入口在窗口重建后更新事件出口；不导出到前端 bindings。
func (api *API) AttachEmitter(emit Emitter) {
	api.controller.SetEmitter(emit)
}

// AttachDirectoryPicker 注入原生目录选择器；不导出到前端 bindings。
func (api *API) AttachDirectoryPicker(picker DirectoryPicker) {
	api.picker = picker
}

// PickDirectory 打开原生目录选择并返回绝对路径；未注入对话框时失败关闭。
func (api *API) PickDirectory(_ context.Context, title, defaultPath string) (string, error) {
	if api.picker == nil {
		return "", errInvalid("directory picker is unavailable in this runtime")
	}
	path, err := api.picker(title, defaultPath)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(path) == "" {
		return "", errInvalid("no directory selected")
	}
	return filepath.Abs(path)
}

// Health 返回桥接可用性与只读环境摘要。
func (api *API) Health(_ context.Context) (HealthResult, error) {
	userRoot, _ := userConfigRoot()
	return HealthResult{
		OK:        true,
		AppName:   "Cairn",
		Version:   "0.1.0",
		GoVersion: runtime.Version(),
		UserRoot:  userRoot,
	}, nil
}

// ObserveWorkspace 返回工作区与仓库只读状态，并尝试装载配置指纹。
func (api *API) ObserveWorkspace(_ context.Context, workspace string) (WorkspaceStatus, error) {
	status, err := WorkspaceObserve(workspace)
	if err != nil {
		return status, err
	}
	if status.Exists {
		configuration, loadErr := loadConfiguration(status.Path, "")
		if loadErr != nil {
			status.ConfigError = loadErr.Error()
			status.ConfigLoaded = false
		} else {
			status.ConfigLoaded = true
			status.Fingerprint = configuration.Fingerprint()
			status.ConfigSource = config.ResolveDefaultPath(status.Path)
		}
	}
	return status, nil
}

// ListRecentProjects 返回 UI 最近项目。
func (api *API) ListRecentProjects(_ context.Context, includeArchived bool) ([]RecentProject, error) {
	if api.prefs == nil {
		return []RecentProject{}, errInvalid("preferences store unavailable")
	}
	items := api.prefs.ListRecentProjects(includeArchived)
	if items == nil {
		items = []RecentProject{}
	}
	return items, nil
}

// RememberProject 记录最近项目。
func (api *API) RememberProject(_ context.Context, workspace string) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	if err := api.prefs.TouchProject(workspace); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ArchiveProject 归档或恢复最近项目展示。
func (api *API) ArchiveProject(_ context.Context, workspace string, archived bool) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	if err := api.prefs.SetProjectArchived(workspace, archived); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ListRuns 返回运行历史，并合并会话展示偏好。
func (api *API) ListRuns(ctx context.Context, request WorkspaceRequest) ([]RunHistoryView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return nil, err
	}
	items, err := api.service.ListRuns(ctx, workspace, resolveStateDir(request.StateDir), configuration)
	if err != nil {
		return nil, err
	}
	metaByID := map[string]SessionMeta{}
	if api.prefs != nil {
		for _, meta := range api.prefs.SessionPreferences() {
			metaByID[meta.SessionID] = meta
		}
	}
	out := make([]RunHistoryView, 0, len(items))
	query := strings.TrimSpace(strings.ToLower(request.Query))
	for _, item := range items {
		meta, hasMeta := metaByID[item.SessionID]
		if hasMeta && meta.Archived && !request.IncludeArchived {
			continue
		}
		view := RunHistoryView{RunHistoryItem: item, Title: firstLine(item.Task)}
		if hasMeta && meta.Title != "" {
			view.Title = meta.Title
		}
		if query != "" {
			haystack := strings.ToLower(view.Title + "\n" + item.Task + "\n" + string(item.ID) + "\n" + item.SessionID)
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		out = append(out, view)
	}
	// 置顶优先，再按更新时间倒序。
	sort.SliceStable(out, func(i, j int) bool {
		iMeta := metaByID[out[i].SessionID]
		jMeta := metaByID[out[j].SessionID]
		if iMeta.Pinned != jMeta.Pinned {
			return iMeta.Pinned
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// RenameSession 写入会话展示标题（不修改 Journal）。
func (api *API) RenameSession(_ context.Context, sessionID, title string) (OKResult, error) {
	if strings.TrimSpace(sessionID) == "" {
		return OKResult{}, errInvalid("session_id is required")
	}
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	meta := SessionMeta{SessionID: sessionID, Title: strings.TrimSpace(title)}
	for _, existing := range api.prefs.SessionPreferences() {
		if existing.SessionID == sessionID {
			meta = existing
			meta.Title = strings.TrimSpace(title)
			break
		}
	}
	if err := api.prefs.UpsertSessionMeta(meta); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// SetSessionFlags 更新会话置顶/归档展示。
func (api *API) SetSessionFlags(_ context.Context, sessionID string, pinned, archived bool) (OKResult, error) {
	if strings.TrimSpace(sessionID) == "" {
		return OKResult{}, errInvalid("session_id is required")
	}
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	meta := SessionMeta{SessionID: sessionID, Pinned: pinned, Archived: archived}
	for _, existing := range api.prefs.SessionPreferences() {
		if existing.SessionID == sessionID {
			meta = existing
			meta.Pinned = pinned
			meta.Archived = archived
			break
		}
	}
	if err := api.prefs.UpsertSessionMeta(meta); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// InspectRun 返回运行投影与可选事件。
func (api *API) InspectRun(ctx context.Context, request InspectRequest) (RunDetail, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return RunDetail{}, err
	}
	if strings.TrimSpace(request.RunID) == "" {
		return RunDetail{}, errInvalid("run_id is required")
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return RunDetail{}, err
	}
	inspected, err := api.service.Inspect(ctx, workspace, resolveStateDir(request.StateDir), configuration, domain.RunID(request.RunID), request.IncludeEvents)
	if err != nil {
		return RunDetail{}, err
	}
	detail := RunDetail{
		RunID:     request.RunID,
		SessionID: inspected.Projection.SessionID,
		State:     inspected.Projection.State,
		Phase:     inspected.Projection.Phase(),
		Task:      inspected.Projection.Task,
		Summary:   inspected.Summary,
		Manifest:  inspected.Manifest,
		Events:    inspected.Events,
	}
	return detail, nil
}

// EvidenceFor 返回指定运行的完成证据。
func (api *API) EvidenceFor(ctx context.Context, request InspectRequest) (contract.Result, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return contract.Result{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return contract.Result{}, err
	}
	return api.service.Evidence(ctx, workspace, resolveStateDir(request.StateDir), configuration, domain.RunID(request.RunID))
}

// StartRun 启动任务（异步）。
func (api *API) StartRun(ctx context.Context, request StartRunRequest) (StartRunAck, error) {
	return api.controller.StartRun(ctx, request)
}

// ResumeRun 恢复/审批（异步）。
func (api *API) ResumeRun(ctx context.Context, request ResumeRequest) (StartRunAck, error) {
	return api.controller.ResumeRun(ctx, request)
}

// AbortRun 收尾运行。
func (api *API) AbortRun(ctx context.Context, request AbortRequest) (RunStatusView, error) {
	return api.controller.AbortRun(ctx, request)
}

// ResolveReconcile 对账悬而未决副作用。
func (api *API) ResolveReconcile(ctx context.Context, request ResolveRequest) (RunStatusView, error) {
	return api.controller.ResolveReconcile(ctx, request)
}

// CancelRun 取消运行。
func (api *API) CancelRun(_ context.Context, request CancelRequest) (OKResult, error) {
	if err := api.controller.Cancel(request.RunID, request.Reason); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// Doctor 返回诊断摘要。
func (api *API) Doctor(ctx context.Context, request WorkspaceRequest) (app.DoctorReport, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return app.DoctorReport{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return app.DoctorReport{}, err
	}
	return api.service.Doctor(ctx, workspace, resolveStateDir(request.StateDir), configuration)
}

// MCPStatus 返回 MCP 声明与状态。
func (api *API) MCPStatus(_ context.Context, request WorkspaceRequest) ([]app.MCPServerStatus, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return nil, err
	}
	return api.service.MCPStatus(configuration)
}

// ListSkills 返回技能目录摘要。
func (api *API) ListSkills(_ context.Context, request WorkspaceRequest) ([]SkillSummary, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	catalog, err := api.service.Skills(workspace, true)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return []SkillSummary{}, nil
	}
	items := catalog.Entries()
	out := make([]SkillSummary, 0, len(items))
	for _, item := range items {
		out = append(out, SkillSummary{
			Name:        item.Name,
			Description: item.Description,
			Scope:       item.Scope,
			Disabled:    item.Disabled,
			ManualOnly:  item.ManualOnly,
		})
	}
	return out, nil
}

// SkillSummary 是技能只读摘要。
type SkillSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Disabled    bool   `json:"disabled"`
	ManualOnly  bool   `json:"manual_only"`
}

// ListFileTree 返回工作区有界文件树。
func (api *API) ListFileTree(_ context.Context, workspace string) (FileNode, error) {
	return ListFileTree(workspace)
}

// PreviewFile 返回有界文件预览。
func (api *API) PreviewFile(_ context.Context, workspace, relative string) (FilePreview, error) {
	return PreviewFile(workspace, relative)
}

// ListChanges 返回工作区改动列表（不猜测 Agent 归属）。
func (api *API) ListChanges(_ context.Context, workspace string) (ChangeSet, error) {
	return ListChanges(workspace)
}

// DiffFile 返回单文件统一 diff。
func (api *API) DiffFile(_ context.Context, workspace, relative string) (string, error) {
	return DiffFile(workspace, relative)
}

// OpenPathInEditor 使用系统默认应用打开路径。
func (api *API) OpenPathInEditor(_ context.Context, path string) (OKResult, error) {
	if err := OpenInEditor(path); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "(空任务)"
	}
	if idx := strings.IndexAny(text, "\r\n"); idx >= 0 {
		return strings.TrimSpace(text[:idx])
	}
	return text
}

// Shutdown 有界收尾异步运行，供桌面退出钩子调用。
func (api *API) Shutdown(timeout time.Duration) error {
	return api.controller.Shutdown(timeout)
}
