// web_fetch_test 锁定协议白名单、大小截断与失败关闭。
//
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestWebFetchRejectsNonHTTP 锁定非 http/https 拒绝。
func TestWebFetchRejectsNonHTTP(t *testing.T) {
	result := WebFetch{}.Execute(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`))
	if result.OK {
		t.Fatal("file:// 应拒绝")
	}
	result = WebFetch{}.Execute(context.Background(), json.RawMessage(`{"url":""}`))
	if result.OK {
		t.Fatal("空 url 应拒绝")
	}
}

// TestWebFetchTruncatesLargeBody 锁定超限截断与预览。
func TestWebFetchTruncatesLargeBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", WebFetchMaxBytes+100)))
	}))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	result := WebFetch{Client: client}.Execute(context.Background(), json.RawMessage(`{"url":"`+server.URL+`"}`))
	if !result.OK || !result.Truncated {
		t.Fatalf("应成功且截断: %+v", result)
	}
	var payload WebFetchResult
	if err := json.Unmarshal(result.Data, &payload); err != nil || payload.Bytes != WebFetchMaxBytes {
		t.Fatalf("载荷不符: %v %+v", err, payload)
	}
}

// TestWebFetchSideEffectProcess 锁定副作用分类为 Process。
func TestWebFetchSideEffectProcess(t *testing.T) {
	if got := (WebFetch{}).SideEffect(); got != SideEffectProcess {
		t.Fatalf("副作用应为 process，得到 %q", got)
	}
}
