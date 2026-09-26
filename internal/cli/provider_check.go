// 本文件实现 Provider 合同探测命令：装配与探测在 app.CheckProvider，
// 本层只负责旗标、人类可读输出与退出码。
//
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
)

// generationUnsupportedText 是摘要中表达全块未声明的固定文本。
const generationUnsupportedText = "unsupported"

// checkProvider 对当前配置解析出的 OpenAI-compatible 提供方执行一次真实模型
// 合同探测。付费外呼由用户显式发起本命令触发；结论与证据由应用层收口。
func (command Command) checkProvider(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("check-provider", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace root (defaults to the current directory)")
	configPath := flags.String("config", "", "explicit configuration path")
	stateDir := flags.String("state-dir", "", "state directory override")
	jsonOutput := flags.Bool("json", false, "write a single JSON line to stdout")
	probeTools := flags.Bool("probe-tools", false, "also verify the tool call round trip with a no-op probe tool")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "check-provider takes no positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	result, persistErr := command.Service.CheckProvider(ctx, app.CheckProviderOptions{
		Config:     configuration,
		Workspace:  *workspace,
		StateDir:   *stateDir,
		ProbeTools: *probeTools,
	})
	if persistErr != nil {
		fmt.Fprintln(command.Stderr, "evidence persistence failed:", persistErr)
	}
	if *jsonOutput {
		if err := writeJSONChecked(command.Stdout, result); err != nil {
			fmt.Fprintln(command.Stderr, "check-provider output failed:", err)
			return ExitFailed
		}
	} else {
		command.writeProviderCheck(result)
	}
	switch result.Result {
	case app.ProviderProbePass:
		return ExitOK
	default:
		return ExitFailed
	}
}

// writeProviderCheck 输出人类可读的探测报告；所有取值均为非敏感闭合字段。
func (command Command) writeProviderCheck(result app.ProviderProbeResult) {
	fmt.Fprintf(command.Stdout, "provider kind: %s\n", result.ProviderKind)
	fmt.Fprintf(command.Stdout, "endpoint identity: %s\n", result.EndpointIdentity)
	fmt.Fprintf(command.Stdout, "model: %s (revision: %s)\n", result.Model, result.ModelRevision)
	fmt.Fprintf(command.Stdout, "capability fingerprint: %s\n", result.CapabilityFingerprint)
	if summary := result.Generation; summary != nil {
		parts := make([]string, 0, 5)
		if summary.MaxOutputTokens > 0 {
			parts = append(parts, fmt.Sprintf("max_output_tokens=%d", summary.MaxOutputTokens))
		} else {
			parts = append(parts, fmt.Sprintf("max_output_tokens=%s", generationUnsupportedText))
		}
		for name, value := range map[string]*int{"temperature_milli": summary.TemperatureMilli, "top_p_milli": summary.TopPMilli} {
			if value != nil {
				parts = append(parts, fmt.Sprintf("%s=%d", name, *value))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", name, generationUnsupportedText))
		}
		for _, item := range []struct {
			name  string
			value string
		}{{"reasoning_effort", summary.ReasoningEffort}, {"tool_choice", summary.ToolChoice}} {
			if item.value == "" {
				parts = append(parts, fmt.Sprintf("%s=%s", item.name, generationUnsupportedText))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", item.name, item.value))
		}
		fmt.Fprintf(command.Stdout, "generation: %s\n", strings.Join(parts, ", "))
	} else {
		fmt.Fprintf(command.Stdout, "generation: %s\n", generationUnsupportedText)
	}
	fmt.Fprintf(command.Stdout, "usage stream: %t\n", result.UsageStreamed)
	fmt.Fprintf(command.Stdout, "stop reason: %s\n", stopReasonOrUnknown(result.StopReason))
	fmt.Fprintf(command.Stdout, "tool roundtrip: %s\n", result.ToolRoundtrip)
	if result.Error != "" {
		fmt.Fprintf(command.Stdout, "error: %s\n", result.Error)
	}
	fmt.Fprintf(command.Stdout, "result: %s\n", result.Result)
}

// stopReasonOrUnknown 把空 stop reason 收敛为展示用的 unknown 标记。
func stopReasonOrUnknown(reason string) string {
	if reason == "" {
		return "unknown"
	}
	return reason
}
