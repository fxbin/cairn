// [INPUT]: 确定性补丁参数与共享改写计划。
// [OUTPUT]: 提供基础策略的只读 PatchPlanner。
// [POS]: 共享规划端口不承载能力专用来源。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// authorization_ports.go 定义基础策略消费的只读补丁规划端口。
// 组合根注入实际规划器；本合同不携带工具执行或协议目录能力。
// Author: fxbin
package contract

import "encoding/json"

// PatchPlanner 是审批前确定性补丁规划的最小端口：只读校验并计算改写前后
// 摘要，不写入文件；规划失败必须返回可恢复的错误类型。
type PatchPlanner interface {
	// Plan 在不写入文件的前提下校验参数并返回确定性改写计划。
	Plan(raw json.RawMessage) (PatchPlan, error)
}
