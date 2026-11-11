package desktop

import (
	"context"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
)

// Service 是注册到 Wails 的前端门面：只暴露 UI 需要的用例方法。
// 事件出口注入、生命周期等桌面控制留在 API，不进入 bindings。
type Service struct {
	api *API
}

// NewService 创建前端门面。
func NewService(api *API) *Service {
	return &Service{api: api}
}

// Health 查询桥接健康状态。
func (service *Service) Health(ctx context.Context) (HealthResult, error) {
	return service.api.Health(ctx)
}

// ObserveWorkspace 查询工作区状态。
func (service *Service) ObserveWorkspace(ctx context.Context, workspace string) (WorkspaceStatus, error) {
	return service.api.ObserveWorkspace(ctx, workspace)
}

// PickDirectory 打开原生目录选择对话框。
func (service *Service) PickDirectory(ctx context.Context, title, defaultPath string) (string, error) {
	return service.api.PickDirectory(ctx, title, defaultPath)
}

// ListRecentProjects 列出最近项目。
func (service *Service) ListRecentProjects(ctx context.Context, includeArchived bool) ([]RecentProject, error) {
	return service.api.ListRecentProjects(ctx, includeArchived)
}

// RememberProject 记录最近项目。
func (service *Service) RememberProject(ctx context.Context, workspace string) (OKResult, error) {
	return service.api.RememberProject(ctx, workspace)
}

// ArchiveProject 归档最近项目展示。
func (service *Service) ArchiveProject(ctx context.Context, workspace string, archived bool) (OKResult, error) {
	return service.api.ArchiveProject(ctx, workspace, archived)
}

// ListRuns 列出运行历史。
func (service *Service) ListRuns(ctx context.Context, request WorkspaceRequest) ([]RunHistoryView, error) {
	return service.api.ListRuns(ctx, request)
}

// RenameSession 重命名会话展示标题。
func (service *Service) RenameSession(ctx context.Context, sessionID, title string) (OKResult, error) {
	return service.api.RenameSession(ctx, sessionID, title)
}

// SetSessionFlags 更新会话展示标记。
func (service *Service) SetSessionFlags(ctx context.Context, sessionID string, pinned, archived bool) (OKResult, error) {
	return service.api.SetSessionFlags(ctx, sessionID, pinned, archived)
}

// InspectRun 查询运行详情。
func (service *Service) InspectRun(ctx context.Context, request InspectRequest) (RunDetail, error) {
	return service.api.InspectRun(ctx, request)
}

// EvidenceFor 查询完成证据。
func (service *Service) EvidenceFor(ctx context.Context, request InspectRequest) (contract.Result, error) {
	return service.api.EvidenceFor(ctx, request)
}

// StartRun 启动任务。
func (service *Service) StartRun(ctx context.Context, request StartRunRequest) (StartRunAck, error) {
	return service.api.StartRun(ctx, request)
}

// ResumeRun 恢复或审批。
func (service *Service) ResumeRun(ctx context.Context, request ResumeRequest) (StartRunAck, error) {
	return service.api.ResumeRun(ctx, request)
}

// AbortRun 收尾运行。
func (service *Service) AbortRun(ctx context.Context, request AbortRequest) (RunStatusView, error) {
	return service.api.AbortRun(ctx, request)
}

// ResolveReconcile 对账悬而未决副作用。
func (service *Service) ResolveReconcile(ctx context.Context, request ResolveRequest) (RunStatusView, error) {
	return service.api.ResolveReconcile(ctx, request)
}

// CancelRun 取消运行。
func (service *Service) CancelRun(ctx context.Context, request CancelRequest) (OKResult, error) {
	return service.api.CancelRun(ctx, request)
}

// Doctor 运行诊断。
func (service *Service) Doctor(ctx context.Context, request WorkspaceRequest) (app.DoctorReport, error) {
	return service.api.Doctor(ctx, request)
}

// MCPStatus 查询 MCP 状态。
func (service *Service) MCPStatus(ctx context.Context, request WorkspaceRequest) ([]app.MCPServerStatus, error) {
	return service.api.MCPStatus(ctx, request)
}

// ListSkills 列出技能。
func (service *Service) ListSkills(ctx context.Context, request WorkspaceRequest) ([]SkillSummary, error) {
	return service.api.ListSkills(ctx, request)
}

