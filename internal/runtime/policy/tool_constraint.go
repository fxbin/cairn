// [INPUT]: 共享授权收紧候选与已求出的基础决定。
// [OUTPUT]: 提供ToolConstraint端口和强制只收紧的求值。
// [POS]: 基础门禁的扩展边界；不识别具体能力规则。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package policy 实现副作用授权与不可放宽的约束。
// Author: fxbin
package policy

import (
	"errors"

	"github.com/fxbin/cairn/internal/contract"
)

// ToolConstraint 只观察工具名和当前效果，不接收审批身份或执行能力。
// 空效果表示无改动；非空效果仍须由Gate核对只收紧。
type ToolConstraint interface {
	ConstrainTool(string, contract.AuthorizationEffect) (contract.AuthorizationConstraint, error)
}

// applyToolConstraints 按装配顺序收紧候选；缺失、未知效果和放宽尝试均拒绝。
func (gate *Gate) applyToolConstraints(toolName string, decision Decision) (Decision, error) {
	for _, constraint := range gate.ToolConstraints {
		if constraint == nil {
			return decision, errors.New("tool constraint is required")
		}
		proposed, err := constraint.ConstrainTool(toolName, decision.Effect)
		if err != nil {
			return decision, err
		}
		if proposed.Effect == "" {
			continue
		}
		if !constraintTightens(decision.Effect, proposed.Effect) {
			return decision, errors.New("tool constraint must preserve or tighten authorization")
		}
		decision.Effect = proposed.Effect
		if proposed.RuleID != "" {
			decision.RuleID = proposed.RuleID
		}
		if proposed.Summary != "" {
			decision.Summary = proposed.Summary
		}
	}
	return decision, nil
}

// constraintTightens 只接受闭合词表中相同或更严格的授权效果。
func constraintTightens(current, next Effect) bool {
	switch current {
	case Allow:
		return next == Allow || next == Ask || next == Deny
	case Ask:
		return next == Ask || next == Deny
	case Deny:
		return next == Deny
	default:
		return false
	}
}
