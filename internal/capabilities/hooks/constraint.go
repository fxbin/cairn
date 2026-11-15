// [INPUT]: 已声明的钩子规则与共享授权效果。
// [OUTPUT]: 提供显式Constraint构造与pre_tool收紧候选。
// [POS]: 钩子授权能力；规则只读快照不执行工具或授予权限。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package hooks 提供可选钩子规则、匹配与受限动作。
// Author: fxbin
package hooks

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/contract"
)

// Constraint 保存已校验的规则快照；外部修改配置不能改变本次求值。
type Constraint struct{ rules []Rule }

// NewConstraint 校验全部规则并复制动作；空规则不创建能力实例。
func NewConstraint(rules []Rule) (*Constraint, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	copied := make([]Rule, len(rules))
	for i, rule := range rules {
		if err := ValidateRule(rule); err != nil {
			return nil, fmt.Errorf("hook rule %d: %w", i, err)
		}
		copied[i] = rule
		copied[i].Actions = append([]Action(nil), rule.Actions...)
	}
	return &Constraint{rules: copied}, nil
}

// ConstrainTool 保留既有deny优先和ask只提升allow语义；不传递审批身份。
func (constraint *Constraint) ConstrainTool(name string, current contract.AuthorizationEffect) (contract.AuthorizationConstraint, error) {
	if constraint == nil {
		return contract.AuthorizationConstraint{}, errors.New("hook constraint is required")
	}
	evaluated := EvaluateTool(constraint.rules, name, current == contract.AuthorizationAllow)
	switch evaluated.Effect {
	case EffectDeny:
		return contract.AuthorizationConstraint{Effect: contract.AuthorizationDeny, RuleID: "hook_deny", Summary: firstNonEmpty(evaluated.Reason, "denied by workspace hook")}, nil
	case EffectAsk:
		if current == contract.AuthorizationAllow {
			return contract.AuthorizationConstraint{Effect: contract.AuthorizationAsk, RuleID: "hook_ask", Summary: firstNonEmpty(evaluated.Reason, "approval required by workspace hook")}, nil
		}
	}
	return contract.AuthorizationConstraint{}, nil
}
