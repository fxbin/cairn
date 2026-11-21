// [INPUT]: 依赖已确认的 Finding、返工预算与冻结策略的授权返工起点。
// [OUTPUT]: 经资格检查后复用既有有界返工入口，返回可审计的返工决定。
// [POS]: 不重建重试状态机；所有返工走 workflow.RetryStep 的冻结授权。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接确认 Finding 与有界自动返工。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
)

// ReworkDecision 是一次返工资格决定。只有 Eligible 为真且给了理由时才应
// 调用 RetryStep；其余情况保留原因供人工介入与审计。
type ReworkDecision struct {
	Eligible          bool
	Reason            string
	BlockingFindings  []string
	RemainingAttempts int
}

// EvaluateRework 按强制停止条件判断确认的 Finding 是否允许自动返工。
// 它只做决策，不触发副作用；预算、权限与未决副作用的最终执行仍由 RetryStep
// 与既有授权链强制。
func EvaluateRework(blockingFindings []string, remainingAttempts int, unresolvedSideEffects bool, authorizedTargets []string, scopeExceeded bool) ReworkDecision {
	decision := ReworkDecision{BlockingFindings: append([]string(nil), blockingFindings...), RemainingAttempts: remainingAttempts}
	switch {
	case len(blockingFindings) == 0:
		decision.Reason = "no confirmed blocking finding"
	case scopeExceeded:
		decision.Reason = "finding exceeds the authorized rework scope"
	case unresolvedSideEffects:
		decision.Reason = "unresolved side effects must be reconciled before rework"
	case remainingAttempts <= 0:
		decision.Reason = "rework attempt budget exhausted"
	case len(authorizedTargets) == 0:
		decision.Reason = "no rework target authorized by the frozen plan"
	default:
		decision.Eligible = true
		decision.Reason = "bounded rework authorized for confirmed findings: " + strings.Join(blockingFindings, ", ")
	}
	return decision
}

// ApplyRework 在资格通过后复用既有 RetryStep 发起一次有界返工。它不绕过
// RetryStep 的冻结策略校验与尝试预算；资格不通过时返回决定但不触发返工。
func (service Service) ApplyRework(ctx context.Context, decision ReworkDecision, options WorkflowRetryOptions) (WorkflowOutcome, ReworkDecision, error) {
	if !decision.Eligible {
		return WorkflowOutcome{}, decision, nil
	}
	if strings.TrimSpace(options.Reason) == "" {
		options.Reason = decision.Reason
	}
	outcome, err := service.RetryStep(ctx, options)
	if err != nil {
		decision.Eligible = false
		decision.Reason = fmt.Sprintf("rework was not authorized: %v", err)
		return WorkflowOutcome{}, decision, err
	}
	decision.RemainingAttempts--
	return outcome, decision, nil
}

// reworkReasonFromFindings 构造可审计的返工原因，绑定确认的发现身份。
func reworkReasonFromFindings(findings []contract.Finding) (string, error) {
	confirmed := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.Status == contract.FindingConfirmed && finding.IsBlocking() {
			confirmed = append(confirmed, finding.ID)
		}
	}
	if len(confirmed) == 0 {
		return "", errors.New("no confirmed blocking finding to drive rework")
	}
	return "自动返工：" + strings.Join(confirmed, ", "), nil
}