// ListFileTree 列出有界文件树。
func (service *Service) ListFileTree(ctx context.Context, workspace string) (FileNode, error) {
	return service.api.ListFileTree(ctx, workspace)
}

// PreviewFile 预览文件。
func (service *Service) PreviewFile(ctx context.Context, workspace, relative string) (FilePreview, error) {
	return service.api.PreviewFile(ctx, workspace, relative)
}

// ListChanges 列出工作区改动。
func (service *Service) ListChanges(ctx context.Context, workspace string) (ChangeSet, error) {
	return service.api.ListChanges(ctx, workspace)
}

// DiffFile 返回文件 diff。
func (service *Service) DiffFile(ctx context.Context, workspace, relative string, scope string) (string, error) {
	return service.api.DiffFile(ctx, workspace, relative, scope)
}

// OpenPathInEditor 在外部编辑器打开工作区内相对路径。
func (service *Service) OpenPathInEditor(ctx context.Context, workspace, relative string) (OKResult, error) {
	return service.api.OpenPathInEditor(ctx, workspace, relative)
}

// ListOpenLocations 列出打开位置目标。
func (service *Service) ListOpenLocations(ctx context.Context, workspace string) ([]OpenLocationTarget, error) {
	return service.api.ListOpenLocations(ctx, workspace)
}

// OpenInLocationWithTarget 用指定目标打开路径。
func (service *Service) OpenInLocationWithTarget(ctx context.Context, workspace, relative, target string) (OKResult, error) {
	return service.api.OpenInLocationWithTarget(ctx, workspace, relative, target)
}

// ListQueue 列出后续队列。
func (service *Service) ListQueue(ctx context.Context, request QueueRequest) ([]QueueItem, error) {
	return service.api.ListQueue(ctx, request)
}

// SubmitQueue 提交后续任务。
func (service *Service) SubmitQueue(ctx context.Context, request QueueSubmitRequest) (QueueItem, error) {
	return service.api.SubmitQueue(ctx, request)
}

// ConfirmQueue 确认启动队列项。
func (service *Service) ConfirmQueue(ctx context.Context, request QueueActionRequest) (StartRunAck, error) {
	return service.api.ConfirmQueue(ctx, request)
}

// CancelQueue 取消队列项。
func (service *Service) CancelQueue(ctx context.Context, request QueueActionRequest) (QueueItem, error) {
	return service.api.CancelQueue(ctx, request)
}

// SessionTree 查询会话树。
func (service *Service) SessionTree(ctx context.Context, request SessionTreeRequest) (SessionTreeView, error) {
	return service.api.SessionTree(ctx, request)
}

// Evidence 查询完成证据。
func (service *Service) Evidence(ctx context.Context, request InspectRequest) (EvidenceView, error) {
	return service.api.Evidence(ctx, request)
}

// SubmitSteering 投递立即补充输入。
func (service *Service) SubmitSteering(ctx context.Context, request SteeringRequest) (OKResult, error) {
	return service.api.SubmitSteering(ctx, request)
}

// ListHooks 列出钩子规则。
func (service *Service) ListHooks(ctx context.Context, workspace string) ([]HookRule, error) {
	return service.api.ListHooks(ctx, workspace)
}

// UpsertHook 写入钩子规则。
func (service *Service) UpsertHook(ctx context.Context, rule HookRule) (OKResult, error) {
	return service.api.UpsertHook(ctx, rule)
}

// DisableHook 停用钩子。
func (service *Service) DisableHook(ctx context.Context, id string) (OKResult, error) {
	return service.api.DisableHook(ctx, id)
}

// ListProjectInstructions 列出规约来源。
func (service *Service) ListProjectInstructions(ctx context.Context, workspace string) (ProjectInstructionsView, error) {
	return service.api.ListProjectInstructions(ctx, workspace)
}

// ListWorkflows 列出工作流。
func (service *Service) ListWorkflows(ctx context.Context, workspace string) ([]WorkflowDefinition, error) {
	return service.api.ListWorkflows(ctx, workspace)
}

