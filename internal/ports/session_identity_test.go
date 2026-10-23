// session_identity_test 锁定会话身份头映射与空值省略。
//
// Author: fxbin
package ports

import "testing"

// TestSessionIdentityHeaders 锁定非空字段映射为请求头。
func TestSessionIdentityHeaders(t *testing.T) {
	identity := SessionIdentity{
		SessionID:       "sess-1",
		ParentSessionID: "sess-0",
		RunID:           "run-1",
		ParentRunID:     "run-0",
	}
	headers := identity.HeaderNames()
	if headers["X-Cairn-Session-Id"] != "sess-1" {
		t.Fatalf("会话头不符: %v", headers)
	}
	if headers["X-Cairn-Parent-Session-Id"] != "sess-0" {
		t.Fatalf("父会话头不符: %v", headers)
	}
	if headers["X-Cairn-Run-Id"] != "run-1" || headers["X-Cairn-Parent-Run-Id"] != "run-0" {
		t.Fatalf("运行头不符: %v", headers)
	}
}

// TestSessionIdentityEmpty 空身份不产生头且 IsZero 为真。
func TestSessionIdentityEmpty(t *testing.T) {
	var identity SessionIdentity
	if !identity.IsZero() {
		t.Fatal("零值应为 IsZero")
	}
	if headers := identity.HeaderNames(); len(headers) != 0 {
		t.Fatalf("零值不应有头: %v", headers)
	}
}
