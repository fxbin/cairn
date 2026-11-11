// studio_tui.go 为 TUI 提供转向桥、UI 阶段追踪与凭据写入的 Application
// 门面；Studio 层不得直接依赖 internal/core/agent 与 internal/credential。
// Author: fxbin
package app

import (
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/core/agent"
	"github.com/fxbin/cairn/internal/runtime/credential"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// toolsWorkspace 是工作区视图的窄别名，桌面端不直接持有 tools 符号。
type toolsWorkspace = tools.Workspace

// TUI 转向与阶段追踪类型别名：终端层只消费，不构造 Core 内部状态。
type (
	// SteeringBridge 是运行中转向消息的窄桥。
	SteeringBridge = agent.SteeringBridge
	// UIPhaseTracker 是 UI 阶段追踪器。
	UIPhaseTracker = agent.UIPhaseTracker
	// BatchPlan 是批量执行计划。
	BatchPlan = agent.BatchPlan
	// UIPhase 是 UI 阶段枚举。
	UIPhase = agent.UIPhase
)

// UI 阶段常量别名。
const (
	UIPhaseActive = agent.UIPhaseActive
	UIPhasePrompt = agent.UIPhasePrompt
)

// NewSteeringBridge 构造运行中转向桥。
func NewSteeringBridge() *agent.SteeringBridge { return agent.NewSteeringBridge() }

// UpsertSecretEnvFile 将密钥值以 0600 原子写入凭据文件；值不进入日志。
func UpsertSecretEnvFile(path, key, secret string) error {
	return credential.UpsertEnvFile(path, key, secret)
}

// WorkspaceKeysPath 返回工作区凭据文件路径。
func WorkspaceKeysPath(workspace string) string {
	return workspace + "/.agent/keys.env"
}

// UserKeysEnvPath 返回用户级凭据文件路径。
func UserKeysEnvPath() (string, error) { return credential.UserKeysPath() }

// KeysFileSegment 是工作区内凭据文件的固定相对路径段。
const KeysFileSegment = credential.KeysFileName

// NewStudioWorkspace 构造受限工作区视图；桌面端路径解析必须经此边界。
func NewStudioWorkspace(root string, ignored []string, maxReadBytes int64, maxSearchFiles int, maxSearchBytes int64) (toolsWorkspace, error) {
	return tools.NewWorkspace(root, ignored, maxReadBytes, maxSearchFiles, maxSearchBytes)
}

// StudioProjectInstructions 读取工作区项目规约的脱敏快照。
func StudioProjectInstructions(root string) (contract.ProjectInstructions, error) {
	view, err := tools.NewWorkspace(root, nil, 0, 0, 0)
	if err != nil {
		return contract.ProjectInstructions{}, err
	}
	return tools.LoadProjectInstructions(view)
}
