package desktop

import (
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// 本文件定义 GUI 桥接层的稳定 DTO。字段名以 JSON 传输为准，前端 bindings
// 依赖这些名称；不得在此暴露密钥、审批指纹原文以外的敏感策略细节或宿主路径
// 之外的任意文件内容。

// ErrorBody 是前端可展示的统一错误信封。
type ErrorBody struct {
	// Code 是稳定机器码，如 waiting_approval、config_drift、state_corrupt。
	Code string `json:"code"`
	// Message 是可展示的中文说明，不含密钥与原始模型输出。
	Message string `json:"message"`
	// RunID 可选，标识错误关联的运行。
	RunID string `json:"run_id,omitempty"`
	// OperationID 可选，标识悬而未决或待审批操作。
	OperationID string `json:"operation_id,omitempty"`
	// Fingerprint 仅在等待审批时返回，供 Resume 绑定一次性决定。
	Fingerprint string `json:"fingerprint,omitempty"`
	// Summary 是待审批操作的影响摘要。
	Summary string `json:"summary,omitempty"`
}

// OKResult 是无数据成功响应。
type OKResult struct {
	OK bool `json:"ok"`
}

// HealthResult 报告桌面桥接可用性与只读环境摘要。
type HealthResult struct {
	OK        bool   `json:"ok"`
	AppName   string `json:"app_name"`
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	UserRoot  string `json:"user_root,omitempty"`
}

// WorkspaceStatus 描述当前工作区与仓库状态的只读投影。
type WorkspaceStatus struct {
	Path          string `json:"path"`
	Exists        bool   `json:"exists"`
	IsWorkspace   bool   `json:"is_workspace"`
	HasAgentDir   bool   `json:"has_agent_dir"`
	HasConfig     bool   `json:"has_config"`
	GitRoot       string `json:"git_root,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Dirty         bool   `json:"dirty"`
	StateDir      string `json:"state_dir"`
	ConfigPath    string `json:"config_path"`
	ConfigLoaded  bool   `json:"config_loaded"`
	ConfigError   string `json:"config_error,omitempty"`
	ConfigSource  string `json:"config_source,omitempty"`
	Fingerprint   string `json:"config_fingerprint,omitempty"`
	ActiveRunID   string `json:"active_run_id,omitempty"`
	ActiveRunNote string `json:"active_run_note,omitempty"`
}

// RecentProject 是 UI 偏好中的最近项目条目，不替代运行事实。
type RecentProject struct {
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	LastUsed time.Time `json:"last_used"`
	Pinned   bool      `json:"pinned"`
	Archived bool      `json:"archived"`
}

// SessionMeta 是会话展示偏好（置顶/归档/重命名），与 Journal 运行事实分离。
type SessionMeta struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	Pinned    bool   `json:"pinned"`
	Archived  bool   `json:"archived"`
}

// RunHistoryView 是侧栏会话列表条目。
type RunHistoryView struct {
	app.RunHistoryItem
	// Title 优先取用户元数据，否则使用任务首行。
	Title string `json:"title"`
	// Pinned/Archived 来自会话展示偏好，供列表排序与操作。
	Pinned   bool `json:"pinned"`
	Archived bool `json:"archived"`
	// WaitingApproval 表示当前处于待审批。
	WaitingApproval bool `json:"waiting_approval"`
	// WaitingReconcile 表示存在悬而未决副作用。
	WaitingReconcile bool `json:"waiting_reconcile"`
}

// StartRunRequest 启动一次新任务。
type StartRunRequest struct {
	Workspace string `json:"workspace"`
	StateDir  string `json:"state_dir,omitempty"`
	Task      string `json:"task"`
	Model     string `json:"model,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	PrevRunID string `json:"prev_run_id,omitempty"`
	// ForkSourceRunID 是分支来源的终态 run；后端解析为安全锚点。
	ForkSourceRunID string `json:"fork_source_run_id,omitempty"`
	// ForkSource 兼容旧字段：与 ForkSourceRunID 等价。
	ForkSource string `json:"fork_source,omitempty"`
	// Mode 是产品工作模式（如 default / read-only）；后端不支持时失败关闭。
	Mode string `json:"mode,omitempty"`
	// RequireSessionTail 要求 prev_run_id 仍是会话唯一链尾；续接时为 true。
	RequireSessionTail bool `json:"require_session_tail,omitempty"`
}

// StartRunAck 确认运行已受理（异步执行）。
type StartRunAck struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id,omitempty"`
	State     string `json:"state"`
}

