// web_search_test 锁定查询校验、端点白名单与副作用分类。
//
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWebSearchRejectsInvalid 锁定空查询与未配置端点失败关闭。
func TestWebSearchRejectsInvalid(t *testing.T) {
	if (WebSearch{}).Execute(context.Background(), json.RawMessage(`{"query":"cairn"}`)).OK {
		t.Fatal("未配置端点应失败")
	}
	if (WebSearch{Endpoint: "https://example.com/search"}).Execute(context.Background(), json.RawMessage(`{"query":""}`)).OK {
		t.Fatal("空 query 应失败")
	}
	if (WebSearch{Endpoint: "file:///tmp/x"}).Execute(context.Background(), json.RawMessage(`{"query":"x"}`)).OK {
		t.Fatal("file:// 端点应失败")
	}
}

// TestWebSearchQueriesEndpoint 锁定 query 参数传入端点并返回 ok。
func TestWebSearchQueriesEndpoint(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	result := WebSearch{Client: client, Endpoint: server.URL + "/search"}.Execute(context.Background(), json.RawMessage(`{"query":"deepseek harness","limit":3}`))
	if !result.OK {
		t.Fatalf("应成功: %+v", result)
	}
	if gotQuery != "deepseek harness" {
		t.Fatalf("query 未传递: %q", gotQuery)
	}
	var payload WebSearchResult
	if err := json.Unmarshal(result.Data, &payload); err != nil || payload.Query != "deepseek harness" {
		t.Fatalf("载荷不符: %v %+v", err, payload)
	}
}

// TestWebSearchSideEffectProcess 锁定副作用分类。
func TestWebSearchSideEffectProcess(t *testing.T) {
	if got := (WebSearch{}).SideEffect(); got != SideEffectProcess {
		t.Fatalf("副作用应为 process，得到 %q", got)
	}
}
