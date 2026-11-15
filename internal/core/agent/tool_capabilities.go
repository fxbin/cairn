// [INPUT]: 可选授权执行端口与不透明执行绑定。
// [OUTPUT]: 传递绑定并拒绝向普通执行降级。
// [POS]: Core 不识别协议名称，缺必需端口时失败关闭。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// tool_capabilities.go 定义可选授权执行边界，不识别协议名称。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/fxbin/cairn/internal/contract"
)

// authorizedToolExecutor 将策略的不透明执行绑定原样交给实际传输。
type authorizedToolExecutor interface {
	ExecuteWithBinding(context.Context, string, json.RawMessage, string) contract.ToolResult
}

// validateToolExecution 拒绝要求授权执行却缺少完整执行端口的装配。
func (loop *Loop) validateToolExecution() error {
	if required, ok := loop.Tools.(interface{ RequiresAuthorizedExecution() bool }); ok && required.RequiresAuthorizedExecution() {
		if _, ok := loop.Tools.(authorizedToolExecutor); !ok {
			return errors.New("tool registry requires an authorized execution port")
		}
	}
	return nil
}

// executeToolWithBinding 非空绑定不得降级普通执行；普通工具保留原最小端口。
func (loop *Loop) executeToolWithBinding(ctx context.Context, name string, arguments json.RawMessage, binding string) contract.ToolResult {
	if executor, ok := loop.Tools.(authorizedToolExecutor); ok {
		return executor.ExecuteWithBinding(ctx, name, arguments, binding)
	}
	if binding != "" {
		return contract.ToolFailure("authorized_execution_unavailable", "authorized tool execution is not configured")
	}
	return loop.Tools.Execute(ctx, name, arguments)
}
