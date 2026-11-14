// 本文件约束显式最小内核组合，防止能力配置被静默忽略。
// Author: fxbin
package config

import "errors"

// validateMinimalPreset 拒绝与最小内核冲突的能力声明；基础授权、沙箱、
// 预算和持久化继续使用原合同，不因工具面缩小而放宽。
func validateMinimalPreset(configuration Config) error {
	if configuration.Preset != PresetMinimal {
		return nil
	}
	if configuration.Skills != nil && configuration.Skills.Enabled {
		return errors.New("minimal preset forbids enabled skills")
	}
	if configuration.MCP != nil {
		for _, server := range configuration.MCP.Servers {
			if !server.Disabled {
				return errors.New("minimal preset forbids enabled MCP servers")
			}
		}
	}
	optional := configuration.Tools
	if optional.Web || optional.ExecSession || optional.DelegateAsync || optional.Browser || optional.RunTests || optional.LspDiagnostics {
		return errors.New("minimal preset forbids optional tools")
	}
	if len(configuration.Hooks) != 0 {
		return errors.New("minimal preset forbids hooks")
	}
	if len(configuration.Verification.Commands) != 0 || configuration.Verification.AutoAfterPatch {
		return errors.New("minimal preset forbids verification commands and auto_after_patch")
	}
	if configuration.Telemetry.Enabled {
		return errors.New("minimal preset forbids telemetry export")
	}
	return nil
}
