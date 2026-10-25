// schedule.go 实现 WFL-06 定时触发与错过执行管理：明确唤醒/休眠/错过/并发/
// 权限/预算/取消合同；可配置、暂停、查看下次触发与真实记录；不借用未授权
// 宿主 cron；无运行支持时显式阻塞。
// Author: fxbin
package workflow

import (
	"fmt"
	"strings"
	"time"
)

// TriggerState 是定时任务状态。
type TriggerState string

const (
	// TriggerActive 表示可触发。
	TriggerActive TriggerState = "active"
	// TriggerPaused 表示已暂停，不触发。
	TriggerPaused TriggerState = "paused"
	// TriggerBlocked 表示运行时能力缺失，显式阻塞。
	TriggerBlocked TriggerState = "blocked"
)

// MissPolicy 是错过触发的处置策略。
type MissPolicy string

const (
	// MissSkip 跳过本次。
	MissSkip MissPolicy = "skip"
	// MissRunOnce 唤醒后补跑一次。
	MissRunOnce MissPolicy = "run_once"
	// MissFail 标记失败，需人工介入。
	MissFail MissPolicy = "fail"
)

// ScheduleSpec 是定时触发配置。
type ScheduleSpec struct {
	ID       string        `json:"id"`
	Interval time.Duration `json:"interval"`
	// NextAt 是下次触发时刻。
	NextAt time.Time    `json:"next_at"`
	State  TriggerState `json:"state"`
	Miss   MissPolicy   `json:"miss_policy"`
	// MaxConcurrent 同时运行上限；0 表示禁止并发。
	MaxConcurrent int `json:"max_concurrent"`
	// OwnerRun 是授权发起方；空值禁止触发。
	OwnerRun string `json:"owner_run,omitempty"`
}

// TriggerRecord 是一次真实触发/错过记录。
type TriggerRecord struct {
	ScheduleID string    `json:"schedule_id"`
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"` // fired | missed | skipped | blocked
	Note       string    `json:"note,omitempty"`
}

// Scheduler 管理定时任务与真实记录；不调用宿主 cron。
type Scheduler struct {
	specs   map[string]*ScheduleSpec
	records []TriggerRecord
	// running 计数用于并发上限。
	running map[string]int
	// hasRuntime 为 false 时全部阻塞（无运行支持）。
	hasRuntime bool
}

// NewScheduler 创建调度器；hasRuntime 显式声明运行能力。
func NewScheduler(hasRuntime bool) *Scheduler {
	return &Scheduler{
		specs:      make(map[string]*ScheduleSpec),
		running:    make(map[string]int),
		hasRuntime: hasRuntime,
	}
}

// Upsert 注册或更新任务；间隔必须为正，ID 必填。
func (scheduler *Scheduler) Upsert(spec ScheduleSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return fmt.Errorf("schedule id is required")
	}
	if spec.Interval <= 0 {
		return fmt.Errorf("interval must be positive")
	}
	if spec.Miss == "" {
		spec.Miss = MissSkip
	}
	if spec.NextAt.IsZero() {
		spec.NextAt = time.Now().UTC().Add(spec.Interval)
	}
	if !scheduler.hasRuntime {
		spec.State = TriggerBlocked
	} else if spec.State == "" {
		spec.State = TriggerActive
	}
	copied := spec
	scheduler.specs[spec.ID] = &copied
	return nil
}

// Pause 暂停任务。
func (scheduler *Scheduler) Pause(id string) error {
	spec, found := scheduler.specs[id]
	if !found {
		return fmt.Errorf("schedule %q not found", id)
	}
	spec.State = TriggerPaused
	return nil
}

// Resume 恢复任务；无运行支持时保持阻塞。
func (scheduler *Scheduler) Resume(id string) error {
	spec, found := scheduler.specs[id]
	if !found {
		return fmt.Errorf("schedule %q not found", id)
	}
	if !scheduler.hasRuntime {
		spec.State = TriggerBlocked
		scheduler.record(id, "blocked", "runtime unavailable")
		return fmt.Errorf("runtime support is required to resume %q", id)
	}
	spec.State = TriggerActive
	return nil
}

// NextAt 返回下次触发时刻。
func (scheduler *Scheduler) NextAt(id string) (time.Time, error) {
	spec, found := scheduler.specs[id]
	if !found {
		return time.Time{}, fmt.Errorf("schedule %q not found", id)
	}
	return spec.NextAt, nil
}

// Tick 在 now 时刻推进调度：返回本次应触发的 ID 列表并记录真实事实。
func (scheduler *Scheduler) Tick(now time.Time) []string {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	fired := make([]string, 0, 2)
	for id, spec := range scheduler.specs {
		if spec.State == TriggerPaused {
			continue
		}
		if spec.State == TriggerBlocked {
			if now.After(spec.NextAt) {
				scheduler.record(id, "blocked", "runtime unavailable")
				spec.NextAt = now.Add(spec.Interval)
			}
			continue
		}
		if now.Before(spec.NextAt) {
			continue
		}
		// 休眠/错过：now 远超 NextAt 时按 MissPolicy 处理
		missed := now.Sub(spec.NextAt) > spec.Interval
		if missed {
			switch spec.Miss {
			case MissSkip:
				scheduler.record(id, "missed", "skipped by policy")
				spec.NextAt = now.Add(spec.Interval)
				continue
			case MissFail:
				scheduler.record(id, "missed", "marked failed")
				spec.State = TriggerPaused
				continue
			case MissRunOnce:
				scheduler.record(id, "missed", "catch-up once")
			}
		}
		if scheduler.running[id] > 0 && spec.MaxConcurrent == 0 {
			scheduler.record(id, "skipped", "previous run still active")
			spec.NextAt = now.Add(spec.Interval)
			continue
		}
		if spec.MaxConcurrent > 0 && scheduler.running[id] >= spec.MaxConcurrent {
			scheduler.record(id, "skipped", "concurrency limit")
			spec.NextAt = now.Add(spec.Interval)
			continue
		}
		if strings.TrimSpace(spec.OwnerRun) == "" {
			scheduler.record(id, "skipped", "no authorized owner run")
			spec.NextAt = now.Add(spec.Interval)
			continue
		}
		scheduler.running[id]++
		scheduler.record(id, "fired", "")
		fired = append(fired, id)
		spec.NextAt = now.Add(spec.Interval)
	}
	return fired
}

// Done 释放一次运行槽。
func (scheduler *Scheduler) Done(id string) {
	if scheduler.running[id] > 0 {
		scheduler.running[id]--
	}
}

// Records 返回真实触发记录副本。
func (scheduler *Scheduler) Records() []TriggerRecord {
	out := make([]TriggerRecord, len(scheduler.records))
	copy(out, scheduler.records)
	return out
}

func (scheduler *Scheduler) record(id, kind, note string) {
	scheduler.records = append(scheduler.records, TriggerRecord{
		ScheduleID: id,
		At:         time.Now().UTC(),
		Kind:       kind,
		Note:       note,
	})
}
