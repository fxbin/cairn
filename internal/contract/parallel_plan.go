// parallel_plan.go 定义只读工具并行分组的纯规划合同：识别同一批次中副作用
// 为 none 的调用允许并行，副作用调用保持串行。只产出计划，不执行工具。
// Author: fxbin
package contract

import "sort"

// ParallelGroup 是一组可并行的只读工具调用下标。
type ParallelGroup struct {
	// Indexes 是调用批次中的下标，升序。
	Indexes []int `json:"indexes"`
	// Parallel 为 true 表示可并行执行。
	Parallel bool `json:"parallel"`
}

// PlanParallelGroups 将一批工具调用切分为可并行的只读组与必须串行的副作用组。
// isReadonly 判定由调用方注入；nil 时全部视为串行（失败关闭）。
func PlanParallelGroups(toolNames []string, isReadonly func(name string) bool) []ParallelGroup {
	if isReadonly == nil {
		groups := make([]ParallelGroup, 0, len(toolNames))
		for index := range toolNames {
			groups = append(groups, ParallelGroup{Indexes: []int{index}, Parallel: false})
		}
		return groups
	}
	var groups []ParallelGroup
	flush := func(run []int, parallel bool) {
		if len(run) == 0 {
			return
		}
		indexes := append([]int(nil), run...)
		sort.Ints(indexes)
		groups = append(groups, ParallelGroup{Indexes: indexes, Parallel: parallel})
	}
	var readonlyRun []int
	var serialRun []int
	for index, name := range toolNames {
		if isReadonly(name) {
			if len(serialRun) > 0 {
				flush(serialRun, false)
				serialRun = nil
			}
			readonlyRun = append(readonlyRun, index)
			continue
		}
		if len(readonlyRun) > 0 {
			flush(readonlyRun, true)
			readonlyRun = nil
		}
		serialRun = append(serialRun, index)
	}
	flush(readonlyRun, true)
	flush(serialRun, false)
	return groups
}
