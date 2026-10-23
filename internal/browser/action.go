// Package browser 的受控页面操作：点击/输入/滚动/上传均需经授权链；
// 高风险动作额外标注，由策略层决定是否放行。
// Author: fxbin
package browser

import (
	"fmt"
	"strings"
	"time"
)

// ActionType 是受控操作类型。
type ActionType string

const (
	// ActionClick 点击元素。
	ActionClick ActionType = "click"
	// ActionInput 输入文本。
	ActionInput ActionType = "input"
	// ActionScroll 滚动。
	ActionScroll ActionType = "scroll"
	// ActionUpload 上传文件。
	ActionUpload ActionType = "upload"
)

// RiskLevel 是操作风险等级，供策略层审批。
type RiskLevel string

const (
	// RiskLow 表示普通浏览操作。
	RiskLow RiskLevel = "low"
	// RiskHigh 表示登录/发布/删除等高风险动作。
	RiskHigh RiskLevel = "high"
)

// PageAction 是一次待授权的页面操作。
type PageAction struct {
	ID          string     `json:"id"`
	TabID       string     `json:"tab_id"`
	Selector    string     `json:"selector"`
	Type        ActionType `json:"type"`
	Value       string     `json:"value,omitempty"`
	RunID       string     `json:"run_id,omitempty"`
	Risk        RiskLevel  `json:"risk"`
	RequestedAt time.Time  `json:"requested_at"`
}

// ActionResult 是操作后的可检查事实。
type ActionResult struct {
	ActionID   string    `json:"action_id"`
	OK         bool      `json:"ok"`
	ErrorClass string    `json:"error_class,omitempty"`
	AppliedAt  time.Time `json:"applied_at"`
}

// ClassifyRisk 按动作与值判定风险；敏感动作用 high。
func ClassifyRisk(actionType ActionType, value string) RiskLevel {
	switch actionType {
	case ActionUpload:
		return RiskHigh
	case ActionInput:
		lower := strings.ToLower(value)
		if strings.Contains(lower, "password") || strings.Contains(lower, "token") {
			return RiskHigh
		}
		return RiskLow
	default:
		return RiskLow
	}
}

// ValidatePageAction 校验操作绑定明确目标；不执行。
func ValidatePageAction(action PageAction) error {
	if strings.TrimSpace(action.ID) == "" {
		return fmt.Errorf("action id is required")
	}
	if strings.TrimSpace(action.TabID) == "" {
		return fmt.Errorf("tab_id is required")
	}
	if strings.TrimSpace(action.Selector) == "" {
		return fmt.Errorf("selector is required")
	}
	switch action.Type {
	case ActionClick, ActionInput, ActionScroll, ActionUpload:
	default:
		return fmt.Errorf("unknown action type %q", action.Type)
	}
	if action.Type == ActionInput && action.Value == "" {
		return fmt.Errorf("input requires value")
	}
	return nil
}
