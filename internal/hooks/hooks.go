// hooks.go 定义工作区钩子：事件匹配与有限效果。
// 钩子不得授予权限；tool 类效果只收紧 Policy，动作类仅 queue_followup。
// Author: fxbin
package hooks

import (
	"fmt"
	"path"
	"strings"
)

// Event 是钩子订阅的生命周期事件。
type Event string

const (
	// EventPreTool 工具提议后、执行前；可收紧/放行/追问。
	EventPreTool Event = "pre_tool"
	// EventPostTool 工具结果落地后；仅观察与动作。
	EventPostTool Event = "post_tool"
	// EventRunFinished 运行进入终态后。
	EventRunFinished Event = "run_finished"
	// EventRunFailed 运行失败后。
	EventRunFailed Event = "run_failed"
)

// Effect 是钩子对工具调用的收紧效果（不放宽 Policy.Deny）。
type Effect string

const (
	// EffectAllow 不改变当前 Policy 决策中的允许/追问。
	EffectAllow Effect = "allow"
	// EffectAsk 强制进入审批（仅能把 allow 升为 ask）。
	EffectAsk Effect = "ask"
	// EffectDeny 否决工具调用。
	EffectDeny Effect = "deny"
)

// ActionKind 是钩子动作类型；当前仅允许后续入队。
type ActionKind string

// ActionQueueFollowup 将内容排入持久后续队列。
const ActionQueueFollowup ActionKind = "queue_followup"

// Action 是钩子触发后的有限动作。
type Action struct {
	Kind    ActionKind `json:"kind"`
	Content string     `json:"content,omitempty"`
	Reason  string     `json:"reason,omitempty"`
}

// Rule 是一条钩子规则；Enabled 为 false 时跳过。
type Rule struct {
	ID    string `json:"id"`
	Event Event  `json:"event"`
	// ToolMatch 支持精确名或 `*` 后缀通配；空表示不按工具过滤。
	ToolMatch string   `json:"tool_match,omitempty"`
	Effect    Effect   `json:"effect,omitempty"`
	Actions   []Action `json:"actions,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Enabled   bool     `json:"enabled"`
}

// ToolDecision 是 pre_tool 钩子求值结果。
type ToolDecision struct {
	Effect Effect
	Reason string
	HookID string
}

// ValidateRule 校验规则形状；失败关闭，禁止未知动作。
func ValidateRule(rule Rule) error {
	if strings.TrimSpace(rule.ID) == "" {
		return fmt.Errorf("hook id is required")
	}
	switch rule.Event {
	case EventPreTool, EventPostTool, EventRunFinished, EventRunFailed:
	default:
		return fmt.Errorf("unsupported hook event %q", rule.Event)
	}
	if rule.Event == EventPreTool {
		switch rule.Effect {
		case "", EffectAllow, EffectAsk, EffectDeny:
		default:
			return fmt.Errorf("unsupported pre_tool effect %q", rule.Effect)
		}
	}
	for _, action := range rule.Actions {
		if action.Kind != ActionQueueFollowup {
			return fmt.Errorf("unsupported hook action %q", action.Kind)
		}
		if strings.TrimSpace(action.Content) == "" {
			return fmt.Errorf("hook action content is required")
		}
	}
	return nil
}

// MatchTool 判断工具名是否命中通配（空 ToolMatch 命中全部）。
func (rule Rule) MatchTool(toolName string) bool {
	pattern := strings.TrimSpace(rule.ToolMatch)
	if pattern == "" {
		return true
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(toolName, strings.TrimSuffix(pattern, "*"))
	}
	ok, err := path.Match(pattern, toolName)
	return err == nil && ok
}

// EvaluateTool 在已启用规则中求 pre_tool 效果；Deny 优先，其次 Ask。
// 不会返回比输入 Policy 更松的效果。
func EvaluateTool(rules []Rule, toolName string, policyAllow bool) ToolDecision {
	decision := ToolDecision{Effect: EffectAllow}
	if !policyAllow {
		// Policy 已是 ask/deny 时钩子不得放宽。
		decision.Effect = EffectAsk
		decision.Reason = "policy requires approval"
	}
	for _, rule := range rules {
		if !rule.Enabled || rule.Event != EventPreTool || !rule.MatchTool(toolName) {
			continue
		}
		effect := rule.Effect
		if effect == "" {
			effect = EffectAllow
		}
		switch effect {
		case EffectDeny:
			decision.Effect = EffectDeny
			decision.Reason = firstNonEmpty(rule.Reason, "denied by hook")
			decision.HookID = rule.ID
			return decision
		case EffectAsk:
			if decision.Effect == EffectAllow {
				decision.Effect = EffectAsk
				decision.Reason = firstNonEmpty(rule.Reason, "approval required by hook")
				decision.HookID = rule.ID
			}
		}
	}
	return decision
}

// ActionsFor 返回命中事件的启用规则动作。
func ActionsFor(rules []Rule, event Event, toolName string) []Action {
	out := make([]Action, 0, 2)
	for _, rule := range rules {
		if !rule.Enabled || rule.Event != event {
			continue
		}
		if event == EventPreTool || event == EventPostTool {
			if !rule.MatchTool(toolName) {
				continue
			}
		}
		out = append(out, rule.Actions...)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
