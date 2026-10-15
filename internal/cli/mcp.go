// Package cli 提供显式 MCP 配置查询和 OAuth 登录入口，JSON 模式只输出 NDJSON。
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/config"
)

// defaultMCPLoginTimeout 限制用户登录全过程，包含授权元数据和 token 请求。
const defaultMCPLoginTimeout = 5 * time.Minute

// mcpCommand 分派纯配置查询和明确的本机认证动作；查询不会启动外部服务。
func (command Command) mcpCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "mcp requires a subcommand: list, login, logout")
		return ExitUsage
	}
	action := arguments[0]
	if action != "list" && action != "login" && action != "logout" {
		fmt.Fprintln(command.Stderr, "unknown mcp subcommand")
		return ExitUsage
	}
	flags := flag.NewFlagSet("mcp "+action, flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace root")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	timeout := flags.Duration("timeout", defaultMCPLoginTimeout, "authentication operation timeout")
	values := map[string]struct{}{"--workspace": {}, "--config": {}, "--timeout": {}}
	if err := flags.Parse(normalizeFlagsThenPositionals(arguments[1:], values)); err != nil {
		return ExitUsage
	}
	if action == "list" && flags.NArg() != 0 || action != "list" && flags.NArg() != 1 || *timeout <= 0 || *timeout > 30*time.Minute {
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "MCP config error:", err)
		return ExitUsage
	}
	if action == "list" {
		statuses, err := command.Service.MCPStatus(configuration)
		if err != nil {
			fmt.Fprintln(command.Stderr, "MCP status failed:", err)
			return ExitFailed
		}
		for _, status := range statuses {
			if *jsonOutput {
				if writeJSONChecked(command.Stdout, status) != nil {
					return ExitFailed
				}
			} else {
				fmt.Fprintf(command.Stdout, "%s\t%s\texposure=%s disabled=%t auth=%s credentials=%t connect=%s call=%s\n", status.Name, status.Transport, status.Exposure, status.Disabled, status.Authentication, status.CredentialsPresent, status.ConnectPolicy, status.CallPolicy)
			}
		}
		return ExitOK
	}
	operationContext, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	name := flags.Arg(0)
	if action == "login" {
		err = command.Service.MCPLogin(operationContext, configuration, name, command.Stderr)
	} else {
		err = command.Service.MCPLogout(operationContext, configuration, name)
	}
	if err != nil {
		fmt.Fprintln(command.Stderr, "MCP "+action+" failed:", err)
		return exitFor(err)
	}
	report := struct {
		Server  string `json:"server"`
		Action  string `json:"action"`
		Success bool   `json:"success"`
	}{Server: name, Action: action, Success: true}
	if *jsonOutput {
		if writeJSONChecked(command.Stdout, report) != nil {
			return ExitFailed
		}
	} else {
		fmt.Fprintf(command.Stdout, "MCP %s: %s\n", action, name)
	}
	return ExitOK
}
