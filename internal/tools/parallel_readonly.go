// parallel_readonly.go 提供只读工具名集合的判定，供并行执行规划收敛副作用
// 为 none 的工具调用。不执行，只产出集合。
// Author: fxbin
package tools

import "github.com/fxbin/cairn/internal/contract"

// ReadonlyToolNames 返回可并行的只读工具名集合（副作用 none）。
func ReadonlyToolNames(registry *Registry) map[string]bool {
	names := map[string]bool{}
	if registry == nil {
		return names
	}
	for _, name := range registry.order {
		if registry.SideEffect(name) == contract.SideEffectNone {
			names[name] = true
		}
	}
	return names
}
