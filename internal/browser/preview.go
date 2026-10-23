// Package browser 的本地应用预览：绑定开发服务地址与反馈通道；
// 不可信网页不得访问桌面特权 bindings。
// Author: fxbin
package browser

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// PreviewSession 是本地开发服务预览会话。
type PreviewSession struct {
	ID      string `json:"id"`
	BaseURL string `json:"base_url"`
	RunID   string `json:"run_id,omitempty"`
	// AllowDesktopBindings 必须为 false：不可信网页禁止特权 bindings。
	AllowDesktopBindings bool      `json:"allow_desktop_bindings"`
	StartedAt            time.Time `json:"started_at"`
}

// Feedback 是元素反馈/错误定位条目。
type Feedback struct {
	SessionID string    `json:"session_id"`
	Selector  string    `json:"selector,omitempty"`
	Message   string    `json:"message"`
	Severity  string    `json:"severity"`
	At        time.Time `json:"at"`
}

// NewPreviewSession 校验并创建预览；拒绝启用桌面特权。
func NewPreviewSession(id, baseURL string, allowDesktopBindings bool) (PreviewSession, error) {
	if strings.TrimSpace(id) == "" {
		return PreviewSession{}, fmt.Errorf("session id is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return PreviewSession{}, fmt.Errorf("valid base_url is required")
	}
	if allowDesktopBindings {
		return PreviewSession{}, fmt.Errorf("desktop bindings must be disabled for preview")
	}
	return PreviewSession{
		ID:                   id,
		BaseURL:              baseURL,
		AllowDesktopBindings: false,
		StartedAt:            time.Now().UTC(),
	}, nil
}

// AppendFeedback 记录一条反馈；空消息失败关闭。
func AppendFeedback(sessionID, selector, message, severity string, at time.Time) (Feedback, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Feedback{}, fmt.Errorf("session id is required")
	}
	if strings.TrimSpace(message) == "" {
		return Feedback{}, fmt.Errorf("message is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return Feedback{SessionID: sessionID, Selector: selector, Message: message, Severity: severity, At: at.UTC()}, nil
}
