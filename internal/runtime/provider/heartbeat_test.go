// heartbeat_test 锁定心跳探测与重试记录的可见状态。
//
// Author: fxbin
package provider

import (
	"context"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// TestHeartbeatNotConfigured 锁定未装配 ping 时失败分类。
func TestHeartbeatNotConfigured(t *testing.T) {
	tracker := NewHeartbeatTracker(nil)
	probe := tracker.Heartbeat(context.Background())
	if probe.OK || probe.ErrorClass != "not_configured" {
		t.Fatalf("未配置应失败: %+v", probe)
	}
}

// TestHeartbeatRecordsProbeAndRetries 锁定探测与重试记录。
func TestHeartbeatRecordsProbeAndRetries(t *testing.T) {
	tracker := NewHeartbeatTracker(func(context.Context) ports.HeartbeatProbe {
		return ports.HeartbeatProbe{OK: true, Latency: 5 * time.Millisecond}
	})
	probe := tracker.Heartbeat(context.Background())
	if !probe.OK || probe.Latency != 5*time.Millisecond {
		t.Fatalf("探测不符: %+v", probe)
	}
	if tracker.LastHeartbeat() != probe {
		t.Fatal("LastHeartbeat 应与返回一致")
	}
	tracker.RecordRetry(1, "timeout", time.Unix(10, 0).UTC())
	tracker.RecordRetry(2, "rate_limited", time.Unix(11, 0).UTC())
	retries := tracker.RecentRetries()
	if len(retries) != 2 || retries[0].Attempt != 2 || retries[1].Attempt != 1 {
		t.Fatalf("重试记录应新到旧: %+v", retries)
	}
}
