// parallel_readonly_test 锁定只读并行分组与副作用串行。
//
// Author: fxbin
package tools

import "testing"

// TestPlanParallelGroups 锁定只读聚合与副作用隔离。
func TestPlanParallelGroups(t *testing.T) {
	names := []string{"read_file", "search_text", "exec_command", "read_file", "apply_patch"}
	readonly := func(name string) bool { return name == "read_file" || name == "search_text" }
	groups := PlanParallelGroups(names, readonly)
	if len(groups) != 4 {
		t.Fatalf("应分为 4 组，得到 %+v", groups)
	}
	if !groups[0].Parallel || len(groups[0].Indexes) != 2 {
		t.Fatalf("首组应并行且含 2 下标: %+v", groups[0])
	}
	if groups[1].Parallel || groups[1].Indexes[0] != 2 {
		t.Fatalf("副作用应串行: %+v", groups[1])
	}
	if !groups[2].Parallel || groups[2].Indexes[0] != 3 {
		t.Fatalf("第三组应回到只读并行: %+v", groups[2])
	}
	if groups[3].Parallel || groups[3].Indexes[0] != 4 {
		t.Fatalf("末组应串行: %+v", groups[3])
	}
}

// TestPlanParallelGroupsFailClosed 锁定无判定函数时全串行。
func TestPlanParallelGroupsFailClosed(t *testing.T) {
	groups := PlanParallelGroups([]string{"read_file", "read_file"}, nil)
	if len(groups) != 2 {
		t.Fatalf("应全部串行: %+v", groups)
	}
	for _, group := range groups {
		if group.Parallel {
			t.Fatal("无判定函数时不应并行")
		}
	}
}

// TestReadonlyToolNames 锁定副作用 none 的工具归入只读集合。
func TestReadonlyToolNames(t *testing.T) {
	empty := ReadonlyToolNames(nil)
	if len(empty) != 0 {
		t.Fatalf("nil registry 应为空: %v", empty)
	}
}
