// schedule_test 锁定 WFL-06 定时触发、错过策略与运行支持阻塞。
//
// Author: fxbin
package workflow

import (
	"testing"
	"time"
)

// TestSchedulerUpsertAndNextAt 锁定配置校验与下次触发。
func TestSchedulerUpsertAndNextAt(t *testing.T) {
	scheduler := NewScheduler(true)
	if err := scheduler.Upsert(ScheduleSpec{ID: "", Interval: time.Minute}); err == nil {
		t.Fatal("空 ID 应拒绝")
	}
	if err := scheduler.Upsert(ScheduleSpec{ID: "s1", Interval: 0}); err == nil {
		t.Fatal("零间隔应拒绝")
	}
	base := time.Unix(1000, 0).UTC()
	if err := scheduler.Upsert(ScheduleSpec{ID: "s1", Interval: time.Minute, NextAt: base, OwnerRun: "run-1"}); err != nil {
		t.Fatal(err)
	}
	next, err := scheduler.NextAt("s1")
	if err != nil || !next.Equal(base) {
		t.Fatalf("下次触发不符: %v %v", next, err)
	}
	if err := scheduler.Pause("s1"); err != nil {
		t.Fatal(err)
	}
	if fired := scheduler.Tick(base.Add(time.Hour)); len(fired) != 0 {
		t.Fatalf("暂停不应触发: %v", fired)
	}
}

// TestSchedulerMissPolicies 锁定错过策略。
func TestSchedulerMissPolicies(t *testing.T) {
	base := time.Unix(2000, 0).UTC()
	skip := NewScheduler(true)
	_ = skip.Upsert(ScheduleSpec{ID: "s1", Interval: time.Minute, NextAt: base, Miss: MissSkip, OwnerRun: "run-1"})
	if fired := skip.Tick(base.Add(10 * time.Minute)); len(fired) != 0 {
		t.Fatalf("MissSkip 不应补跑: %v", fired)
	}
	records := skip.Records()
	if len(records) == 0 || records[0].Kind != "missed" {
		t.Fatalf("应记录 missed: %+v", records)
	}

	once := NewScheduler(true)
	_ = once.Upsert(ScheduleSpec{ID: "s1", Interval: time.Minute, NextAt: base, Miss: MissRunOnce, OwnerRun: "run-1"})
	if fired := once.Tick(base.Add(10 * time.Minute)); len(fired) != 1 {
		t.Fatalf("MissRunOnce 应补跑一次: %v", fired)
	}

	fail := NewScheduler(true)
	_ = fail.Upsert(ScheduleSpec{ID: "s1", Interval: time.Minute, NextAt: base, Miss: MissFail, OwnerRun: "run-1"})
	fail.Tick(base.Add(10 * time.Minute))
	if records := fail.Records(); len(records) == 0 || records[len(records)-1].Kind != "missed" {
		t.Fatalf("MissFail 应记录: %+v", records)
	}
}

// TestSchedulerBlocksWithoutRuntime 锁定无运行支持时显式阻塞。
func TestSchedulerBlocksWithoutRuntime(t *testing.T) {
	scheduler := NewScheduler(false)
	_ = scheduler.Upsert(ScheduleSpec{ID: "s1", Interval: time.Minute, NextAt: time.Unix(1, 0).UTC(), OwnerRun: "run-1"})
	spec := scheduler.specs["s1"]
	if spec.State != TriggerBlocked {
		t.Fatalf("应阻塞: %+v", spec)
	}
	if err := scheduler.Resume("s1"); err == nil {
		t.Fatal("无运行支持恢复应失败")
	}
	fired := scheduler.Tick(time.Unix(100, 0).UTC())
	if len(fired) != 0 {
		t.Fatalf("阻塞不应触发: %v", fired)
	}
	if records := scheduler.Records(); len(records) == 0 || records[len(records)-1].Kind != "blocked" {
		t.Fatalf("应记录 blocked: %+v", records)
	}
}

// TestSchedulerConcurrencyAndOwner 锁定并发上限与授权属主。
func TestSchedulerConcurrencyAndOwner(t *testing.T) {
	base := time.Unix(3000, 0).UTC()
	scheduler := NewScheduler(true)
	_ = scheduler.Upsert(ScheduleSpec{ID: "s1", Interval: time.Minute, NextAt: base, MaxConcurrent: 0, OwnerRun: "run-1"})
	if fired := scheduler.Tick(base.Add(time.Second)); len(fired) != 1 {
		t.Fatalf("应触发一次: %v", fired)
	}
	// 未释放时再次触发应跳过
	if fired := scheduler.Tick(base.Add(2 * time.Second)); len(fired) != 0 {
		t.Fatalf("并发上限 0 不应重入: %v", fired)
	}
	scheduler.Done("s1")

	// 无属主
	noOwner := NewScheduler(true)
	_ = noOwner.Upsert(ScheduleSpec{ID: "s2", Interval: time.Minute, NextAt: base})
	if fired := noOwner.Tick(base.Add(time.Second)); len(fired) != 0 {
		t.Fatalf("无属主不应触发: %v", fired)
	}
}
