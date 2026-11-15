// [INPUT]: YAML钩子形状、Hooks能力与Core动作合同。
// [OUTPUT]: 提供纯配置校验和冻结动作投影。
// [POS]: 应用执行与Workflow入口先校验；Boot显式创建能力。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package app 连接显式能力与运行用例。
// Author: fxbin
package app

import (
	"fmt"

	"github.com/fxbin/cairn/internal/capabilities/hooks"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/core/agent"
)

// validateHookConfiguration 在执行入口做纯校验，不创建观测、存储或Provider。
// 应急终止不激活钩子，因此保持原有恢复控制面，不消费此校验。
func validateHookConfiguration(configuration config.Config) error {
	_, err := hooks.NewConstraint(hooksFromConfig(configuration.Hooks))
	if err != nil {
		return fmt.Errorf("hooks: %w", err)
	}
	return nil
}

// agentHookRules 把已校验的运行时钩子规则投影为 Loop 的最小数据合同；
// 只保留事件、工具匹配、动作与启用位，顺序与来源规则一致。
func agentHookRules(rules []hooks.Rule) []agent.HookRule {
	if len(rules) == 0 {
		return nil
	}
	converted := make([]agent.HookRule, 0, len(rules))
	for _, rule := range rules {
		item := agent.HookRule{
			Event:     agent.HookEvent(rule.Event),
			ToolMatch: rule.ToolMatch,
			Enabled:   rule.Enabled,
		}
		for _, action := range rule.Actions {
			item.Actions = append(item.Actions, agent.HookAction{
				Kind:    agent.HookActionKind(action.Kind),
				Content: action.Content,
				Reason:  action.Reason,
			})
		}
		converted = append(converted, item)
	}
	return converted
}

// hooksFromConfig 只转换 YAML 钩子形状；能力构造器统一校验，不丢弃规则。
func hooksFromConfig(configured []config.HookConfig) []hooks.Rule {
	if len(configured) == 0 {
		return nil
	}
	rules := make([]hooks.Rule, 0, len(configured))
	for _, item := range configured {
		rule := hooks.Rule{
			ID:        item.ID,
			Event:     hooks.Event(item.Event),
			ToolMatch: item.ToolMatch,
			Effect:    hooks.Effect(item.Effect),
			Reason:    item.Reason,
			Enabled:   item.Enabled,
		}
		for _, action := range item.Actions {
			rule.Actions = append(rule.Actions, hooks.Action{
				Kind:    hooks.ActionKind(action.Kind),
				Content: action.Content,
			})
		}
		rules = append(rules, rule)
	}
	return rules
}
