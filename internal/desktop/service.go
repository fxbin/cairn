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
func (service *Service) DiffFile(ctx context.Context, workspace, relative string) (string, error) {
	return service.api.DiffFile(ctx, workspace, relative)
}

// OpenPathInEditor 在外部编辑器打开路径。
func (service *Service) OpenPathInEditor(ctx context.Context, path string) (OKResult, error) {
	return service.api.OpenPathInEditor(ctx, path)
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
