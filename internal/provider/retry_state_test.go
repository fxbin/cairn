// retry_state_test 锁定重试状态迁移与并发快照。
//
// Author: fxbin
package provider

import (
	"testing"
	"time"
)

// TestRetryTrackerTransitions 锁定 retrying→gave_up/idle 迁移。
func TestRetryTrackerTransitions(t *testing.T) {
	tracker := NewRetryTracker()
	next := time.Now().Add(time.Second).UTC()
	tracker.BeginRetrying("op-1", "timeout", 1, next)
	state := tracker.Snapshot()["op-1"]
	if state.Phase != RetryPhaseRetrying || state.Attempts != 1 || state.LastError != "timeout" {
		t.Fatalf("重试态不符: %+v", state)
	}
	if tracker.ActiveCount() != 1 {
		t.Fatalf("活跃数应为 1，得到 %d", tracker.ActiveCount())
	}
	tracker.MarkGaveUp("op-1", "timeout", 3)
	state = tracker.Snapshot()["op-1"]
	if state.Phase != RetryPhaseGaveUp || state.Attempts != 3 {
		t.Fatalf("放弃态不符: %+v", state)
	}
	if tracker.ActiveCount() != 0 {
		t.Fatal("放弃后不应计入活跃")
	}
	tracker.MarkIdle("op-1")
	if len(tracker.Snapshot()) != 0 {
		t.Fatal("idle 应清除状态")
	}
}
