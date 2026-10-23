// step_test 锁定 WFL-04 步骤进度、失败与恢复语义。
//
// Author: fxbin
package workflow

import (
	"testing"
	"time"
)

func mustPlan(t *testing.T) *Plan {
	t.Helper()
	plan, err := NewPlan("run-1", []Step{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// TestPlanMarkAndResume 锁定状态迁移与恢复位置。
func TestPlanMarkAndResume(t *testing.T) {
	plan := mustPlan(t)
	now := time.Unix(10, 0).UTC()
	if err := plan.MarkStep("s1", StepRunning, StepIO{}, StepIO{}, "", now); err != nil {
		t.Fatal(err)
	}
	if err := plan.MarkStep("s1", StepSucceeded, StepIO{Ref: "in://1"}, StepIO{Ref: "out://1"}, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := plan.MarkStep("s2", StepRunning, StepIO{}, StepIO{}, "", now); err != nil {
		t.Fatal(err)
	}
	if err := plan.MarkStep("s2", StepFailed, StepIO{}, StepIO{}, "timeout", now); err != nil {
		t.Fatal(err)
	}
	if plan.ResumeFrom() != 1 {
		t.Fatalf("恢复下标应为 1，得到 %d", plan.ResumeFrom())
	}
	if err := plan.MarkStep("s1", StepFailed, StepIO{}, StepIO{}, "x", now); err == nil {
		t.Fatal("成功后不应再改状态")
	}
}

// TestCancelRemainingAndReconcile 锁定取消位置与不确定副作用阻断重跑。
func TestCancelRemainingAndReconcile(t *testing.T) {
	plan := mustPlan(t)
	now := time.Unix(20, 0).UTC()
	_ = plan.MarkStep("s1", StepRunning, StepIO{}, StepIO{}, "", now)
	_ = plan.MarkStep("s1", StepUncertain, StepIO{}, StepIO{}, "network_blip", now)
	if ok, _ := plan.CanRestart(); ok {
		t.Fatal("不确定副作用应阻断重跑")
	}
	if len(plan.RequiresReconcile()) != 1 {
		t.Fatalf("应标出需对账步骤: %+v", plan.RequiresReconcile())
	}
	if err := plan.MarkStep("s1", StepSucceeded, StepIO{}, StepIO{}, "", now); err != nil {
		t.Fatal(err)
	}
	if ok, reason := plan.CanRestart(); !ok {
		t.Fatalf("对账后应允许: %s", reason)
	}
	if pos := plan.CancelRemaining(now); pos != 1 {
		t.Fatalf("取消位置应为 1，得到 %d", pos)
	}
}

// TestNewPlanRejectsDuplicates 锁定 ID 约束。
func TestNewPlanRejectsDuplicates(t *testing.T) {
	if _, err := NewPlan("run", []Step{{ID: "a"}, {ID: "a"}}); err == nil {
		t.Fatal("重复 ID 应拒绝")
	}
	if _, err := NewPlan("", []Step{{ID: "a"}}); err == nil {
		t.Fatal("空 run id 应拒绝")
	}
}
