// tool_result.go 定义工具调用的标准结果合同：字段顺序与 JSON 名称绑定事件
// 日志中的工具回执，迁移前后序列化字节必须逐位一致。构造助手只生成结果值，
// 不执行工具，也不授予权限。
// Author: fxbin
package contract

import "encoding/json"

// ToolResult 是工具调用返回给 Agent 并可写入事件日志的标准结果。
type ToolResult struct {
	OK        bool            `json:"ok"`
	Code      string          `json:"code"`
	Data      json.RawMessage `json:"data,omitempty"`
	Preview   string          `json:"preview,omitempty"`
	Truncated bool            `json:"truncated"`
	Bytes     int64           `json:"bytes"`
	SHA256    string          `json:"sha256,omitempty"`
}

// ToolFailure 创建不含原始数据的结构化工具失败结果。
func ToolFailure(code, preview string) ToolResult { return ToolResult{Code: code, Preview: preview} }

// ToolSuccess 将结果数据序列化为 JSON 并创建结构化成功结果。
func ToolSuccess(data any, preview string, bytes int64, digest string, truncated bool) ToolResult {
	encoded, err := json.Marshal(data)
	if err != nil {
		return ToolFailure("internal", err.Error())
	}
	return ToolResult{OK: true, Code: "ok", Data: encoded, Preview: preview, Bytes: bytes, SHA256: digest, Truncated: truncated}
}

// CodeTerminationUnconfirmed 表示取消信号已发出，但进程退出与资源回收仍未得到确认；
// 调用方必须保留副作用操作的非终态并进入人工对账。
const CodeTerminationUnconfirmed = "termination_unconfirmed"

// 补丁恢复错误码区分调用形状与精确文本失配；它们不授予写入权限。
const (
	CodeInvalidPatchArguments = "invalid_patch_arguments"
	CodePatchTextMismatch     = "patch_text_mismatch"
)

// CodeStalePrecondition 标识 Patch 的已给定摘要已过期；调用方应先重新读取文件，
// 而不是盲目重试或把该安全前置条件错误误报为策略拒绝。
const CodeStalePrecondition = "stale_precondition"

// MCPQuiesceTool 是运行期资源回收入口，不作为模型工具声明或通用命令暴露。
const MCPQuiesceTool = "mcp_close"

// DelegateTaskName 是委派工具的固定名称。
const DelegateTaskName = "delegate_task"

// DelegateMaxTurnsLimit 限制单次委派可声明的最大轮数，防止模型自授超额预算。
const DelegateMaxTurnsLimit = 100

// DefaultMaxWorkers 是并行 worker 默认上限。
const DefaultMaxWorkers = 4

// SideEffectClass 对工具可能造成的副作用进行保守分类。
type SideEffectClass string

const (
	SideEffectNone      SideEffectClass = "none"
	SideEffectWorkspace SideEffectClass = "workspace"
	SideEffectProcess   SideEffectClass = "process"
)
