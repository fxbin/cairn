// retry_policy_test 锁定 MCP 幂等重试策略边界。
//
// Author: fxbin
package mcpclient

import "testing"

// TestRetryPolicyAllowsIdempotentOnly 锁定只读/幂等工具可重试。
func TestRetryPolicyAllowsIdempotentOnly(t *testing.T) {
	policy := NewRetryPolicy("custom_query")
	if !policy.AllowRetry("mcp_discover") {
		t.Fatal("discover 应可重试")
	}
	if !policy.AllowRetry("tool_search") {
		t.Fatal("search 应可重试")
	}
	if !policy.AllowRetry("custom_query") {
		t.Fatal("显式登记应可重试")
	}
	if policy.AllowRetry("web_fetch") {
		t.Fatal("fetch 不应自动重试")
	}
	if policy.AllowRetry("write_file") {
		t.Fatal("未登记工具不应重试")
	}
	var nilPolicy *RetryPolicy
	if nilPolicy.AllowRetry("tool_search") {
		t.Fatal("nil 策略应失败关闭")
	}
}
