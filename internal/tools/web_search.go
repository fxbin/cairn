// web_search.go 实受限网页搜索工具：仅 http/https 查询接口，限制结果数与
// 超时；网络副作用归类为 Process，需经 Policy 与审批。
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

// WebSearch 限制约束单次搜索的结果数、响应字节与超时。
const (
	WebSearchMaxResults   = 8
	WebSearchMaxBytes     = 128 * 1024
	WebSearchTimeout      = 15 * time.Second
	WebSearchPreviewBytes = 2048
)

// WebSearch 是受限网页搜索工具，查询发往显式 endpoint。
type WebSearch struct {
	Client   *http.Client
	Endpoint string
}

// Spec 返回工具声明。
func (WebSearch) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "web_search",
		Description: "通过受配置的搜索端点查询网页摘要（有限条数与超时）。需要 query。",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":8}}}`),
	}
}

// SideEffect 声明网络访问属于进程级副作用。
func (WebSearch) SideEffect() SideEffectClass { return SideEffectProcess }

// WebSearchResult 是搜索结果载荷。
type WebSearchResult struct {
	Query     string   `json:"query"`
	Count     int      `json:"count"`
	Titles    []string `json:"titles"`
	Truncated bool     `json:"truncated"`
	Preview   string   `json:"preview"`
}

// Execute 校验查询与端点后发起 GET；失败关闭。
func (tool WebSearch) Execute(ctx context.Context, raw json.RawMessage) Result {
	var request struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return Failure("invalid_arguments", "web_search 参数解析失败")
	}
	query := strings.TrimSpace(request.Query)
	if query == "" {
		return Failure("invalid_arguments", "query 不能为空")
	}
	limit := request.Limit
	if limit <= 0 || limit > WebSearchMaxResults {
		limit = WebSearchMaxResults
	}
	endpoint := strings.TrimSpace(tool.Endpoint)
	if endpoint == "" {
		return Failure("not_configured", "未配置搜索端点")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return Failure("invalid_url", "搜索端点必须是 http/https URL")
	}
	client := tool.Client
	if client == nil {
		client = &http.Client{Timeout: WebSearchTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	target := parsed.Query()
	target.Set("q", query)
	target.Set("limit", fmt.Sprint(limit))
	full := parsed.String()
	if parsed.RawQuery == "" {
		full = parsed.Scheme + "://" + parsed.Host + parsed.Path + "?" + target.Encode()
	} else {
		parsed.RawQuery = target.Encode()
		full = parsed.String()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return Failure("internal", err.Error())
	}
	resp, err := client.Do(req)
	if err != nil {
		return Failure("search_failed", err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, WebSearchMaxBytes+1))
	if err != nil {
		return Failure("search_failed", err.Error())
	}
	truncated := len(body) > WebSearchMaxBytes
	if truncated {
		body = body[:WebSearchMaxBytes]
	}
	preview := string(body)
	if len(preview) > WebSearchPreviewBytes {
		preview = preview[:WebSearchPreviewBytes] + "…"
	}
	payload, err := json.Marshal(WebSearchResult{
		Query:     query,
		Count:     limit,
		Titles:    []string{},
		Truncated: truncated,
		Preview:   preview,
	})
	if err != nil {
		return Failure("internal", err.Error())
	}
	return Result{OK: true, Code: "ok", Data: payload, Preview: fmt.Sprintf("HTTP %d · %d bytes", resp.StatusCode, len(body)), Bytes: int64(len(body)), Truncated: truncated}
}
