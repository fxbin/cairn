// catalog_sync_test 锁定目录同步状态与 TTL 失效。
//
// Author: fxbin
package mcpclient

import (
	"testing"
	"time"
)

// TestCatalogSyncTTL 锁定超时降为 stale。
func TestCatalogSyncTTL(t *testing.T) {
	syncer := NewCatalogSyncer(5 * time.Minute)
	at := time.Unix(1000, 0).UTC()
	syncer.Record("docs", "sha256:1", 3, at)
	state, ok := syncer.Status("docs", at.Add(time.Minute))
	if !ok || state.Status != CatalogFresh {
		t.Fatalf("应新鲜: %+v", state)
	}
	state, _ = syncer.Status("docs", at.Add(10*time.Minute))
	if state.Status != CatalogStale {
		t.Fatalf("超 TTL 应 stale: %+v", state)
	}
}

// TestCatalogSyncDriftAndInvalid 锁定漂移与损坏标记。
func TestCatalogSyncDriftAndInvalid(t *testing.T) {
	syncer := NewCatalogSyncer(0)
	syncer.Record("docs", "d", 1, time.Now().UTC())
	syncer.MarkDrifted("docs")
	state, _ := syncer.Status("docs", time.Now().UTC())
	if state.Status != CatalogDrifted {
		t.Fatalf("应 drifted: %+v", state)
	}
	syncer.MarkInvalid("docs")
	state, _ = syncer.Status("docs", time.Now().UTC())
	if state.Status != CatalogInvalid {
		t.Fatalf("应 invalid: %+v", state)
	}
	if _, ok := syncer.Status("none", time.Now().UTC()); ok {
		t.Fatal("未知服务器不应存在")
	}
}
