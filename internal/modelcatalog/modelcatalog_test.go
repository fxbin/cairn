package modelcatalog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// catalogFixture 是含两条可消费条目与一条缺 limit 条目的最小目录负载。
func catalogFixture() []byte {
	return []byte(`{
		"zai": {"name":"Z.ai","models":{
			"glm-5.2":{"name":"GLM-5.2","family":"glm","tool_call":true,"reasoning":true,"temperature":true,
				"limit":{"context":1048576,"output":131072},"cost":{"input":1.4,"output":4.4}},
			"glm-5.2-air":{"name":"GLM 5.2 Air","family":"glm","tool_call":true,
				"limit":{"context":131072,"output":8192},"cost":{"input":0.2,"output":1.1}},
			"broken-entry":{"name":"Broken","tool_call":true}
		}},
		"deepseek": {"name":"DeepSeek","models":{
			"deepseek-v4-flash":{"name":"DeepSeek V4 Flash","family":"deepseek","tool_call":true,
				"limit":{"context":128000,"output":16384},"cost":{"input":0.14,"output":0.28}}
		}}
	}`)
}

// TestParseCatalogValidatesConsumedFields 锁定投影严格性：消费字段缺失的
// 条目跳过计数、合法条目字段保真、全坏负载失败关闭。
func TestParseCatalogValidatesConsumedFields(t *testing.T) {
	fetchedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	catalog, err := parseCatalog(catalogFixture(), fetchedAt)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(catalog.Entries) != 3 || catalog.SkippedCount != 1 {
		t.Fatalf("entries=%d skipped=%d, want 3/1", len(catalog.Entries), catalog.SkippedCount)
	}
	var glm ModelEntry
	for _, entry := range catalog.Entries {
		if entry.ID == "glm-5.2" {
			glm = entry
		}
	}
	if glm.Provider != "zai" || !glm.ToolCall || !glm.Reasoning || glm.ContextTokens != 1048576 || glm.OutputTokens != 131072 ||
		glm.InputUSDPerMillion != 1.4 || glm.OutputUSDPerMillion != 4.4 {
		t.Fatalf("glm projection drifted: %+v", glm)
	}
	if _, err := parseCatalog([]byte("{\"a\":{\"models\":{\"x\":{\"name\":\"X\"}}}}"), fetchedAt); err == nil {
		t.Fatal("all-broken payload must fail closed")
	}
	if _, err := parseCatalog([]byte("{\"truncated"), fetchedAt); err == nil {
		t.Fatal("truncated payload must fail closed")
	}
}

// TestLoadCacheTTLAndAtomicWrite 锁定缓存行为：新鲜缓存不出网、过期缓存
// 走网络并原子回写、版本不符拒绝。
func TestLoadCacheTTLAndAtomicWrite(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	fetches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		w.Write(catalogFixture())
	}))
	defer server.Close()
	cachePath := filepath.Join(t.TempDir(), "nested", "catalog.json")

	fresh, err := Load(Options{CatalogURL: server.URL, CachePath: cachePath, Now: func() time.Time { return now }})
	if err != nil || fresh.FromStaleCache || len(fresh.Entries) != 3 {
		t.Fatalf("initial load: entries=%d stale=%v err=%v", len(fresh.Entries), fresh.FromStaleCache, err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file must exist: %v", err)
	}
	if _, err := Load(Options{CatalogURL: server.URL, CachePath: cachePath, Now: func() time.Time { return now.Add(catalogTTL - time.Minute) }}); err != nil || fetches != 1 {
		t.Fatalf("fresh cache must skip network: fetches=%d err=%v", fetches, err)
	}
	expired, err := Load(Options{CatalogURL: server.URL, CachePath: cachePath, Now: func() time.Time { return now.Add(catalogTTL + time.Minute) }})
	if err != nil || fetches != 2 || expired.FromStaleCache {
		t.Fatalf("expired cache must refresh: fetches=%d stale=%v err=%v", fetches, expired.FromStaleCache, err)
	}
}

// TestLoadFallsBackToStaleCache 锁定弱网降级：网络不可达且缓存存在时返
// 回陈旧目录并标注；两者皆无返回原始错误。
func TestLoadFallsBackToStaleCache(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(catalogFixture())
	}))
	cachePath := filepath.Join(t.TempDir(), "catalog.json")
	if _, err := Load(Options{CatalogURL: server.URL, CachePath: cachePath, Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	server.Close()
	stale, err := Load(Options{CatalogURL: server.URL, CachePath: cachePath, Now: func() time.Time { return now.Add(72 * time.Hour) }})
	if err != nil || !stale.FromStaleCache || len(stale.Entries) != 3 {
		t.Fatalf("stale fallback: entries=%d stale=%v err=%v", len(stale.Entries), stale.FromStaleCache, err)
	}
	if _, err := Load(Options{CatalogURL: server.URL, CachePath: "", Now: func() time.Time { return now }}); err == nil {
		t.Fatal("no cache and dead network must fail")
	}
}

// TestLookupFuzzyMatchesGLM 锁定向导实际会用的模糊命中形态：小写连字符
// 输入精确命中 GLM-5.2、族名部分命中返回多候选、无命中返回空。
func TestLookupFuzzyMatchesGLM(t *testing.T) {
	catalog, err := parseCatalog(catalogFixture(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	exact := catalog.Lookup("glm-5.2")
	if len(exact) != 1 || exact[0].ID != "glm-5.2" {
		t.Fatalf("normalized query must hit exactly: %+v", exact)
	}
	family := catalog.Lookup("glm")
	if len(family) < 2 {
		t.Fatalf("family query must surface both glm entries: %+v", family)
	}
	if got := catalog.Lookup("gpt-99"); len(got) != 0 {
		t.Fatalf("unknown model must return empty: %+v", got)
	}
	if !strings.EqualFold(normalizeModelName("GLM_5.2 Air"), "glm-5.2-air") {
		t.Fatal("normalizer must fold case, underscores and spaces")
	}
}

// TestCacheWrapperRejectsForeignVersion 锁定缓存版本失败关闭。
func TestCacheWrapperRejectsForeignVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	payload := "{\"schema_version\":99,\"fetched_at\":\"2026-08-29T00:00:00Z\",\"entries\":[{\"provider\":\"x\",\"id\":\"y\",\"name\":\"Y\",\"tool_call\":true,\"context_tokens\":8}]}"
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCache(path); err == nil {
		t.Fatal("foreign schema version must be rejected")
	}
}
