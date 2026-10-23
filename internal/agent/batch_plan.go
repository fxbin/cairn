// batch_plan.go 为模型返回的工具调用批次生成并行计划；
// 仅作诊断投影，执行仍走串行 Journal 合同（单写入者）。
// Author: fxbin
package agent

import (
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// BatchPlanEntry 是批次中单个调用的计划归属。
type BatchPlanEntry struct {
	CallID   string `json:"call_id"`
	ToolName string `json:"tool_name"`
	Group    int    `json:"group"`
	Parallel bool   `json:"parallel"`
}

// BatchPlan 是工具批次的并行执行计划（诊断用）。
type BatchPlan struct {
	Entries []BatchPlanEntry `json:"entries"`
	Groups  int              `json:"groups"`
	// Parallelizable 表示存在可并行的只读组。
	Parallelizable bool `json:"parallelizable"`
}

// PlanBatch 按副作用分类生成批次计划。
// isReadonly 为 nil 时全部标记为串行（失败关闭）。
func PlanBatch(calls []ports.ToolCall, isReadonly func(name string) bool) BatchPlan {
	names := make([]string, len(calls))
	for index, call := range calls {
		names[index] = call.Name
	}
	groups := tools.PlanParallelGroups(names, isReadonly)
	plan := BatchPlan{Entries: make([]BatchPlanEntry, 0, len(calls)), Groups: len(groups)}
	for groupIndex, group := range groups {
		for _, callIndex := range group.Indexes {
			plan.Entries = append(plan.Entries, BatchPlanEntry{
				CallID:   calls[callIndex].ID,
				ToolName: calls[callIndex].Name,
				Group:    groupIndex,
				Parallel: group.Parallel,
			})
			if group.Parallel {
				plan.Parallelizable = true
			}
		}
	}
	return plan
}

// ReadonlyFromRegistry 返回基于注册表副作用判定的只读函数。
func ReadonlyFromRegistry(registry *tools.Registry) func(name string) bool {
	if registry == nil {
		return nil
	}
	readonly := tools.ReadonlyToolNames(registry)
	return func(name string) bool { return readonly[name] }
}
