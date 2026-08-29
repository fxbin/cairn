// 本文件持久化人工晋升决策、不可变版本与 sandbox 激活。三者必须分阶段
// 提交：创建决策不会改变任何激活指针；激活与回退各自拥有独立事务和审计。
//
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

// PromoteCandidate 在单一事务内写入人工决策和 promoted_inactive 版本。它在
// 提交前重验来源、候选、评测、审批元组和预定回退目标，绝不写入激活指针。
func (store *Store) PromoteCandidate(ctx context.Context, promotion memory.Promotion, approvalFingerprint string, now time.Time) (memory.Promotion, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	promotion.CreatedAt = stripMonotonic(now)
	promotion.Status = memory.PromotionStatusPromotedInactive
	promotion.RevokedAt = time.Time{}
	promotion.ApprovalFingerprint = approvalFingerprint
	if promotion.DecisionID == "" {
		decisionID, err := memory.PromotionDecisionID(promotion)
		if err != nil {
			return memory.Promotion{}, err
		}
		promotion.DecisionID = decisionID
	}
	if err := promotion.Validate(); err != nil {
		return memory.Promotion{}, fmt.Errorf("validate promotion: %w", err)
	}
	expectedApproval, err := memory.PromotionApprovalFingerprint(promotion)
	if err != nil {
		return memory.Promotion{}, err
	}
	if approvalFingerprint != expectedApproval {
		return memory.Promotion{}, fmt.Errorf("%w: promotion approval fingerprint mismatch; expected %s", memory.ErrConflict, expectedApproval)
	}
	expectedDecisionID, err := memory.PromotionDecisionID(promotion)
	if err != nil || expectedDecisionID != promotion.DecisionID {
		return memory.Promotion{}, fmt.Errorf("%w: promotion decision identity is not derivable", memory.ErrConflict)
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Promotion{}, fmt.Errorf("start promotion transaction: %w", err)
	}
	defer transaction.Rollback()

	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, promotion.CandidateID)
	if err != nil || !found {
		return memory.Promotion{}, fmt.Errorf("load promotion candidate: %w", err)
	}
	if candidate.Status != memory.CandidateStateEvaluatedPassed || candidate.TargetKind != promotion.TargetKind || candidate.VariantFingerprint != promotion.VariantFingerprint || candidate.VariantSpecSHA256 != promotion.VariantSpecSHA256 || candidate.BaselineFingerprint != promotion.BaselineFingerprint {
		return memory.Promotion{}, fmt.Errorf("%w: candidate facts do not permit promotion", memory.ErrConflict)
	}
	if err := requireEvolutionSourceActive(ctx, transaction, candidate, now); err != nil {
		return memory.Promotion{}, err
	}
	evaluation, found, err := lookupEvolutionEvaluation(ctx, transaction, promotion.EvaluationID)
	if err != nil || !found {
		return memory.Promotion{}, fmt.Errorf("load promotion evaluation: %w", err)
	}
	if evaluation.CandidateID != candidate.ID || evaluation.Status != memory.EvaluationStatusPassed || evaluation.EvalSetFingerprint != promotion.EvalSetFingerprint || evaluation.BaselineConfigFingerprint != promotion.BaselineFingerprint || evaluation.SuiteResultDigest != promotion.SuiteResultDigest || evaluation.ComparatorVersion != promotion.ComparatorVersion {
		return memory.Promotion{}, fmt.Errorf("%w: evaluation facts do not permit promotion", memory.ErrConflict)
	}
	currentMax := 0
	if err := transaction.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM evolution_promotions WHERE target_kind = ?", promotion.TargetKind).Scan(&currentMax); err != nil {
		return memory.Promotion{}, fmt.Errorf("read promotion version ceiling: %w", err)
	}
	if promotion.Version != currentMax+1 {
		return memory.Promotion{}, fmt.Errorf("%w: promotion version %d is not the next version %d", memory.ErrConflict, promotion.Version, currentMax+1)
	}
	expectedID, err := memory.PromotionID(promotion.TargetKind, promotion.Version)
	if err != nil || promotion.ID != expectedID {
		return memory.Promotion{}, fmt.Errorf("%w: promotion identity is not derivable", memory.ErrConflict)
	}
	activation, activationFound, err := lookupEvolutionActivation(ctx, transaction, promotion.TargetKind)
	if err != nil {
		return memory.Promotion{}, err
	}
	if activationFound && promotion.RollbackTargetID != activation.PromotionID {
		return memory.Promotion{}, fmt.Errorf("%w: decision rollback target must equal the current sandbox version", memory.ErrConflict)
	}
	if !activationFound && promotion.RollbackTargetID != "" {
		return memory.Promotion{}, fmt.Errorf("%w: decision rollback target must be empty while sandbox uses the baseline", memory.ErrConflict)
	}
	if promotion.RollbackTargetID != "" {
		target, found, err := lookupEvolutionPromotion(ctx, transaction, promotion.RollbackTargetID)
		if err != nil || !found || target.TargetKind != promotion.TargetKind || target.Status != memory.PromotionStatusSandboxActivated {
			return memory.Promotion{}, fmt.Errorf("%w: decision rollback target is not an activated version", memory.ErrConflict)
		}
	}
	variantJSON, err := json.Marshal(promotion.Variant)
	if err != nil {
		return memory.Promotion{}, fmt.Errorf("encode promotion variant: %w", err)
	}
	metricsJSON, err := json.Marshal(promotion.Metrics)
	if err != nil {
		return memory.Promotion{}, fmt.Errorf("encode promotion metrics: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_promotion_decisions(
		decision_id, schema_version, candidate_id, evaluation_id, target_kind,
		variant_fingerprint, variant_spec_sha256, baseline_fingerprint, eval_set_fingerprint,
		suite_result_digest, comparator_version, conclusion_trigger, metrics_json,
		rollback_target_promotion_id, approval_fingerprint, approval_nonce, approver, decided_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		promotion.DecisionID, promotion.SchemaVersion, promotion.CandidateID, promotion.EvaluationID, promotion.TargetKind,
		promotion.VariantFingerprint, promotion.VariantSpecSHA256, promotion.BaselineFingerprint, promotion.EvalSetFingerprint,
		promotion.SuiteResultDigest, promotion.ComparatorVersion, promotion.ConclusionTrigger, string(metricsJSON),
		nullString(promotion.RollbackTargetID), promotion.ApprovalFingerprint, promotion.ApprovalNonce, promotion.Approver, promotion.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return memory.Promotion{}, fmt.Errorf("insert promotion decision: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_promotions(
		promotion_id, schema_version, decision_id, target_kind, version, variant_json, status, created_at, revoked_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`, promotion.ID, promotion.SchemaVersion, promotion.DecisionID,
		promotion.TargetKind, promotion.Version, string(variantJSON), promotion.Status, promotion.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return memory.Promotion{}, fmt.Errorf("insert evolution version: %w", err)
	}
	if err := transitionEvolutionCandidate(ctx, transaction, candidate.ID, memory.CandidateStatePromoted); err != nil {
		return memory.Promotion{}, err
	}
	if err := insertEvolutionAudit(ctx, transaction, candidate.ID, memory.EvolutionAuditPromoted, "", promotion.CreatedAt); err != nil {
		return memory.Promotion{}, err
	}
	result, found, err := lookupEvolutionPromotion(ctx, transaction, promotion.ID)
	if err != nil || !found || !evolutionPromotionRowsMatch(result, promotion) {
		return memory.Promotion{}, errors.New("promotion decision/version read-back verification failed")
	}
	if err := transaction.Commit(); err != nil {
		return memory.Promotion{}, fmt.Errorf("commit promotion decision: %w", err)
	}
	return result, nil
}

// ActivatePromotion 把已批准的 inactive 版本显式激活到唯一 sandbox 环境。预定
// rollback target 必须仍等于当前指针，避免延期激活时绕过审批上下文。
func (store *Store) ActivatePromotion(ctx context.Context, targetKind, promotionID string, now time.Time) (memory.Promotion, memory.Activation, error) {
	if targetKind != memory.TargetKindContextCompactionParams || promotionID == "" {
		return memory.Promotion{}, memory.Activation{}, errors.New("sandbox activation requires target kind and version id")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("start sandbox activation transaction: %w", err)
	}
	defer transaction.Rollback()
	promotion, found, err := lookupEvolutionPromotion(ctx, transaction, promotionID)
	if err != nil || !found || promotion.TargetKind != targetKind || promotion.Status != memory.PromotionStatusPromotedInactive {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: version is not eligible for sandbox activation", memory.ErrConflict)
	}
	current, currentFound, err := lookupEvolutionActivation(ctx, transaction, targetKind)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	if currentFound && promotion.RollbackTargetID != current.PromotionID {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: activation context diverges from the approved rollback target", memory.ErrConflict)
	}
	if !currentFound && promotion.RollbackTargetID != "" {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: activation requires baseline but decision carries a rollback target", memory.ErrConflict)
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE evolution_promotions SET status = ? WHERE promotion_id = ?", memory.PromotionStatusSandboxActivated, promotion.ID); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("mark sandbox activation: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_activations(environment, target_kind, promotion_id, previous_promotion_id, activated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(environment, target_kind) DO UPDATE SET promotion_id = excluded.promotion_id, previous_promotion_id = excluded.previous_promotion_id, activated_at = excluded.activated_at`,
		memory.SandboxEnvironment, targetKind, promotion.ID, nullString(promotion.RollbackTargetID), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("advance sandbox activation: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, promotion.CandidateID, memory.EvolutionAuditSandboxActivated, "", now); err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	promotion.Status = memory.PromotionStatusSandboxActivated
	activation, found, err := lookupEvolutionActivation(ctx, transaction, targetKind)
	if err != nil || !found || activation.PromotionID != promotion.ID || activation.PreviousPromotionID != promotion.RollbackTargetID {
		return memory.Promotion{}, memory.Activation{}, errors.New("sandbox activation read-back verification failed")
	}
	if err := transaction.Commit(); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("commit sandbox activation: %w", err)
	}
	return promotion, activation, nil
}

// RollbackPromotion 只回退当前 sandbox 激活版本，理由必须非空且属于封闭词表。
// 它恢复决策固定的 previous version 或产品基线，并将被回退版本永久置 revoked。
func (store *Store) RollbackPromotion(ctx context.Context, targetKind, reason string, now time.Time) (memory.Promotion, memory.Activation, error) {
	if targetKind != memory.TargetKindContextCompactionParams || reason == "" || !memory.ValidEvolutionRejectionReason(reason) {
		return memory.Promotion{}, memory.Activation{}, errors.New("rollback requires a non-empty reason from the closed lexicon")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("start rollback transaction: %w", err)
	}
	defer transaction.Rollback()
	activation, found, err := lookupEvolutionActivation(ctx, transaction, targetKind)
	if err != nil || !found {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: no sandbox activation to roll back", memory.ErrConflict)
	}
	promotion, found, err := lookupEvolutionPromotion(ctx, transaction, activation.PromotionID)
	if err != nil || !found || promotion.Status != memory.PromotionStatusSandboxActivated {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: activation points at a non-activated version", memory.ErrConflict)
	}
	if activation.PreviousPromotionID != promotion.RollbackTargetID {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: activation previous version diverges from the approved decision", memory.ErrConflict)
	}
	// 被恢复版本必须仍处于 sandbox 激活态，且恢复后的指针延续该版本决策
	// 固定的回退目标作为 previous——否则链式回退会丢失审批上下文，使全库
	// 检查失配并形成不可回退的死路版本。
	restoredTarget := ""
	if activation.PreviousPromotionID != "" {
		restored, found, err := lookupEvolutionPromotion(ctx, transaction, activation.PreviousPromotionID)
		if err != nil {
			return memory.Promotion{}, memory.Activation{}, err
		}
		if !found || restored.Status != memory.PromotionStatusSandboxActivated {
			return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: restored version is not sandbox-activated", memory.ErrConflict)
		}
		restoredTarget = restored.RollbackTargetID
		if _, err := transaction.ExecContext(ctx, "UPDATE evolution_activations SET promotion_id = ?, previous_promotion_id = ?, activated_at = ? WHERE environment = ? AND target_kind = ?",
			restored.ID, nullString(restoredTarget), now.UTC().Format(time.RFC3339Nano), memory.SandboxEnvironment, targetKind); err != nil {
			return memory.Promotion{}, memory.Activation{}, fmt.Errorf("restore sandbox activation: %w", err)
		}
	} else if _, err := transaction.ExecContext(ctx, "DELETE FROM evolution_activations WHERE environment = ? AND target_kind = ?", memory.SandboxEnvironment, targetKind); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("reset sandbox activation to baseline: %w", err)
	}
	promotion.Status = memory.PromotionStatusRevoked
	promotion.RevokedAt = stripMonotonic(now)
	if _, err := transaction.ExecContext(ctx, "UPDATE evolution_promotions SET status = ?, revoked_at = ? WHERE promotion_id = ?", promotion.Status, promotion.RevokedAt.UTC().Format(time.RFC3339Nano), promotion.ID); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("revoke rolled-back version: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, promotion.CandidateID, memory.EvolutionAuditPromotionRolledBack, reason, promotion.RevokedAt); err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	rolled, found, err := lookupEvolutionPromotion(ctx, transaction, promotion.ID)
	if err != nil || !found || !evolutionPromotionRowsMatch(rolled, promotion) {
		return memory.Promotion{}, memory.Activation{}, errors.New("rollback version read-back verification failed")
	}
	resultActivation := memory.Activation{Environment: memory.SandboxEnvironment, TargetKind: targetKind}
	if activation.PreviousPromotionID != "" {
		resultActivation, found, err = lookupEvolutionActivation(ctx, transaction, targetKind)
		if err != nil || !found || resultActivation.PromotionID != activation.PreviousPromotionID || resultActivation.PreviousPromotionID != restoredTarget {
			return memory.Promotion{}, memory.Activation{}, errors.New("rollback activation read-back verification failed")
		}
	}
	if err := transaction.Commit(); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("commit sandbox rollback: %w", err)
	}
	return rolled, resultActivation, nil
}

// ListPromotions 只读列出某一目标类型的完整版本投影。
func (store *Store) ListPromotions(targetKind string) ([]memory.Promotion, error) {
	rows, err := store.db.QueryContext(context.Background(), "SELECT promotion_id FROM evolution_promotions WHERE target_kind = ? ORDER BY version ASC", targetKind)
	if err != nil {
		return nil, fmt.Errorf("query promotions: %w", err)
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	promotions := make([]memory.Promotion, 0, len(ids))
	for _, id := range ids {
		promotion, found, err := lookupEvolutionPromotion(context.Background(), store.db, id)
		if err != nil || !found {
			return nil, fmt.Errorf("read listed promotion: %w", err)
		}
		promotions = append(promotions, promotion)
	}
	return promotions, nil
}

// ActivationFor 只读返回当前 sandbox 激活指针；无行表示产品基线。
func (store *Store) ActivationFor(targetKind string) (memory.Activation, bool, error) {
	return lookupEvolutionActivation(context.Background(), store.db, targetKind)
}

// lookupEvolutionPromotion 联结版本与不可变决策，构造唯一的只读展示投影。
func lookupEvolutionPromotion(ctx context.Context, source queryer, promotionID string) (memory.Promotion, bool, error) {
	const query = `SELECT p.promotion_id, p.schema_version, p.decision_id, p.target_kind, p.version, p.variant_json, p.status, p.created_at, p.revoked_at,
		d.candidate_id, d.evaluation_id, d.variant_fingerprint, d.variant_spec_sha256, d.baseline_fingerprint, d.eval_set_fingerprint, d.suite_result_digest, d.comparator_version, d.conclusion_trigger, d.metrics_json, d.rollback_target_promotion_id, d.approval_fingerprint, d.approval_nonce, d.approver
		FROM evolution_promotions p JOIN evolution_promotion_decisions d ON d.decision_id = p.decision_id WHERE p.promotion_id = ?`
	var promotion memory.Promotion
	var variantJSON, metricsJSON, createdAt string
	var rollbackTarget, revokedAt sql.NullString
	err := source.QueryRowContext(ctx, query, promotionID).Scan(&promotion.ID, &promotion.SchemaVersion, &promotion.DecisionID, &promotion.TargetKind, &promotion.Version, &variantJSON, &promotion.Status, &createdAt, &revokedAt,
		&promotion.CandidateID, &promotion.EvaluationID, &promotion.VariantFingerprint, &promotion.VariantSpecSHA256, &promotion.BaselineFingerprint, &promotion.EvalSetFingerprint, &promotion.SuiteResultDigest, &promotion.ComparatorVersion, &promotion.ConclusionTrigger, &metricsJSON, &rollbackTarget, &promotion.ApprovalFingerprint, &promotion.ApprovalNonce, &promotion.Approver)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Promotion{}, false, nil
	}
	if err != nil {
		return memory.Promotion{}, false, fmt.Errorf("query promotion: %w", err)
	}
	if err := json.Unmarshal([]byte(variantJSON), &promotion.Variant); err != nil || json.Unmarshal([]byte(metricsJSON), &promotion.Metrics) != nil {
		return memory.Promotion{}, false, errors.New("decode stored promotion projection")
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return memory.Promotion{}, false, fmt.Errorf("parse promotion created_at: %w", err)
	}
	promotion.CreatedAt, promotion.Conclusion, promotion.RollbackTargetID = parsed, memory.EvaluationStatusPassed, rollbackTarget.String
	if revokedAt.Valid {
		promotion.RevokedAt, err = time.Parse(time.RFC3339Nano, revokedAt.String)
		if err != nil {
			return memory.Promotion{}, false, fmt.Errorf("parse promotion revoked_at: %w", err)
		}
	}
	if err := promotion.Validate(); err != nil {
		return memory.Promotion{}, false, fmt.Errorf("stored promotion violates its contract: %w", err)
	}
	return promotion, true, nil
}

// lookupEvolutionActivation 读取唯一 sandbox 指针；行缺失表示产品基线。
func lookupEvolutionActivation(ctx context.Context, source queryer, targetKind string) (memory.Activation, bool, error) {
	const query = `SELECT environment, target_kind, promotion_id, previous_promotion_id, activated_at FROM evolution_activations WHERE environment = ? AND target_kind = ?`
	var activation memory.Activation
	var previous sql.NullString
	var activatedAt string
	err := source.QueryRowContext(ctx, query, memory.SandboxEnvironment, targetKind).Scan(&activation.Environment, &activation.TargetKind, &activation.PromotionID, &previous, &activatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Activation{}, false, nil
	}
	if err != nil {
		return memory.Activation{}, false, fmt.Errorf("query sandbox activation: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, activatedAt)
	if err != nil {
		return memory.Activation{}, false, fmt.Errorf("parse activation time: %w", err)
	}
	activation.PreviousPromotionID, activation.ActivatedAt = previous.String, parsed
	return activation, true, nil
}

// evolutionPromotionRowsMatch 比较版本投影的全部闭合字段。
func evolutionPromotionRowsMatch(actual, expected memory.Promotion) bool {
	return actual.ID == expected.ID && actual.SchemaVersion == expected.SchemaVersion && actual.DecisionID == expected.DecisionID && actual.CandidateID == expected.CandidateID && actual.EvaluationID == expected.EvaluationID && actual.TargetKind == expected.TargetKind && actual.Version == expected.Version && actual.Variant == expected.Variant && actual.VariantFingerprint == expected.VariantFingerprint && actual.VariantSpecSHA256 == expected.VariantSpecSHA256 && actual.BaselineFingerprint == expected.BaselineFingerprint && actual.EvalSetFingerprint == expected.EvalSetFingerprint && actual.SuiteResultDigest == expected.SuiteResultDigest && actual.ComparatorVersion == expected.ComparatorVersion && actual.Conclusion == expected.Conclusion && actual.ConclusionTrigger == expected.ConclusionTrigger && actual.Metrics == expected.Metrics && actual.RollbackTargetID == expected.RollbackTargetID && actual.ApprovalFingerprint == expected.ApprovalFingerprint && actual.ApprovalNonce == expected.ApprovalNonce && actual.Approver == expected.Approver && actual.Status == expected.Status && actual.CreatedAt.Equal(expected.CreatedAt) && actual.RevokedAt.Equal(expected.RevokedAt)
}

// nullString 把空串折叠为 SQL NULL，供可空外键列使用。
func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

// lookupEvolutionPromotionForCandidate 按决策唯一候选键读取版本投影。
func lookupEvolutionPromotionForCandidate(ctx context.Context, source queryer, candidateID string) (memory.Promotion, bool, error) {
	var id string
	err := source.QueryRowContext(ctx, "SELECT p.promotion_id FROM evolution_promotions p JOIN evolution_promotion_decisions d ON d.decision_id = p.decision_id WHERE d.candidate_id = ?", candidateID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Promotion{}, false, nil
	}
	if err != nil {
		return memory.Promotion{}, false, fmt.Errorf("query promotion by candidate: %w", err)
	}
	return lookupEvolutionPromotion(ctx, source, id)
}

// checkPromotionInvariants 验证决策、版本、候选、评测和 sandbox 指针的全库
// 交叉一致性，防止格式合法的证据字段被旁路篡改。
func checkPromotionInvariants(ctx context.Context, source queryerRows) error {
	rows, err := source.QueryContext(ctx, "SELECT promotion_id FROM evolution_promotions ORDER BY target_kind, version")
	if err != nil {
		return fmt.Errorf("query promotion ledger: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	versions := map[string][]int{}
	for _, id := range ids {
		promotion, found, err := lookupEvolutionPromotion(ctx, source, id)
		if err != nil || !found {
			return fmt.Errorf("read promotion invariant: %w", err)
		}
		expectedID, err := memory.PromotionID(promotion.TargetKind, promotion.Version)
		if err != nil || expectedID != promotion.ID {
			return fmt.Errorf("promotion %s identity is not derivable", promotion.ID)
		}
		expectedApproval, err := memory.PromotionApprovalFingerprint(promotion)
		if err != nil || expectedApproval != promotion.ApprovalFingerprint {
			return fmt.Errorf("promotion %s approval tuple is not derivable", promotion.ID)
		}
		expectedDecision, err := memory.PromotionDecisionID(promotion)
		if err != nil || expectedDecision != promotion.DecisionID {
			return fmt.Errorf("promotion %s decision identity is not derivable", promotion.ID)
		}
		candidate, found, err := lookupEvolutionCandidate(ctx, source, promotion.CandidateID)
		if err != nil || !found || candidate.Status != memory.CandidateStatePromoted || candidate.TargetKind != promotion.TargetKind || candidate.VariantFingerprint != promotion.VariantFingerprint || candidate.VariantSpecSHA256 != promotion.VariantSpecSHA256 || candidate.BaselineFingerprint != promotion.BaselineFingerprint {
			return fmt.Errorf("promotion %s diverges from its candidate", promotion.ID)
		}
		evaluation, found, err := lookupEvolutionEvaluation(ctx, source, promotion.EvaluationID)
		if err != nil || !found || evaluation.CandidateID != candidate.ID || evaluation.Status != memory.EvaluationStatusPassed || evaluation.EvalSetFingerprint != promotion.EvalSetFingerprint || evaluation.BaselineConfigFingerprint != promotion.BaselineFingerprint || evaluation.SuiteResultDigest != promotion.SuiteResultDigest || evaluation.ComparatorVersion != promotion.ComparatorVersion {
			return fmt.Errorf("promotion %s diverges from its evaluation", promotion.ID)
		}
		if promotion.RollbackTargetID != "" {
			target, found, err := lookupEvolutionPromotion(ctx, source, promotion.RollbackTargetID)
			if err != nil || !found || target.TargetKind != promotion.TargetKind || target.Version >= promotion.Version {
				return fmt.Errorf("promotion %s has an invalid rollback target", promotion.ID)
			}
		}
		versions[promotion.TargetKind] = append(versions[promotion.TargetKind], promotion.Version)
	}
	for kind, values := range versions {
		for index, value := range values {
			if value != index+1 {
				return fmt.Errorf("promotion versions for %s are not contiguous", kind)
			}
		}
	}
	activationRows, err := source.QueryContext(ctx, "SELECT environment, target_kind, promotion_id, previous_promotion_id, activated_at FROM evolution_activations")
	if err != nil {
		return fmt.Errorf("query sandbox activations: %w", err)
	}
	type activationRef struct {
		environment string
		targetKind  string
		promotionID string
		previousID  sql.NullString
		activatedAt string
	}
	var activationRefs []activationRef
	for activationRows.Next() {
		var ref activationRef
		if err := activationRows.Scan(&ref.environment, &ref.targetKind, &ref.promotionID, &ref.previousID, &ref.activatedAt); err != nil {
			return err
		}
		activationRefs = append(activationRefs, ref)
	}
	if err := activationRows.Err(); err != nil {
		return err
	}
	if err := activationRows.Close(); err != nil {
		return err
	}
	for _, ref := range activationRefs {
		if ref.environment != memory.SandboxEnvironment || ref.targetKind != memory.TargetKindContextCompactionParams {
			return errors.New("activation carries an unsupported environment or target")
		}
		promotion, found, err := lookupEvolutionPromotion(ctx, source, ref.promotionID)
		if err != nil || !found || promotion.Status != memory.PromotionStatusSandboxActivated || promotion.TargetKind != ref.targetKind || promotion.RollbackTargetID != ref.previousID.String {
			return errors.New("sandbox activation diverges from its approved version")
		}
		if ref.previousID.Valid {
			prior, found, err := lookupEvolutionPromotion(ctx, source, ref.previousID.String)
			if err != nil || !found || prior.Status != memory.PromotionStatusSandboxActivated || prior.TargetKind != ref.targetKind {
				return errors.New("sandbox activation previous version is invalid")
			}
		}
	}
	return nil
}
