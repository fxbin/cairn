// contract_aliases.go 保留工具层既有符号名：纯数据合同已归位 internal/contract，
// 这里以类型别名和函数转发维持 tools 包的对外表面，避免扩大无关改动面。
// Author: fxbin
package tools

import (
	"encoding/json"

	"github.com/fxbin/cairn/internal/contract"
)

// ExecArguments 是 exec_command 的受限执行参数，合同归位 contract 层。
type ExecArguments = contract.ExecArguments

// ParseExecArguments 校验命令参数并填充与执行器相同的默认值。
func ParseExecArguments(raw json.RawMessage) (ExecArguments, error) {
	return contract.ParseExecArguments(raw)
}

// SessionAction 是会话动作类型，合同归位 contract 层。
type SessionAction = contract.SessionAction

// 会话动作常量沿用 contract 层定义。
const (
	// SessionOpen 创建受控 tmux 会话。
	SessionOpen = contract.SessionOpen
	// SessionWrite 向会话注入输入。
	SessionWrite = contract.SessionWrite
	// SessionRead 读取自游标起的输出。
	SessionRead = contract.SessionRead
	// SessionClose 关闭会话并回收进程组。
	SessionClose = contract.SessionClose
)

// SessionRequest 是 exec_session 的统一入参，合同归位 contract 层。
type SessionRequest = contract.SessionRequest

// ValidateSessionRequest 校验会话动作；不执行任何外部调用。
func ValidateSessionRequest(request SessionRequest) error {
	return contract.ValidateSessionRequest(request)
}

// IsSensitiveInput 判断输入是否触发二次确认。
func IsSensitiveInput(content string) bool {
	return contract.IsSensitiveSessionInput(content)
}

// MCPProposal 是不产生连接或调用的本地授权投影，合同归位 contract 层。
type MCPProposal = contract.MCPProposal

// PatchArgumentsError 描述可由模型重建调用形状修复的参数错误。
type PatchArgumentsError = contract.PatchArgumentsError

// PatchPreconditionError 描述改写前置摘要与当前文件不一致。
type PatchPreconditionError = contract.PatchPreconditionError

// PatchTextMatchError 描述精确匹配失配。
type PatchTextMatchError = contract.PatchTextMatchError

// PatchFailureResult 将非空规划错误转换为无副作用的模型恢复反馈。
func PatchFailureResult(cause error) contract.ToolResult {
	return contract.PatchFailureResult(cause)
}

// PatchPlan 是审批与执行共用的确定性改写计划及前后摘要；为兼容 tui 等既有
// 消费方，这里别名到 contract 层的授权合同定义，二者字段逐位相同。
type PatchPlan = contract.PatchPlan

// GuardWrite 在显式拒绝标记下拒绝写；无标记视为信任（向后兼容）。
func GuardWrite(root, operation string) error {
	return contract.GuardWorkspaceWrite(root, operation)
}

// IsExplicitlyUntrusted 检查是否存在显式拒绝标记。
func IsExplicitlyUntrusted(root string) bool {
	return contract.IsExplicitlyUntrustedWorkspace(root)
}

// WorkspaceRoot 返回补丁工具绑定的工作区根路径。
func (tool ApplyPatch) WorkspaceRoot() string {
	return tool.Workspace.Root
}

// ResolveWorkspace 把相对路径解析为绝对路径与规范相对路径，越界必须返回错误。
func (tool ApplyPatch) ResolveWorkspace(relative string) (string, string, error) {
	return tool.Workspace.Resolve(relative)
}

// WorkspaceObservation 返回工具视图的工作区观察摘要。
func (tool ApplyPatch) WorkspaceObservation() (string, error) {
	return tool.Workspace.Observation()
}
