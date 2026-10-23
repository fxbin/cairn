// safety_event_test 锁定安全事件记录、边界与统计。
//
// Author: fxbin
package policy

import (
	"testing"
	"time"
)

// TestSafetyLogRecordAndSnapshot 锁定记录与快照。
func TestSafetyLogRecordAndSnapshot(t *testing.T) {
	log := NewSafetyLog(2)
	log.Record(SafetyEvent{ToolName: "exec_command", Action: SafetyBlocked, RuleID: "r1"})
	log.Record(SafetyEvent{ToolName: "apply_patch", Action: SafetyPrompted})
	log.Record(SafetyEvent{ToolName: "web_fetch", Action: SafetySandboxed})
	snapshot := log.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("应有界保留 2 条: %+v", snapshot)
	}
	if snapshot[0].ToolName != "web_fetch" {
		t.Fatalf("应新到旧: %+v", snapshot)
	}
	counts := log.CountByAction()
	if counts[SafetySandboxed] != 1 || counts[SafetyBlocked] != 0 {
		t.Fatalf("统计不符: %v", counts)
	}
}

// TestSafetyEventTimestamp 锁定零时间自动填充。
func TestSafetyEventTimestamp(t *testing.T) {
	log := NewSafetyLog(0)
	log.Record(SafetyEvent{ToolName: "x", Action: SafetyHooked})
	snapshot := log.Snapshot()
	if snapshot[0].OccurredAt.IsZero() {
		t.Fatal("应填充时间戳")
	}
	if log.limit != 64 {
		t.Fatalf("默认上限不符: %d", log.limit)
	}
	_ = time.Now()
}
