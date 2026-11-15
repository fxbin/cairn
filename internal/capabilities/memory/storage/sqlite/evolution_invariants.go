// [INPUT]: 依赖库内候选、来源与审计事实。
// [OUTPUT]: 提供演化状态、时间线和冷却一致性校验。
// [POS]: 只读完整性检查不把终态作为损坏豁免。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package sqlite 提供能力内的SQLite持久化叶子。
// Author: fxbin
package sqlite

import (
	"context"

	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

// checkEvolutionInvariants 验证演化候选的结构不变量：审计时间线、状态与
// 审计一致、rejected 与冷却绑定、来源派生版本存在且同工作区，以及非终态
// 候选的来源仍为 active（否则必须已迁移到 invalidated）。到期判定必须使用
// 调用方注入的同一时钟，禁止在此回退到墙钟。
func checkEvolutionInvariants(ctx context.Context, source queryerRows, checkNow time.Time) error {
	return checkEvolutionInvariantsAt(ctx, source, checkNow)
}

// checkEvolutionInvariantsAt 以给定时刻执行演化不变量校验，供测试冻结时钟。
func checkEvolutionInvariantsAt(ctx context.Context, source queryerRows, checkNow time.Time) error {
	candidates, err := listEvolutionCandidatesOn(ctx, source)
	if err != nil {
		return err
	}
	cooldowns, err := listEvolutionCooldowns(ctx, source)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		audit, err := evolutionAuditFor(ctx, source, candidate.ID)
		if err != nil {
			return err
		}
		created, rejected, invalidated, expired := 0, 0, 0, 0
		evaluationStarted, evaluationPassed, evaluationFailed, evaluationInconclusive := 0, 0, 0, 0
		promotedAudits, sandboxActivated, promotionRolledBack := 0, 0, 0
		for _, entry := range audit {
			if entry.CandidateID != candidate.ID || entry.OccurredAt.Before(candidate.CreatedAt) {
				return fmt.Errorf("evolution candidate %s has an invalid audit timeline", candidate.ID)
			}
			switch entry.Action {
			case memory.EvolutionAuditCreated:
				if entry.Reason != "" || !entry.OccurredAt.Equal(candidate.CreatedAt) {
					return fmt.Errorf("evolution candidate %s has an invalid creation audit", candidate.ID)
				}
				created++
			case memory.EvolutionAuditRejected:
				if !memory.ValidEvolutionRejectionReason(entry.Reason) {
					return fmt.Errorf("evolution candidate %s has an invalid rejection audit", candidate.ID)
				}
				rejected++
			case memory.EvolutionAuditInvalidated:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid invalidation audit", candidate.ID)
				}
				invalidated++
			case memory.EvolutionAuditExpired:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid expiry audit", candidate.ID)
				}
				expired++
			case memory.EvolutionAuditEvaluationStarted:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid evaluation start audit", candidate.ID)
				}
				evaluationStarted++
			case memory.EvolutionAuditEvaluatedPassed:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid evaluation pass audit", candidate.ID)
				}
				evaluationPassed++
			case memory.EvolutionAuditEvaluatedFailed:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid evaluation fail audit", candidate.ID)
				}
				evaluationFailed++
			case memory.EvolutionAuditEvaluationInconclusive:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid evaluation inconclusive audit", candidate.ID)
				}
				evaluationInconclusive++
			case memory.EvolutionAuditPromoted:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid promotion audit", candidate.ID)
				}
				promotedAudits++
			case memory.EvolutionAuditSandboxActivated:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid sandbox activation audit", candidate.ID)
				}
				sandboxActivated++
			case memory.EvolutionAuditPromotionRolledBack:
				if !memory.ValidEvolutionRejectionReason(entry.Reason) {
					return fmt.Errorf("evolution candidate %s has an invalid rollback audit", candidate.ID)
				}
				promotionRolledBack++
			default:
				return fmt.Errorf("evolution candidate %s has an unknown audit action", candidate.ID)
			}
		}
		if created != 1 {
			return fmt.Errorf("evolution candidate %s must have exactly one creation audit", candidate.ID)
		}
		sourceVersion, err := evolutionSourceForCandidate(ctx, source, candidate)
		if err != nil {
			return err
		}
		evaluationRow, hasEvaluation, err := lookupEvolutionEvaluationForCandidate(ctx, source, candidate.ID)
		if err != nil {
			return err
		}
		terminalEvaluationAudits := evaluationPassed + evaluationFailed + evaluationInconclusive
		switch candidate.Status {
		case memory.CandidateStateDraft:
			// draft 只允许 created 审计与活跃来源；评测生命周期尚未开始。
			if rejected != 0 || invalidated != 0 || expired != 0 || evaluationStarted != 0 || terminalEvaluationAudits != 0 {
				return fmt.Errorf("draft evolution candidate %s has terminal audit", candidate.ID)
			}
			if sourceVersion.Status != memory.DerivedStatusActive {
				return fmt.Errorf("draft evolution candidate %s has an inactive source version", candidate.ID)
			}
			if !sourceVersion.ExpiresAt.After(checkNow) {
				return fmt.Errorf("draft evolution candidate %s has an expired source version", candidate.ID)
			}
		case memory.CandidateStateReady:
			// ready 表示冻结输入就绪但评测未启动：禁止一切终态与评测审计，
			// 冻结指纹必须随登记写入。
			if rejected != 0 || invalidated != 0 || expired != 0 || evaluationStarted != 0 || terminalEvaluationAudits != 0 {
				return fmt.Errorf("ready evolution candidate %s has terminal or evaluation audit", candidate.ID)
			}
			if sourceVersion.Status != memory.DerivedStatusActive {
				return fmt.Errorf("ready evolution candidate %s has an inactive source version", candidate.ID)
			}
			if !sourceVersion.ExpiresAt.After(checkNow) {
				return fmt.Errorf("ready evolution candidate %s has an expired source version", candidate.ID)
			}
		case memory.CandidateStateEvaluating:
			if evaluationStarted != 1 || terminalEvaluationAudits != 0 || rejected != 0 || invalidated != 0 || expired != 0 {
				return fmt.Errorf("evaluating evolution candidate %s has invalid audit state", candidate.ID)
			}
			if !hasEvaluation || evaluationRow.Status != memory.EvaluationStatusRunning {
				return fmt.Errorf("evaluating evolution candidate %s lacks a running evaluation row", candidate.ID)
			}
			if sourceVersion.Status != memory.DerivedStatusActive {
				return fmt.Errorf("evaluating evolution candidate %s has an inactive source version", candidate.ID)
			}
			if !sourceVersion.ExpiresAt.After(checkNow) {
				return fmt.Errorf("evaluating evolution candidate %s has an expired source version", candidate.ID)
			}
		case memory.CandidateStateEvaluatedPassed, memory.CandidateStateEvaluatedFailed, memory.CandidateStateInconclusive:
			expectedConclusion := 0
			rowStatus := ""
			if candidate.Status == memory.CandidateStateEvaluatedPassed {
				expectedConclusion, rowStatus = evaluationPassed, memory.EvaluationStatusPassed
			} else if candidate.Status == memory.CandidateStateEvaluatedFailed {
				expectedConclusion, rowStatus = evaluationFailed, memory.EvaluationStatusFailed
			} else {
				expectedConclusion, rowStatus = evaluationInconclusive, memory.EvaluationStatusInconclusive
			}
			if evaluationStarted != 1 || expectedConclusion != 1 || terminalEvaluationAudits != 1 || rejected != 0 || invalidated != 0 || expired != 0 || promotedAudits != 0 || sandboxActivated != 0 || promotionRolledBack != 0 {
				return fmt.Errorf("%s evolution candidate %s has invalid evaluation audit state", candidate.Status, candidate.ID)
			}
			if !hasEvaluation || evaluationRow.Status != rowStatus {
				return fmt.Errorf("%s evolution candidate %s lacks a matching evaluation row", candidate.Status, candidate.ID)
			}
		case memory.CandidateStateRejected:
			if evaluationStarted != 0 || terminalEvaluationAudits != 0 || promotedAudits != 0 || sandboxActivated != 0 || promotionRolledBack != 0 {
				return fmt.Errorf("rejected evolution candidate %s carries evaluation or promotion audits", candidate.ID)
			}
			if rejected != 1 || invalidated != 0 || expired != 0 {
				return fmt.Errorf("rejected evolution candidate %s has invalid audit state", candidate.ID)
			}
			until, exists := cooldowns[cooldownKeyOf(candidate)]
			if !exists {
				return fmt.Errorf("rejected evolution candidate %s lacks a cooldown record", candidate.ID)
			}
			var rejectTime time.Time
			for _, entry := range audit {
				if entry.Action == memory.EvolutionAuditRejected {
					rejectTime = entry.OccurredAt
				}
			}
			if rejectTime.IsZero() || until.Before(rejectTime) {
				return fmt.Errorf("rejected evolution candidate %s has an incoherent cooldown deadline", candidate.ID)
			}
		case memory.CandidateStateInvalidated:
			if rejected != 0 || expired != 0 || promotedAudits != 0 || sandboxActivated != 0 || promotionRolledBack != 0 {
				return fmt.Errorf("invalidated evolution candidate %s has invalid audit state", candidate.ID)
			}
			if hasEvaluation && evaluationRow.Status != memory.EvaluationStatusInvalid {
				return fmt.Errorf("invalidated evolution candidate %s carries a non-invalid evaluation row", candidate.ID)
			}
			if hasEvaluation && evaluationRow.Status == memory.EvaluationStatusInvalid && evaluationStarted == 1 && invalidated == 1 && evaluationPassed+evaluationFailed+evaluationInconclusive == 0 {
				// 评测安全作废路径：隔离或证据合同破坏令结论为 invalid，
				// 候选随之 invalidated；来源活性不再是必要条件。
			} else {
				// 来源失效驱动的受限作废：来源必须已经不处于可用状态。
				if invalidated != 1 || evaluationStarted != 0 || terminalEvaluationAudits != 0 {
					return fmt.Errorf("invalidated evolution candidate %s has invalid audit state", candidate.ID)
				}
				if sourceVersion.Status == memory.DerivedStatusActive && sourceVersion.ExpiresAt.After(checkNow) {
					return fmt.Errorf("invalidated evolution candidate %s has an active source version", candidate.ID)
				}
			}
		case memory.CandidateStateExpired:
			if rejected != 0 || invalidated != 0 || expired != 1 || evaluationStarted != 0 || terminalEvaluationAudits != 0 || promotedAudits != 0 || sandboxActivated != 0 || promotionRolledBack != 0 {
				return fmt.Errorf("expired evolution candidate %s has invalid audit state", candidate.ID)
			}
			if hasEvaluation {
				return fmt.Errorf("expired evolution candidate %s carries an evaluation row", candidate.ID)
			}
			for _, entry := range audit {
				if entry.Action == memory.EvolutionAuditExpired && entry.OccurredAt.Before(candidate.ExpiresAt) {
					return fmt.Errorf("expired evolution candidate %s has an early expiry audit", candidate.ID)
				}
			}
		case memory.CandidateStatePromoted:
			if evaluationStarted != 1 || evaluationPassed != 1 || terminalEvaluationAudits != 1 || promotedAudits != 1 || rejected != 0 || invalidated != 0 || expired != 0 || sandboxActivated > 1 || promotionRolledBack > 1 || promotionRolledBack > sandboxActivated {
				return fmt.Errorf("promoted evolution candidate %s has invalid audit state", candidate.ID)
			}
			if !hasEvaluation || evaluationRow.Status != memory.EvaluationStatusPassed {
				return fmt.Errorf("promoted evolution candidate %s lacks a passed evaluation row", candidate.ID)
			}
			promotionRow, hasPromotion, err := lookupEvolutionPromotionForCandidate(ctx, source, candidate.ID)
			if err != nil {
				return err
			}
			if !hasPromotion {
				return fmt.Errorf("promoted evolution candidate %s lacks a promotion ledger row", candidate.ID)
			}
			expectedStatus := memory.PromotionStatusPromotedInactive
			if sandboxActivated == 1 {
				expectedStatus = memory.PromotionStatusSandboxActivated
			}
			if promotionRolledBack == 1 {
				expectedStatus = memory.PromotionStatusRevoked
			}
			if promotionRow.Status != expectedStatus {
				return fmt.Errorf("promoted evolution candidate %s has a promotion status that diverges from its audit history", candidate.ID)
			}
		default:
			return fmt.Errorf("evolution candidate %s has unknown or corrupt state %q", candidate.ID, candidate.Status)
		}
	}
	return nil
}
