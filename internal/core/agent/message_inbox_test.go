// message_inbox_test 锁定消息投递、Drain 与上下文格式化。
//
// Author: fxbin
package agent

import "testing"

// TestMessageInboxDeliverDrain 锁定投递与清空。
func TestMessageInboxDeliverDrain(t *testing.T) {
	inbox := NewMessageInbox(0)
	inbox.Deliver(InboxMessage{From: "run-p", To: "run-c", Content: "继续"})
	messages := inbox.Drain()
	if len(messages) != 1 || messages[0].Content != "继续" {
		t.Fatalf("消息不符: %+v", messages)
	}
	if len(inbox.Drain()) != 0 {
		t.Fatal("Drain 后应为空")
	}
}

// TestFormatForContext 锁定格式化包含来源与免责声明。
func TestFormatForContext(t *testing.T) {
	if FormatForContext(nil) != "" {
		t.Fatal("空消息应空串")
	}
	text := FormatForContext([]InboxMessage{{From: "run-p", Content: "继续"}})
	if !contains(text, "run-p") || !contains(text, "不改变授权") {
		t.Fatalf("格式化缺失: %s", text)
	}
}

func contains(text, sub string) bool {
	return len(text) >= len(sub) && (text == sub || len(sub) == 0 || (len(text) > 0 && (stringContains(text, sub))))
}

func stringContains(text, sub string) bool {
	for i := 0; i+len(sub) <= len(text); i++ {
		if text[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
