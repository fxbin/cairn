// web_fetch.go 实现受限 HTTP 抓取工具：只允许 http/https，限制大小与超时，
// 结果截断预览；网络副作用归类为 Process，需经 Policy 与审批。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// WebFetch 限制约束单次抓取的字节、超时与协议范围。
const (
	WebFetchMaxBytes     = 256 * 1024
	WebFetchTimeout      = 15 * time.Second
	WebFetchPreviewBytes = 2048
)

// WebFetch 是受限网页抓取工具。
type WebFetch struct {
	Client *http.Client
}

// Spec 返回工具声明。
func (WebFetch) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "web_fetch",
		Description: "抓取 http/https 网页文本（有限大小与超时）。需要 url。",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["url"],"properties":{"url":{"type":"string","minLength":1}}}`),
	}
}

// SideEffect 声明网络访问属于进程级副作用。
func (WebFetch) SideEffect() SideEffectClass { return SideEffectProcess }

// WebFetchResult 是抓取结果载荷。
type WebFetchResult struct {
	URL       string `json:"url"`
	Status    int    `json:"status"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Preview   string `json:"preview"`
}

// Execute 校验协议与主机名后抓取页面；失败关闭，不跟随无限重定向。
func (tool WebFetch) Execute(ctx context.Context, raw json.RawMessage) Result {
	var request struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return Failure("invalid_arguments", "web_fetch 参数解析失败")
	}
	target := strings.TrimSpace(request.URL)
	if target == "" {
		return Failure("invalid_arguments", "url 不能为空")
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Failure("invalid_url", "仅允许 http/https URL")
	}
	if parsed.Hostname() == "" {
		return Failure("invalid_url", "URL 缺少主机名")
	}
	client := tool.Client
	if client == nil {
		client = &http.Client{Timeout: WebFetchTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Failure("internal", err.Error())
	}
	resp, err := client.Do(req)
	if err != nil {
		return Failure("fetch_failed", err.Error())
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, WebFetchMaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return Failure("fetch_failed", err.Error())
	}
	truncated := len(body) > WebFetchMaxBytes
	if truncated {
		body = body[:WebFetchMaxBytes]
	}
	preview := string(body)
	if len(preview) > WebFetchPreviewBytes {
		preview = preview[:WebFetchPreviewBytes] + "…"
	}
	payload, err := json.Marshal(WebFetchResult{
		URL:       parsed.String(),
		Status:    resp.StatusCode,
		Bytes:     len(body),
		Truncated: truncated,
		Preview:   preview,
	})
	if err != nil {
		return Failure("internal", err.Error())
	}
	return Result{OK: true, Code: "ok", Data: payload, Preview: fmt.Sprintf("HTTP %d · %d bytes", resp.StatusCode, len(body)), Bytes: int64(len(body)), Truncated: truncated}
}
