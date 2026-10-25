package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// TestDelegateTaskSpecAndFailClosedExecution 锁定委派工具的声明、副作用
// 分类与通用执行路径的失败关闭行为。
func TestDelegateTaskSpecAndFailClosedExecution(t *testing.T) {
	tool := DelegateTask{}
	spec := tool.Spec()
	if spec.Name != DelegateTaskName {
		t.Fatalf("spec name = %q", spec.Name)
	}
	if tool.SideEffect() != SideEffectProcess {
		t.Fatalf("side effect = %q", tool.SideEffect())
	}
	result := tool.Execute(context.Background(), json.RawMessage(`{"task":"child work"}`))
	if result.OK || result.Code != "not_wired" {
		t.Fatalf("generic execution must fail closed: %+v", result)
	}
}
