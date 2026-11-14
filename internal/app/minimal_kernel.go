// 本文件为既有 Boot 提供最小内核工具面，不建立第二条装配路径。
// Author: fxbin
package app

import (
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/core/agent"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// minimalToolRegistry 仅暴露基础端口的方法集，避免具体注册表的 MCP/Skill
// 方法被 Loop 的可选能力识别；所有执行仍交给同一静态注册表。
type minimalToolRegistry struct {
	agent.ToolRegistry
}

// minimalTools 构造基础读、文件名检索、内容检索、确定性补丁与 argv 命令工具；
// 命令实例由 Boot 注入，保留环境白名单、沙箱、取消和资源限制。
func minimalTools(workspace tools.Workspace, execTool tools.ExecCommand) []tools.Tool {
	return []tools.Tool{
		tools.ListFiles{Workspace: workspace},
		tools.ReadFile{Workspace: workspace},
		tools.SearchText{Workspace: workspace},
		tools.ApplyPatch{Workspace: workspace},
		execTool,
	}
}

// validateMinimalRunConfiguration 在最小组合的应用入口提前校验，确保冲突
// 配置不能在 Boot 校验之前创建遥测等可选资源；既有预设保留原校验顺序。
func validateMinimalRunConfiguration(configuration config.Config) error {
	if configuration.Preset == config.PresetMinimal {
		return config.Validate(configuration)
	}
	return nil
}
