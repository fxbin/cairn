// hook_contract.go 定义 Agent Loop 消费的最小钩子数据合同。
// 钩子只在观察点产生受限动作；效果收紧由 Policy 侧规则求值，不在本合同内。
// Author: fxbin
package agent

import (
	"path"
	"strings"
)

// HookEvent 是 Loop 分发钩子动作时使用的生命周期事件词表；取值与工作区钩子配置一致。
type HookEvent string

const (
	// HookEventPreTool 工具提议后、执行前；仅用于动作过滤与工具匹配判定。
	HookEventPreTool HookEvent = "pre_tool"
	// HookEventPostTool 工具结果落地后；动作随操作终态事件持久化。
	HookEventPostTool HookEvent = "post_tool"
	// HookEventUserPromptSubmit 用户输入进入轮次前；仅观察与动作。
	HookEventUserPromptSubmit HookEvent = "user_prompt_submit"
	// HookEventTurnStart 轮次打开后、模型请求前；仅观察与动作。
	HookEventTurnStart HookEvent = "turn_start"
	// HookEventPreCompact 上下文压缩前；仅观察与动作。
	HookEventPreCompact HookEvent = "pre_compact"
	// HookEventNotification 审批等待、预算告警等通知点；仅观察与动作。
	HookEventNotification HookEvent = "notification"
)

// HookActionKind 是钩子动作类型词表；当前仅允许后续入队。
type HookActionKind string

// HookActionQueueFollowup 将内容排入持久后续队列。
const HookActionQueueFollowup HookActionKind = "queue_followup"

// HookAction 是钩子触发后的有限动作；JSON 字段与既有事件载荷保持一致。
type HookAction struct {
	Kind    HookActionKind `json:"kind"`
	Content string         `json:"content,omitempty"`
	Reason  string         `json:"reason,omitempty"`
}

// HookRule 是 Loop 分发动作所需的最小规则视图：只包含事件、工具匹配、动作与启用位。
// pre_tool 效果与规则形状校验仍由 Policy 侧持有，避免 Core 扩权。
type HookRule struct {
	Event     HookEvent
	ToolMatch string
	Actions   []HookAction
	Enabled   bool
}

// MatchTool 判断工具名是否命中规则：空匹配与 `*` 命中全部，`*` 后缀按前缀匹配，
// 其余模式沿用 path.Match 语义。
func (rule HookRule) MatchTool(toolName string) bool {
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

// hookActionsFor 按规则顺序拼接命中事件的启用动作；pre_tool/post_tool 额外要求工具匹配。
// 求值顺序与工作区钩子实现一致：先到先得，不去重、不排序。
func hookActionsFor(rules []HookRule, event HookEvent, toolName string) []HookAction {
	out := make([]HookAction, 0, 2)
	for _, rule := range rules {
		if !rule.Enabled || rule.Event != event {
			continue
		}
		if event == HookEventPreTool || event == HookEventPostTool {
			if !rule.MatchTool(toolName) {
				continue
			}
		}
		out = append(out, rule.Actions...)
	}
	return out
}
