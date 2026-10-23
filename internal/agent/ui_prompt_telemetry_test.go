// ui_prompt_telemetry_test 锁定活跃/等待阶段记录与时长统计。
//
// Author: fxbin
package agent

import (
	"testing"
	"time"
)

// TestUIPhaseTrackerRecords 锁定阶段切换与时长。
func TestUIPhaseTrackerRecords(t *testing.T) {
	tracker := NewUIPhaseTracker(0)
	start := time.Unix(100, 0).UTC()
	tracker.Begin(UIPhaseActive, "tool", start)
	tracker.Begin(UIPhasePrompt, "approval", start.Add(2*time.Second))
	tracker.End(start.Add(5 * time.Second))
	snapshot := tracker.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("应有 2 条记录: %+v", snapshot)
	}
	if snapshot[0].Phase != UIPhasePrompt || snapshot[1].Phase != UIPhaseActive {
		t.Fatalf("顺序不符: %+v", snapshot)
	}
	totals := tracker.TotalByPhase()
	if totals[UIPhaseActive] != 2*time.Second {
		t.Fatalf("active 时长不符: %v", totals)
	}
	if totals[UIPhasePrompt] != 3*time.Second {
		t.Fatalf("prompt 时长不符: %v", totals)
	}
}