// SaveWorkflow 保存工作流。
func (service *Service) SaveWorkflow(ctx context.Context, def WorkflowDefinition) (OKResult, error) {
	return service.api.SaveWorkflow(ctx, def)
}

// RunWorkflow 预演或运行工作流。
func (service *Service) RunWorkflow(ctx context.Context, workspace, workflowID string, dryRun bool) (WorkflowRunAck, error) {
	return service.api.RunWorkflow(ctx, workspace, workflowID, dryRun)
}

// ListMemories 列出记忆候选。
func (service *Service) ListMemories(ctx context.Context, request WorkspaceRequest) ([]MemoryCandidateView, error) {
	return service.api.ListMemories(ctx, request)
}

// RevokeMemory 撤销记忆候选。
func (service *Service) RevokeMemory(ctx context.Context, request struct {
	Workspace   string `json:"workspace"`
	StateDir    string `json:"state_dir,omitempty"`
	CandidateID string `json:"candidate_id"`
	Reason      string `json:"reason"`
}) (MemoryCandidateView, error) {
	return service.api.RevokeMemory(ctx, request)
}

// ListCommands 列出内置命令。
func (service *Service) ListCommands(ctx context.Context) ([]CommandInfo, error) {
	return service.api.ListCommands(ctx)
}

// InstallSkill 安装技能。
func (service *Service) InstallSkill(ctx context.Context, workspace, scope, source string) (SkillSummary, error) {
	return service.api.InstallSkill(ctx, workspace, scope, source)
}

// SetSkillEnabled 启停技能。
func (service *Service) SetSkillEnabled(ctx context.Context, workspace, scope, name string, enabled bool) (OKResult, error) {
	return service.api.SetSkillEnabled(ctx, workspace, scope, name, enabled)
}

// RemoveSkill 卸载技能。
func (service *Service) RemoveSkill(ctx context.Context, workspace, scope, name string) (OKResult, error) {
	return service.api.RemoveSkill(ctx, workspace, scope, name)
}

// ConsolidateMemories 整理记忆。
func (service *Service) ConsolidateMemories(ctx context.Context, request WorkspaceRequest) (int, error) {
	return service.api.ConsolidateMemories(ctx, request)
}

// SaveReviewNote 保存审查备注。
func (service *Service) SaveReviewNote(ctx context.Context, note ReviewNote) (OKResult, error) {
	return service.api.SaveReviewNote(ctx, note)
}

// ListReviewNotes 列出审查备注。
func (service *Service) ListReviewNotes(ctx context.Context, workspace, relative string) ([]ReviewNote, error) {
	return service.api.ListReviewNotes(ctx, workspace, relative)
}

// ProbeProvider 显式探测 Provider。
func (service *Service) ProbeProvider(ctx context.Context, request WorkspaceRequest) (ProviderProbeView, error) {
	return service.api.ProbeProvider(ctx, request)
}

// WriteProjectInstructions 写入规约。
func (service *Service) WriteProjectInstructions(ctx context.Context, workspace, content string) (OKResult, error) {
	return service.api.WriteProjectInstructions(ctx, workspace, content)
}

// InspectMemory 查看记忆详情。
func (service *Service) InspectMemory(ctx context.Context, workspace, candidateID string) (MemoryDetail, error) {
	return service.api.InspectMemory(ctx, workspace, candidateID)
}

// ListProviderPresets 列出命名连接预设。
func (service *Service) ListProviderPresets(ctx context.Context, request WorkspaceRequest) ([]ProviderPresetView, error) {
	return service.api.ListProviderPresets(ctx, request)
}

// UseProviderPreset 切换激活的命名连接预设。
func (service *Service) UseProviderPreset(ctx context.Context, workspace, name string) (OKResult, error) {
	return service.api.UseProviderPreset(ctx, workspace, name)
}

// RunProviderSetup 执行配置向导。
func (service *Service) RunProviderSetup(ctx context.Context, request ProviderSetupRequest) (ProviderSetupView, error) {
	return service.api.RunProviderSetup(ctx, request)
}

// ConfirmProviderAdmission 记录用户明确确认的 Provider 准入。
func (service *Service) ConfirmProviderAdmission(ctx context.Context, request ProviderAdmissionRequest) (OKResult, error) {
	return service.api.ConfirmProviderAdmission(ctx, request)
}
