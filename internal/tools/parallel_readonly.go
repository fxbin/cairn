// parallel_readonly.go 实现只读工具并行执行规划：识别同一批次中副作用为
// none 的工具调用，允许并行；副作用工具保持串行。不执行，只产出计划。
// Author: fxbin
package tools

import "sort"

// ParallelGroup 是一组可并行的只读工具调用下标。
type ParallelGroup struct {
	// Indexes 是调用批次中的下标，升序。
	Indexes []int `json:"indexes"`
	// Parallel 为 true 表示可并行执行。
	Parallel bool `json:"parallel"`
}

// ReadonlyToolNames 返回可并行的只读工具名集合（副作用 none）。
func ReadonlyToolNames(registry *Registry) map[string]bool {
	names := map[string]bool{}
	if registry == nil {
		return names
	}
	for _, name := range registry.order {
		if registry.SideEffect(name) == SideEffectNone {
			names[name] = true
		}
	}
	return names
}

// PlanParallelGroups 将一批工具调用切分为可并行的只读组与必须串行的副作用组。
// sideEffect 判定由调用方注入；nil 时全部视为串行（失败关闭）。
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
