// workspace_backend.go 定义可替换工作区后端的窄合同：后端负责工作区版本
// 的准备、分叉、观察、比较、丢弃与发布，但不接管文件写入本身，也不改变
// 事件溯源、授权链与验证语义。
//
// 三条不可绕过的边界：
//   - 工作区版本是文件层面的产物，不等于会话检查点，也不等于任务恢复。会话
//     检查点由事件链重放决定，任务恢复由运行生命周期与对账决定。
//   - 版本标识与内容摘要分离。前者是后端不透明的定位符，后者是可跨后端
//     比较的内容事实；漂移检测只依据摘要，不依据路径推断。
//   - 发布必须重新授权、携带验证证据并校验目标摘要；后端不得以"最后写者
//     胜出"静默覆盖业务代码，冲突与漂移都要显式失败。
//
// 该合同当前没有运行时消费方，只是为后续并发工作流固定形状；任何接入都
// 需要新的决策记录，不得据此宣称已支持并行分支。
//
// Author: fxbin
package ports

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// WorkspaceBackendKind 标识工作区后端的实现种类，便于跨后端记录版本来源。
type WorkspaceBackendKind string

const (
	// WorkspaceBackendSharedDirectory 表示直接使用共享工作区目录的后端：
	// 所有版本指向同一物理目录，写入互相可见。
	WorkspaceBackendSharedDirectory WorkspaceBackendKind = "shared_directory"
	// WorkspaceBackendGitWorktree 表示基于 Git worktree 的隔离后端：每个
	// 版本拥有独立检出目录，共享同一对象库。
	WorkspaceBackendGitWorktree WorkspaceBackendKind = "git_worktree"
)

// WorkspaceVersion 是不透明的工作区版本标识。ID 的含义由后端自行定义，
// 调用方只能整体比较，不得解析其内部结构。
type WorkspaceVersion struct {
	// Backend 是产生该版本的后端种类。
	Backend WorkspaceBackendKind `json:"backend"`
	// ID 是后端内部的不透明版本标识。
	ID string `json:"id"`
}

// IsZero 表示版本为空值；空版本不得参与任何后端操作。
func (version WorkspaceVersion) IsZero() bool {
	return version.Backend == "" || version.ID == ""
}

// WorkspaceDigest 是工作区内容摘要，用于跨版本比较与漂移检测。
type WorkspaceDigest string

// IsZero 表示摘要为空值；空摘要不能用于发布前的目标校验。
func (digest WorkspaceDigest) IsZero() bool {
	return digest == ""
}

// 工作区角色常量：角色决定版本允许的操作面，后端不得越权授予。
const (
	// WorkspaceRoleOwner 是可写入并可发布的角色。
	WorkspaceRoleOwner = "owner"
	// WorkspaceRoleWriter 是可写入但不可发布的角色。
	WorkspaceRoleWriter = "writer"
	// WorkspaceRoleReader 是只读角色。
	WorkspaceRoleReader = "reader"
)

// WorkspaceOwner 描述工作区版本的所有者。所有权用于归属运行与派生
// 授权，不授予任何额外权限。
type WorkspaceOwner struct {
	// RunID 是拥有该版本的运行标识。
	RunID string `json:"run_id"`
	// Role 是所有者角色，取 WorkspaceRoleXxx 之一。
	Role string `json:"role"`
}

// WorkspacePermission 描述后端授予单个版本的权限面。
type WorkspacePermission struct {
	// Write 允许修改工作区内容。
	Write bool `json:"write"`
	// Execute 允许在工作区内执行命令。
	Execute bool `json:"execute"`
	// Publish 允许把该版本发布回目标版本。
	Publish bool `json:"publish"`
}

// WorkspaceState 描述某个时刻的工作区版本状态。
type WorkspaceState struct {
	// Version 是该状态对应的版本标识。
	Version WorkspaceVersion `json:"version"`
	// Digest 是观察时刻的内容摘要。
	Digest WorkspaceDigest `json:"digest"`
	// Owner 是该版本的所有者。
	Owner WorkspaceOwner `json:"owner"`
	// Permission 是该版本实际获得的权限面。
	Permission WorkspacePermission `json:"permission"`
	// Isolated 表示该版本是否拥有独占的文件系统视图。共享目录后端恒为
	// 假：多个版本共享同一物理目录，写入互相可见。
	Isolated bool `json:"isolated"`
}

