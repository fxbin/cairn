// batch_plan_test 锁定批次并行计划的分组与失败关闭。
//
// Author: fxbin
package agent

import (
	"testing"

	"github.com/fxbin/cairn/internal/ports"
)

// TestPlanBatchGroupsReadonly 锁定只读并行与副作用串行。
func TestPlanBatchGroupsReadonly(t *testing.T) {
	calls := []ports.ToolCall{
		{ID: "c1", Name: "read_file"},
		{ID: "c2", Name: "search_text"},
		{ID: "c3", Name: "exec_command"},
	}
	readonly := func(name string) bool { return name != "exec_command" }
	plan := PlanBatch(calls, readonly)
	if plan.Groups != 2 || !plan.Parallelizable {
		t.Fatalf("计划不符: %+v", plan)
	}
	if !plan.Entries[0].Parallel || plan.Entries[2].Parallel {
		t.Fatalf("并行标记不符: %+v", plan.Entries)
	}
}

// TestPlanBatchFailClosed 无判定函数时全串行。
func TestPlanBatchFailClosed(t *testing.T) {
	plan := PlanBatch([]ports.ToolCall{{ID: "c1", Name: "read_file"}}, nil)
	if plan.Parallelizable || plan.Entries[0].Parallel {
		t.Fatalf("应全串行: %+v", plan)
	}
}