// ResumeRequest 恢复运行并可选携带一次性审批决定。
// Model/Mode 必须与启动时的运行时覆盖一致，使冻结指纹可复现；
// 真实配置漂移仍由 requireMatchingConfig 拒绝。
type ResumeRequest struct {
	Workspace   string `json:"workspace"`
	StateDir    string `json:"state_dir,omitempty"`
	RunID       string `json:"run_id"`
	Approve     bool   `json:"approve"`
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason,omitempty"`
	Model       string `json:"model,omitempty"`
	Mode        string `json:"mode,omitempty"`
}

// ResolveRequest 对悬而未决副作用做人工对账。
type ResolveRequest struct {
	Workspace   string `json:"workspace"`
	StateDir    string `json:"state_dir,omitempty"`
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
	Verdict     string `json:"verdict"`
	Note        string `json:"note"`
	Evidence    string `json:"evidence"`
}

// AbortRequest 收尾不存在不确定外部副作用的运行。
type AbortRequest struct {
	Workspace string `json:"workspace"`
	StateDir  string `json:"state_dir,omitempty"`
	RunID     string `json:"run_id"`
	Reason    string `json:"reason"`
}

// CancelRequest 请求取消运行（合作式取消）。
type CancelRequest struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason"`
}

// WorkspaceRequest 是只读查询的公共工作区定位参数。
type WorkspaceRequest struct {
	Workspace string `json:"workspace"`
	StateDir  string `json:"state_dir,omitempty"`
	// Query 可选：按标题/任务/run 标识过滤历史列表。
	Query string `json:"query,omitempty"`
	// IncludeArchived 是否包含已归档会话展示项。
	IncludeArchived bool `json:"include_archived,omitempty"`
}

// InspectRequest 查询运行投影。
type InspectRequest struct {
	Workspace     string `json:"workspace"`
	StateDir      string `json:"state_dir,omitempty"`
	RunID         string `json:"run_id"`
	IncludeEvents bool   `json:"include_events"`
}

// RunDetail 是运行详情视图。
type RunDetail struct {
	RunID     string                `json:"run_id"`
	SessionID string                `json:"session_id,omitempty"`
	State     domain.RunState       `json:"state"`
	Phase     string                `json:"phase,omitempty"`
	Task      string                `json:"task,omitempty"`
	Summary   app.RunSummary        `json:"summary"`
	Manifest  *contract.RunManifest `json:"manifest,omitempty"`
	Events    []domain.Event        `json:"events,omitempty"`
}

// StreamEvent 是推送给前端的增量通知信封。
// Kind 取值：run.event | run.status | run.waiting_approval | run.waiting_reconcile | run.finished | error。
type StreamEvent struct {
	Workspace  string         `json:"workspace,omitempty"`
	Kind       string         `json:"kind"`
	RunID      string         `json:"run_id,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Seq        uint64         `json:"seq,omitempty"`
	OccurredAt time.Time      `json:"occurred_at,omitempty"`
	Event      *domain.Event  `json:"event,omitempty"`
	Status     *RunStatusView `json:"status,omitempty"`
	Error      *ErrorBody     `json:"error,omitempty"`
}

// RunStatusView 是运行生命周期的精简状态。
type RunStatusView struct {
	RunID            string `json:"run_id"`
	SessionID        string `json:"session_id,omitempty"`
	State            string `json:"state"`
	Phase            string `json:"phase,omitempty"`
	WaitingApproval  bool   `json:"waiting_approval"`
	WaitingReconcile bool   `json:"waiting_reconcile"`
	ResponsePreview  string `json:"response_preview,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	ErrorMessage     string `json:"error_message,omitempty"`
}

// SteeringRequest 向运行中的任务投递立即补充输入；与后续队列语义分离。
type SteeringRequest struct {
	RunID   string `json:"run_id"`
	InputID string `json:"input_id,omitempty"`
	Content string `json:"content"`
}
