// retry_policy.go 定义 MCP 工具失败后的重试策略：只允许只读或幂等工具
// 自动重试；副作用未知或非幂等时失败关闭，不隐式重放。
// Author: fxbin
package mcpclient

import "strings"

// RetryPolicy 决定一次 MCP 失败是否可自动重试。
type RetryPolicy struct {
	// IdempotentNames 是已知幂等工具名（可配置扩展）。
	IdempotentNames map[string]bool
}

// NewRetryPolicy 创建默认策略；read-only 语义工具默认可重试。
func NewRetryPolicy(extra ...string) *RetryPolicy {
	names := map[string]bool{
		"mcp_discover": true,
		"tool_search":  true,
		"web_fetch":    false, // 抓取非幂等，不自动重试
		"web_search":   true,  // 查询幂等
	}
	for _, name := range extra {
		names[strings.TrimSpace(name)] = true
	}
	return &RetryPolicy{IdempotentNames: names}
}

// AllowRetry 判断工具失败后是否可自动重试。
// 未登记的工具默认不重试（失败关闭）。
func (policy *RetryPolicy) AllowRetry(toolName string) bool {
	if policy == nil {
		return false
	}
	return policy.IdempotentNames[toolName]
}