// WorkspaceRequest 描述一次工作区准备或分叉的请求。
type WorkspaceRequest struct {
	// Owner 是新版本的所有者；后端不得把所有权授予其他运行。
	Owner WorkspaceOwner `json:"owner"`
	// Permission 是请求授予的权限面；后端只能授予请求范围内的权限。
	Permission WorkspacePermission `json:"permission"`
	// Label 是人类可读标签，只用于日志与报告，不得包含密钥。
	Label string `json:"label"`
}

// WorkspaceDiff 描述两个版本之间的差异。
type WorkspaceDiff struct {
	// Base 是差异的基线版本。
	Base WorkspaceVersion `json:"base"`
	// Head 是被比较的版本。
	Head WorkspaceVersion `json:"head"`
	// BaseDigest 是基线版本的内容摘要。
	BaseDigest WorkspaceDigest `json:"base_digest"`
	// HeadDigest 是被比较版本的内容摘要。
	HeadDigest WorkspaceDigest `json:"head_digest"`
	// ChangedPaths 是相对基线发生变化的工作区相对路径。
	ChangedPaths []string `json:"changed_paths"`
	// ConflictedPaths 是存在互相覆盖、无法自动合并的路径。
	ConflictedPaths []string `json:"conflicted_paths"`
	// Comparable 表示后端能否给出路径级差异。无法给出路径级差异的后端
	// 只能依赖摘要做整体漂移检测，不得声称已定位冲突。
	Comparable bool `json:"comparable"`
}

// WorkspacePublishRequest 描述一次发布请求。发布是把源版本的内容并入目标
// 版本的动作，必须重新授权并携带验证证据。
type WorkspacePublishRequest struct {
	// Source 是待发布的源版本。
	Source WorkspaceVersion `json:"source"`
	// Target 是接收内容的目标版本。
	Target WorkspaceVersion `json:"target"`
	// ExpectedTargetDigest 是调用方在发起发布前观察到的目标摘要；后端必须
	// 校验目标当前摘要与它一致，否则按漂移显式失败。
	ExpectedTargetDigest WorkspaceDigest `json:"expected_target_digest"`
	// Authorized 表示本次发布已重新获得授权；为假时后端必须拒绝发布。
	Authorized bool `json:"authorized"`
	// VerifierPassed 表示本次发布已有验证器证据；为假时后端必须拒绝发布。
	VerifierPassed bool `json:"verifier_passed"`
}

// WorkspacePublishResult 描述一次成功发布的结果。
type WorkspacePublishResult struct {
	// Published 是发布后目标版本的新标识。
	Published WorkspaceVersion `json:"published"`
	// Digest 是发布后目标版本的内容摘要。
	Digest WorkspaceDigest `json:"digest"`
	// ChangedPaths 是本次发布并入目标版本的路径。
	ChangedPaths []string `json:"changed_paths"`
}

// WorkspaceBackend 定义可替换工作区后端的窄合同。实现方必须遵守：
//   - 写入与命令仍走既有授权链，后端不引入旁路；
//   - 发布不得静默覆盖，冲突、漂移与疑似机密都要显式失败；
//   - 观察范围不得窄于工具视图的忽略规则，忽略项只限制工具视图。
type WorkspaceBackend interface {
	// Prepare 准备一个新的工作区版本，返回其初始状态。
	Prepare(ctx context.Context, request WorkspaceRequest) (WorkspaceState, error)
	// Fork 从基线版本派生一个新版本；派生出的版本按后端能力决定是否隔离。
	Fork(ctx context.Context, base WorkspaceVersion, request WorkspaceRequest) (WorkspaceState, error)
	// Observe 观察指定版本当前的内容摘要、权限与所有权。
	Observe(ctx context.Context, version WorkspaceVersion) (WorkspaceState, error)
	// Diff 比较基线版本与目标版本的内容差异。
	Diff(ctx context.Context, base WorkspaceVersion, head WorkspaceVersion) (WorkspaceDiff, error)
	// Discard 丢弃指定版本；后端应把基线内容恢复到可继续使用的状态。
	Discard(ctx context.Context, version WorkspaceVersion) error
	// Publish 把源版本发布回目标版本；必须校验授权、验证证据与目标摘要。
	Publish(ctx context.Context, request WorkspacePublishRequest) (WorkspacePublishResult, error)
}

