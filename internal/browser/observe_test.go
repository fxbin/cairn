// observe_test 锁定 BRW-03 观察时效与元素绑定。
//
// Author: fxbin
package browser

import (
	"testing"
	"time"
)

// TestObservationFreshness 锁定截图过期须重观察。
func TestObservationFreshness(t *testing.T) {
	store := NewObservationStore()
	now := time.Unix(1000, 0).UTC()
	if err := store.Record(Observation{ID: "obs-1", TabID: "tab-1", Kind: ObsScreenshot, CapturedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequireFresh("obs-1", now.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("应新鲜: %v", err)
	}
	if _, err := store.RequireFresh("obs-1", now.Add(2*time.Minute), time.Minute); err == nil {
		t.Fatal("超 maxAge 应判定过期")
	}
	// ExpiresAt 优先
	_ = store.Record(Observation{ID: "obs-2", TabID: "tab-1", Kind: ObsScreenshot, CapturedAt: now, ExpiresAt: now.Add(10 * time.Second)})
	if _, err := store.RequireFresh("obs-2", now.Add(20*time.Second), time.Hour); err == nil {
		t.Fatal("ExpiresAt 过期应失败")
	}
}

// TestBindElementRejectsMismatch 锁定元素绑定与类别约束。
func TestBindElementRejectsMismatch(t *testing.T) {
	store := NewObservationStore()
	_ = store.Record(Observation{ID: "obs-a", TabID: "tab-1", Kind: ObsAccessibilityTree, CapturedAt: time.Now().UTC()})
	_ = store.Record(Observation{ID: "obs-s", TabID: "tab-1", Kind: ObsScreenshot, CapturedAt: time.Now().UTC()})
	target, err := BindElement(store, "obs-a", "el-1")
	if err != nil || target.TabID != "tab-1" || target.ElementID != "el-1" {
		t.Fatalf("绑定应成功: %+v %v", target, err)
	}
	if _, err := BindElement(store, "obs-s", "el-1"); err == nil {
		t.Fatal("截图观察不应承载元素引用")
	}
	if _, err := BindElement(store, "obs-a", ""); err == nil {
		t.Fatal("空元素 ID 应拒绝")
	}
	if _, err := BindElement(store, "missing", "el-1"); err == nil {
		t.Fatal("缺失观察应拒绝")
	}
}
