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
	"github.com/fxbin/cairn/internal/tools"
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
		if hasMeta {
			view.Pinned = meta.Pinned
			view.Archived = meta.Archived
			if meta.Title != "" {
				view.Title = meta.Title
			}
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

// DiffFile 返回单文件统一 diff；scope 指定 staged/unstaged/untracked/all。
func (api *API) DiffFile(_ context.Context, workspace, relative string, scope string) (string, error) {
	return DiffFile(workspace, relative, DiffScope(scope))
}

// OpenPathInEditor 使用系统默认应用打开工作区内相对路径；与预览同一边界。
func (api *API) OpenPathInEditor(_ context.Context, workspace, relative string) (OKResult, error) {
	if err := OpenInEditor(workspace, relative); err != nil {
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

// SubmitSteering 向运行中的任务投递立即补充输入。
func (api *API) SubmitSteering(_ context.Context, request SteeringRequest) (OKResult, error) {
	if err := api.controller.SubmitSteering(request.RunID, request.InputID, request.Content); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ListHooks 列出工作区钩子规则（有限动作，不授予权限）。
func (api *API) ListHooks(_ context.Context, workspace string) ([]HookRule, error) {
	if api.prefs == nil {
		return nil, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	return api.prefs.ListHooks(abs), nil
}

// UpsertHook 创建或更新钩子规则；仅允许 queue_followup。
func (api *API) UpsertHook(_ context.Context, rule HookRule) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(rule.Workspace)
	if err != nil {
		return OKResult{}, err
	}
	rule.Workspace = abs
	if rule.Event == "" {
		rule.Event = "run_finished"
	}
	if rule.ID == "" {
		rule.ID = domain.NewID("hook")
	}
	if err := api.prefs.UpsertHook(rule); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// DisableHook 停用钩子。
func (api *API) DisableHook(_ context.Context, id string) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	if err := api.prefs.DisableHook(id); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ProjectInstructionsView 是规约发现的只读投影。
type ProjectInstructionsView struct {
	Documents []ProjectInstructionDoc `json:"documents"`
}

// ProjectInstructionDoc 单条规约来源与摘要。
type ProjectInstructionDoc struct {
	Source  string `json:"source,omitempty"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Bytes   int    `json:"bytes,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// ListProjectInstructions 发现工作区/用户 AGENTS.md，不解释正文命令。
func (api *API) ListProjectInstructions(_ context.Context, workspace string) (ProjectInstructionsView, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return ProjectInstructionsView{}, err
	}
	ws, err := tools.NewWorkspace(abs, nil, 0, 0, 0)
	if err != nil {
		return ProjectInstructionsView{}, err
	}
	snapshot, err := tools.LoadProjectInstructions(ws)
	if err != nil {
		return ProjectInstructionsView{}, err
	}
	out := make([]ProjectInstructionDoc, 0, len(snapshot.Documents))
	for _, doc := range snapshot.Documents {
		excerpt := ""
		if len(doc.Content) > 200 {
			excerpt = string(doc.Content[:200]) + "…"
		} else {
			excerpt = string(doc.Content)
		}
		out = append(out, ProjectInstructionDoc{
			Source:  string(doc.Source),
			Path:    doc.Path,
			SHA256:  doc.SourceSHA256,
			Bytes:   len(doc.Content),
			Excerpt: excerpt,
		})
	}
	return ProjectInstructionsView{Documents: out}, nil
}

// ListWorkflows 列出工作区工作流定义。
func (api *API) ListWorkflows(_ context.Context, workspace string) ([]WorkflowDefinition, error) {
	if api.prefs == nil {
		return nil, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	return api.prefs.ListWorkflows(abs), nil
}

// SaveWorkflow 保存工作流定义。
func (api *API) SaveWorkflow(_ context.Context, def WorkflowDefinition) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(def.Workspace)
	if err != nil {
		return OKResult{}, err
	}
	def.Workspace = abs
	if def.ID == "" {
		def.ID = domain.NewID("wfl")
	}
	if def.MaxSteps == 0 {
		def.MaxSteps = 8
	}
	if err := api.prefs.SaveWorkflow(def); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// RunWorkflow 预演或启动工作流。
func (api *API) RunWorkflow(ctx context.Context, workspace, workflowID string, dryRun bool) (WorkflowRunAck, error) {
	return api.controller.RunWorkflow(ctx, workspace, workflowID, dryRun)
}

// MemoryCandidateView 是记忆候选的只读投影。
type MemoryCandidateView struct {
	ID                  string `json:"id"`
	Kind                string `json:"kind"`
	RunID               string `json:"run_id,omitempty"`
	Outcome             string `json:"outcome"`
	Status              string `json:"status"`
	TrustedVerification bool   `json:"trusted_verification"`
	CreatedAt           string `json:"created_at,omitempty"`
	Summary             string `json:"summary,omitempty"`
}

// ListMemories 召回当前工作区记忆候选；空结果与失败有区别。
func (api *API) ListMemories(ctx context.Context, request WorkspaceRequest) ([]MemoryCandidateView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return nil, err
	}
	candidates, err := api.service.RecallMemory(ctx, app.MemoryRecallOptions{
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
		Limit:     50,
	})
	if err != nil {
		return nil, err
	}
	out := make([]MemoryCandidateView, 0, len(candidates))
	for _, item := range candidates {
		out = append(out, MemoryCandidateView{
			ID:                  item.ID,
			Kind:                item.Kind,
			RunID:               item.RunID,
			Outcome:             item.Outcome,
			Status:              item.Status,
			TrustedVerification: item.TrustedVerification,
			CreatedAt:           item.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	return out, nil
}

// RevokeMemory 撤销记忆候选；原因使用有限枚举，不接受自由备注。
func (api *API) RevokeMemory(ctx context.Context, request struct {
	Workspace   string `json:"workspace"`
	StateDir    string `json:"state_dir,omitempty"`
	CandidateID string `json:"candidate_id"`
	Reason      string `json:"reason"`
}) (MemoryCandidateView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return MemoryCandidateView{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return MemoryCandidateView{}, err
	}
	item, err := api.service.RevokeMemory(ctx, app.MemoryRevokeOptions{
		CandidateID: request.CandidateID,
		Reason:      request.Reason,
		Workspace:   workspace,
		StateDir:    resolveStateDir(request.StateDir),
		Config:      configuration,
	})
	if err != nil {
		return MemoryCandidateView{}, err
	}
	return MemoryCandidateView{
		ID:      item.ID,
		Kind:    item.Kind,
		Outcome: item.Outcome,
		Status:  item.Status,
	}, nil
}

// CommandInfo 描述一个内置命令能力。
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
}

// ListCommands 返回与 CLI/GUI 同源的内置命令目录（只读）。
func (api *API) ListCommands(_ context.Context) ([]CommandInfo, error) {
	return []CommandInfo{
		{Name: "run", Description: "启动一次 Agent 运行", Scope: "workspace"},
		{Name: "resume", Description: "恢复/审批/对账/Abort", Scope: "workspace"},
		{Name: "doctor", Description: "只读诊断", Scope: "workspace"},
		{Name: "provider", Description: "Provider 配置/探测/准入", Scope: "user"},
		{Name: "mcp", Description: "MCP 配置与本机认证查询", Scope: "user"},
		{Name: "skills", Description: "技能目录与安装", Scope: "user/workspace"},
		{Name: "memory", Description: "记忆召回/整理/演化", Scope: "workspace"},
		{Name: "queue", Description: "后续队列提交/确认/取消", Scope: "workspace"},
		{Name: "bench", Description: "冻结评测（显式本地配置）", Scope: "benchmark"},
	}, nil
}

// InstallSkill 从明确来源安装技能到原生目录；不执行包内脚本。
func (api *API) InstallSkill(_ context.Context, workspace, scope, source string) (SkillSummary, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return SkillSummary{}, err
	}
	store, err := api.service.SkillStoreFor(abs, scope)
	if err != nil {
		return SkillSummary{}, err
	}
	name, err := store.Install(source)
	if err != nil {
		return SkillSummary{}, err
	}
	return SkillSummary{Name: name, Scope: scope}, nil
}

// SetSkillEnabled 启用或停用技能；只读共享来源不可写入。
func (api *API) SetSkillEnabled(_ context.Context, workspace, scope, name string, enabled bool) (OKResult, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return OKResult{}, err
	}
	store, err := api.service.SkillStoreFor(abs, scope)
	if err != nil {
		return OKResult{}, err
	}
	if err := store.SetEnabled(name, enabled); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// RemoveSkill 卸载技能；不删除会话历史。
func (api *API) RemoveSkill(_ context.Context, workspace, scope, name string) (OKResult, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return OKResult{}, err
	}
	store, err := api.service.SkillStoreFor(abs, scope)
	if err != nil {
		return OKResult{}, err
	}
	if err := store.Remove(name); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ConsolidateMemories 显式整理记忆派生候选；结果来自真实存储，不覆盖 Journal。
func (api *API) ConsolidateMemories(ctx context.Context, request WorkspaceRequest) (int, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return 0, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return 0, err
	}
	results, err := api.service.ConsolidateMemory(ctx, app.MemoryConsolidateOptions{
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
	})
	if err != nil {
		return 0, err
	}
	return len(results), nil
}

// SaveReviewNote 保存行级审查备注（本地元数据）。
func (api *API) SaveReviewNote(_ context.Context, note ReviewNote) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(note.Workspace)
	if err != nil {
		return OKResult{}, err
	}
	note.Workspace = abs
	if err := api.prefs.SaveReviewNote(note); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ListReviewNotes 列出审查备注。
func (api *API) ListReviewNotes(_ context.Context, workspace, relative string) ([]ReviewNote, error) {
	if api.prefs == nil {
		return nil, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	return api.prefs.ListReviewNotes(abs, relative), nil
}

// ProviderProbeView 是显式探测的只读结论；不含密钥与消息正文。
type ProviderProbeView struct {
	EvidenceFingerprint       string `json:"evidence_fingerprint,omitempty"`
	ConfigFingerprint         string `json:"config_fingerprint,omitempty"`
	RequestProfileFingerprint string `json:"request_profile_fingerprint,omitempty"`
	ToolRoundtrip             string `json:"tool_roundtrip,omitempty"`
	UsageStreamed             bool   `json:"usage_streamed"`
	Result                    string `json:"result"`
	ProviderKind              string `json:"provider_kind,omitempty"`
	Model                     string `json:"model,omitempty"`
	Error                     string `json:"error,omitempty"`
}

// ProbeProvider 由用户显式发起合同探测；打开页面不自动消耗额度。
func (api *API) ProbeProvider(ctx context.Context, request WorkspaceRequest) (ProviderProbeView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return ProviderProbeView{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return ProviderProbeView{}, err
	}
	result, err := api.service.CheckProvider(ctx, app.CheckProviderOptions{
		Config:     configuration,
		Workspace:  workspace,
		StateDir:   resolveStateDir(request.StateDir),
		ProbeTools: true,
	})
	if err != nil {
		return ProviderProbeView{}, err
	}
	return ProviderProbeView{
		EvidenceFingerprint: result.EvidenceFingerprint, ConfigFingerprint: result.ConfigFingerprint, RequestProfileFingerprint: result.RequestProfileFingerprint,
		ToolRoundtrip: result.ToolRoundtrip, UsageStreamed: result.UsageStreamed,
		Result:       result.Result,
		ProviderKind: result.ProviderKind,
		Model:        result.Model,
		Error:        result.Error,
	}, nil
}

// WriteProjectInstructions 写入工作区 AGENTS.md（原子、固定路径）。
func (api *API) WriteProjectInstructions(_ context.Context, workspace, content string) (OKResult, error) {
	if err := WriteProjectInstructions(workspace, content); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// MemoryDetail 是记忆详情投影。
type MemoryDetail struct {
	Candidate MemoryCandidateView `json:"candidate"`
	Audit     []string            `json:"audit,omitempty"`
}

// InspectMemory 查看记忆来源链与审计；不授予权限。
func (api *API) InspectMemory(ctx context.Context, workspace, candidateID string) (MemoryDetail, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return MemoryDetail{}, err
	}
	configuration, err := loadConfiguration(abs, "")
	if err != nil {
		return MemoryDetail{}, err
	}
	result, err := api.service.InspectMemory(ctx, app.MemoryInspectOptions{
		CandidateID: candidateID,
		Workspace:   abs,
		Config:      configuration,
	})
	if err != nil {
		return MemoryDetail{}, err
	}
	audit := make([]string, 0, len(result.Audit))
	for _, entry := range result.Audit {
		audit = append(audit, entry.Action)
	}
	return MemoryDetail{
		Candidate: MemoryCandidateView{
			ID:      result.Candidate.ID,
			Kind:    result.Candidate.Kind,
			RunID:   result.Candidate.RunID,
			Outcome: result.Candidate.Outcome,
			Status:  result.Candidate.Status,
		},
		Audit: audit,
	}, nil
}

// ProviderPresetView 是命名连接预设的非敏感投影。
type ProviderPresetView struct {
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	Model     string `json:"model"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	Admission string `json:"admission"`
	Active    bool   `json:"active"`
}

// ListProviderPresets 列出命名连接预设；不返回密钥值。
func (api *API) ListProviderPresets(_ context.Context, request WorkspaceRequest) ([]ProviderPresetView, error) {
	abs, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	configuration, err := loadConfiguration(abs, request.StateDir)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderPresetView, 0, len(configuration.Provider.Presets))
	for name, preset := range configuration.Provider.Presets {
		protocol := preset.Protocol
		if protocol == "" {
			protocol = "openai-chat-completions"
		}
		admission := preset.Admission
		if admission == "" {
			admission = "required"
		}
		out = append(out, ProviderPresetView{
			Name: name, Protocol: protocol, Model: preset.Model,
			BaseURL: preset.BaseURL, APIKeyEnv: preset.APIKeyEnv,
			Admission: admission, Active: configuration.Provider.Use == name,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UseProviderPreset 切换激活的命名连接预设；仅影响下一新 run。
func (api *API) UseProviderPreset(_ context.Context, workspace, name string) (OKResult, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return OKResult{}, err
	}
	if strings.TrimSpace(name) == "" {
		return OKResult{}, errInvalid("preset name is required")
	}
	configuration, err := loadConfiguration(abs, "")
	if err != nil {
		return OKResult{}, err
	}
	if _, exists := configuration.Provider.Presets[name]; !exists {
		return OKResult{}, errInvalid("preset is not defined in provider.presets")
	}
	// 与 CLI writeProviderUse 同语义：清除内联连接字段并写入 use。
	// 配置文件原子替换，失败不落盘。
	path := config.ResolveDefaultPath(abs)
	if err := config.UseProviderPreset(path, name); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ProviderSetupRequest 是配置向导的显式输入。
type ProviderSetupRequest struct {
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKeyEnv    string `json:"api_key_env"`
	Workspace    string `json:"workspace"`
	Admission    string `json:"admission,omitempty"`
	Overwrite    bool   `json:"overwrite,omitempty"`
	UserScope    bool   `json:"user_scope,omitempty"`
	CatalogURL   string `json:"catalog_url,omitempty"`
	CatalogCache string `json:"catalog_cache,omitempty"`
}

// ProviderSetupView 是配置生成结果的非敏感投影。
type ProviderSetupView struct {
	ConfigPath     string   `json:"config_path"`
	APIKeyEnv      string   `json:"api_key_env"`
	CatalogMatched bool     `json:"catalog_matched"`
	ProbeRan       bool     `json:"probe_ran"`
	Notes          []string `json:"notes,omitempty"`
}

// RunProviderSetup 生成 v2 配置；覆盖既有配置需显式 overwrite。
func (api *API) RunProviderSetup(ctx context.Context, request ProviderSetupRequest) (ProviderSetupView, error) {
	workspace := request.Workspace
	if strings.TrimSpace(workspace) == "" {
		return ProviderSetupView{}, errInvalid("workspace is required")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return ProviderSetupView{}, err
	}
	if strings.TrimSpace(request.BaseURL) == "" || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.APIKeyEnv) == "" {
		return ProviderSetupView{}, errInvalid("base_url, model and api_key_env are required")
	}
	outcome, err := app.NewSetupService().Run(ctx, app.SetupInput{
		BaseURL:           request.BaseURL,
		Model:             request.Model,
		APIKeyEnv:         request.APIKeyEnv,
		CatalogURL:        request.CatalogURL,
		CatalogCacheDir:   request.CatalogCache,
		Workspace:         abs,
		Admission:         request.Admission,
		OverwriteExisting: request.Overwrite,
		ScopeUserConfig:   request.UserScope,
	})
	if err != nil {
		return ProviderSetupView{}, err
	}
	return ProviderSetupView{
		ConfigPath:     outcome.ConfigPath,
		APIKeyEnv:      outcome.APIKeyEnv,
		CatalogMatched: outcome.CatalogMatched,
		ProbeRan:       outcome.ProbeRan,
		Notes:          outcome.Notes,
	}, nil
}

// ProviderAdmissionRequest 携带用户刚复核的非敏感绑定，不接受密钥或自造身份。
type ProviderAdmissionRequest struct {
	Workspace                 string `json:"workspace"`
	StateDir                  string `json:"state_dir,omitempty"`
	EvidenceFingerprint       string `json:"evidence_fingerprint"`
	ConfigFingerprint         string `json:"config_fingerprint"`
	RequestProfileFingerprint string `json:"request_profile_fingerprint"`
	Reason                    string `json:"reason"`
	Supersedes                string `json:"supersedes,omitempty"`
}

// ConfirmProviderAdmission 重新装载当前配置并记录一次用户显式准入决定。
func (api *API) ConfirmProviderAdmission(ctx context.Context, request ProviderAdmissionRequest) (OKResult, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return OKResult{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return OKResult{}, err
	}
	_, err = api.service.ConfirmProviderAdmission(ctx, app.ConfirmProviderAdmissionOptions{
		Workspace: workspace, StateDir: resolveStateDir(request.StateDir), Config: configuration,
		EvidenceFingerprint: request.EvidenceFingerprint, ConfigFingerprint: request.ConfigFingerprint,
		RequestProfileFingerprint: request.RequestProfileFingerprint, Reason: request.Reason, Supersedes: request.Supersedes,
	})
	if err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}