// 工作区后端的失败语义：错误均为可判定失败，调用方不得以重试掩盖。
var (
	// ErrWorkspaceUnsupported 表示后端不支持该操作，或当前版本形态无法
	// 安全完成该操作（例如共享目录无法归因分支变更）。
	ErrWorkspaceUnsupported = errors.New("workspace backend does not support this operation")
	// ErrWorkspaceVersionUnknown 表示版本不在后端已知集合内。
	ErrWorkspaceVersionUnknown = errors.New("workspace version is unknown to the backend")
	// ErrWorkspaceOwnership 表示操作者不是该版本的所有者，或权限面不足。
	ErrWorkspaceOwnership = errors.New("workspace operation is not allowed for this owner")
	// ErrWorkspaceDrift 表示目标版本当前内容与预期摘要不符。
	ErrWorkspaceDrift = errors.New("workspace target drifted from the expected digest")
	// ErrWorkspaceConflict 表示存在互相覆盖、无法自动合并的变更。
	ErrWorkspaceConflict = errors.New("workspace has conflicting changes that cannot be merged")
	// ErrWorkspaceNotAuthorized 表示发布缺少重新授权证据。
	ErrWorkspaceNotAuthorized = errors.New("workspace publish is not authorized")
	// ErrWorkspaceVerificationMissing 表示发布缺少验证器证据。
	ErrWorkspaceVerificationMissing = errors.New("workspace publish lacks verifier evidence")
	// ErrWorkspaceSecretLeak 表示待发布内容包含疑似机密。
	ErrWorkspaceSecretLeak = errors.New("workspace publish contains suspected secrets")
	// ErrWorkspaceDirty 表示待发布版本存在未提交或未归档的内容变更。
	ErrWorkspaceDirty = errors.New("workspace version has uncommitted changes")
)

// WorkspaceDriftError 描述目标版本与预期摘要不符的具体差异；摘要值不含
// 路径与正文，可安全写入事件与日志。
type WorkspaceDriftError struct {
	// Expected 是调用方发起发布时观察到的目标摘要。
	Expected WorkspaceDigest
	// Actual 是后端在发布时观察到的目标摘要。
	Actual WorkspaceDigest
}

// Error 返回不含本地路径的漂移描述。
func (err *WorkspaceDriftError) Error() string {
	return fmt.Sprintf("workspace target drifted: expected %s, actual %s", err.Expected, err.Actual)
}

// Unwrap 使漂移错误可被 errors.Is 识别为 ErrWorkspaceDrift。
func (err *WorkspaceDriftError) Unwrap() error { return ErrWorkspaceDrift }

// WorkspaceConflictError 描述无法自动合并的冲突路径。
type WorkspaceConflictError struct {
	// Paths 是互相覆盖的工作区相对路径。
	Paths []string
}

// Error 返回冲突路径列表，不包含文件正文。
func (err *WorkspaceConflictError) Error() string {
	return fmt.Sprintf("workspace conflict on paths: %s", strings.Join(err.Paths, ", "))
}

// Unwrap 使冲突错误可被 errors.Is 识别为 ErrWorkspaceConflict。
func (err *WorkspaceConflictError) Unwrap() error { return ErrWorkspaceConflict }

// WorkspaceSecretLeakError 描述疑似机密命中的路径；只报告路径，不复制正文。
type WorkspaceSecretLeakError struct {
	// Paths 是命中疑似机密规则的工作区相对路径。
	Paths []string
}

// Error 返回命中路径列表，不包含文件正文。
func (err *WorkspaceSecretLeakError) Error() string {
	return fmt.Sprintf("workspace publish contains suspected secrets at: %s", strings.Join(err.Paths, ", "))
}

// Unwrap 使机密错误可被 errors.Is 识别为 ErrWorkspaceSecretLeak。
func (err *WorkspaceSecretLeakError) Unwrap() error { return ErrWorkspaceSecretLeak }
