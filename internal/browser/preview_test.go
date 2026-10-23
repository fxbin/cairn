// preview_test 锁定 BRW-05 本地预览禁用桌面特权与反馈边界。
//
// Author: fxbin
package browser

import "testing"

// TestNewPreviewSessionRejectsDesktopBindings 锁定特权 bindings 失败关闭。
func TestNewPreviewSessionRejectsDesktopBindings(t *testing.T) {
	if _, err := NewPreviewSession("p1", "http://127.0.0.1:5173", true); err == nil {
		t.Fatal("桌面 bindings 应拒绝")
	}
	session, err := NewPreviewSession("p1", "http://127.0.0.1:5173", false)
	if err != nil || session.AllowDesktopBindings {
		t.Fatalf("应禁用 bindings: %+v %v", session, err)
	}
	if _, err := NewPreviewSession("", "http://127.0.0.1:5173", false); err == nil {
		t.Fatal("空 ID 应拒绝")
	}
	if _, err := NewPreviewSession("p2", "not-a-url", false); err == nil {
		t.Fatal("非法 URL 应拒绝")
	}
}

// TestAppendFeedbackBounds 锁定反馈必填会话与消息。
func TestAppendFeedbackBounds(t *testing.T) {
	feedback, err := AppendFeedback("p1", "#app", "元素错位", "warn", zeroTime())
	if err != nil || feedback.Message != "元素错位" {
		t.Fatalf("反馈应成功: %+v %v", feedback, err)
	}
	if _, err := AppendFeedback("", "#app", "x", "warn", zeroTime()); err == nil {
		t.Fatal("空会话应拒绝")
	}
	if _, err := AppendFeedback("p1", "#app", " ", "warn", zeroTime()); err == nil {
		t.Fatal("空消息应拒绝")
	}
}
