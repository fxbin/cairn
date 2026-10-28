// delegate_task.go 实现模型侧委派工具的声明与失败关闭执行：工具层只暴露
// 封闭参数与副作用分类，实际委派由应用层回调经 Agent Loop 注入父运行身份
// 后受控执行；不接触 Journal，不虚构子代理。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"

	"github.com/fxbin/cairn/internal/ports"
)

// DelegateTaskName 是委派工具的固定名称。
const DelegateTaskName = "delegate_task"

// DelegateMaxTurnsLimit 限制单次委派可声明的最大轮数，防止模型自授超额预算。
const DelegateMaxTurnsLimit = 100

// DelegateRequest 是模型可见的委派参数；仅封闭字段，不含工具子集或令牌额度。
// Async 为 true 时要求运行配置显式启用 tools.delegate_async，且以异步方式派发：
// 工具立即返回子运行标识与运行中状态，父运行不被同步等待阻塞。
type DelegateRequest struct {
	Task     string `json:"task"`
	MaxTurns int    `json:"max_turns,omitempty"`
	Async    bool   `json:"async,omitempty"`
}

// DelegateTask 是模型侧委派工具；执行必须经 Agent Loop 注入父运行身份，
// 通用执行路径一律失败关闭。
type DelegateTask struct{}

// Spec 返回模型可见工具声明。
func (DelegateTask) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        DelegateTaskName,
		Description: "Delegate a bounded subtask to a child agent run sharing this workspace with a narrowed tool set (child tools are a subset of the parent surface). Blocks until the child finishes or pauses for approval. Set async=true to return immediately with the child run id in state running (requires the tools.delegate_async configuration; at most one active async child, and parent side-effect tools are refused until it settles). Requires task text and optional max_turns (1-100) within the parent budget.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["task"],"properties":{"task":{"type":"string","minLength":1},"max_turns":{"type":"integer","minimum":1,"maximum":100},"async":{"type":"boolean","description":"Dispatch the child asynchronously and return immediately; requires tools.delegate_async to be enabled."}}}`),
	}
}

// SideEffect 声明为进程级副作用：委派消耗父预算并经子工具产生受控副作用。
func (DelegateTask) SideEffect() SideEffectClass { return SideEffectProcess }

// Execute 在通用路径上失败关闭：委派需要 Agent Loop 注入父运行身份，
// 不得在无身份上下文中启动子运行。
func (DelegateTask) Execute(ctx context.Context, raw json.RawMessage) Result {
	return Failure("not_wired", "delegate_task requires the agent loop to inject the parent run identity")
}
