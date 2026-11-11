// authorization_ports.go 定义策略门禁消费的最小授权端口：补丁规划器与 MCP
// 授权投影来源。Gate 只依赖这些窄接口，不持有工具注册表或具体工具实现；
// 组合根负责把具体实现装配为端口。端口不授予权限，也不产生副作用。
// Author: fxbin
package contract

import "encoding/json"

// PatchPlanner 是审批前确定性补丁规划的最小端口：只读校验并计算改写前后
// 摘要，不写入文件；规划失败必须返回可恢复的错误类型。
type PatchPlanner interface {
	// Plan 在不写入文件的前提下校验参数并返回确定性改写计划。
	Plan(raw json.RawMessage) (PatchPlan, error)
}

// MCPProposalSource 是本地 MCP 授权投影的最小端口：由已持久化目录派生，
// 不建立连接；未命中工具时 found 为 false，目录或 schema 漂移必须返回错误。
type MCPProposalSource interface {
	// Proposal 返回 MCP 工具的本地授权投影。
	Proposal(name string, arguments json.RawMessage) (MCPProposal, bool, error)
}
