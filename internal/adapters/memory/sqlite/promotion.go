// 本文件承载晋升版本账本的存储事务：人工晋升（evaluated_passed → promoted，
// 版本记录落账与激活指针推进）与激活回退（指针回移与版本置 rolled_back）。
// 两个事务都遵循写集封存 → 写入 → 多重读回比对 → 提交或整体回滚的仓库
// 惯例；晋升额外要求审批指纹精确匹配与评测摘要逐字一致。
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

// PromoteCandidate 在单一事务内执行人工晋升：候选必须处于 evaluated_passed
// 且其评测行 status=passed、摘要与请求逐字一致；变体规格摘要必须与登记值
// 相同（正文与指纹双核对）；版本号取该目标类型当前最大值加一并在事务内锁
// 定写入；审批指纹必须与全部冻结输入的重算值精确匹配。写集为候选迁移、
// 版本记录插入、激活指针 upsert 与审计四件，任一读回偏差整体回滚。
func (store *Store) PromoteCandidate(ctx context.Context, promotion memory.Promotion, approvalFingerprint string, now time.Time) (memory.Promotion, error) {
	if err := promotion.Validate(); err != nil {
		return memory.Promotion{}, fmt.Errorf("validate promotion: %w", err)
	}
	if approvalFingerprint == "" {
		return memory.Promotion{}, errors.New("promotion requires an explicit approval fingerprint")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	promotion.Status = memory.PromotionStatusActive
	promotion.RolledBackAt = time.Time{}
	promotion.CreatedAt = stripMonotonic(now)

	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Promotion{}, fmt.Errorf("start promotion transaction: %w", err)
	}
	defer transaction.Rollback()

	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, promotion.CandidateID)
	if err != nil {
		return memory.Promotion{}, err
	}
	if !found {
		return memory.Promotion{}, memory.ErrNotFound
	}
	if candidate.Status != memory.CandidateStateEvaluatedPassed {
		return memory.Promotion{}, fmt.Errorf("%w: evolution candidate %s must be evaluated_passed before promotion, got %s", memory.ErrConflict, candidate.ID, candidate.Status)
	}
	if candidate.TargetKind != promotion.TargetKind {
		return memory.Promotion{}, fmt.Errorf("%w: promotion target kind does not match the candidate", memory.ErrConflict)
	}
	if candidate.VariantFingerprint != promotion.VariantFingerprint || candidate.VariantSpecSHA256 != promotion.VariantSpecSHA256 {
		return memory.Promotion{}, fmt.Errorf("%w: promotion variant does not match the registered candidate", memory.ErrConflict)
	}
	if candidate.BaselineFingerprint != promotion.BaselineFingerprint {
		return memory.Promotion{}, fmt.Errorf("%w: promotion baseline fingerprint does not match the registered candidate", memory.ErrConflict)
	}
	evaluation, found, err := lookupEvolutionEvaluationForCandidate(ctx, transaction, promotion.CandidateID)
	if err != nil {
		return memory.Promotion{}, err
	}
	if !found || evaluation.Status != memory.EvaluationStatusPassed {
		return memory.Promotion{}, fmt.Errorf("%w: promotion requires a passed evaluation row", memory.ErrConflict)
	}
	if evaluation.EvalSetFingerprint != promotion.EvalSetFingerprint || evaluation.SuiteResultDigest != promotion.SuiteResultDigest {
		return memory.Promotion{}, fmt.Errorf("%w: promotion frozen inputs diverge from the concluded evaluation", memory.ErrConflict)
	}
	if evaluation.ComparatorVersion != promotion.ComparatorVersion {
		return memory.Promotion{}, fmt.Errorf("%w: promotion comparator version diverges from the concluded evaluation", memory.ErrConflict)
	}
	promotion.Conclusion = memory.EvaluationStatusPassed

	// 版本号在事务内取该目标类型最大值加一：并发晋升同目标类型时，唯一
	// 约束与读回共同保证只有一个事务成功，另一个以冲突失败关闭。
	currentMax := 0
	if err := transaction.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM evolution_promotions WHERE target_kind = ?", promotion.TargetKind).Scan(&currentMax); err != nil {
		return memory.Promotion{}, fmt.Errorf("read promotion version ceiling: %w", err)
	}
	if promotion.Version != currentMax+1 {
		return memory.Promotion{}, fmt.Errorf("%w: promotion version %d is not the next version %d", memory.ErrConflict, promotion.Version, currentMax+1)
	}
	derivedID, deriveErr := memory.PromotionID(promotion.CandidateID, promotion.VariantFingerprint, promotion.Version)
	if deriveErr != nil {
		return memory.Promotion{}, fmt.Errorf("re-derive promotion identity: %w", deriveErr)
	}
	if derivedID != promotion.ID {
		return memory.Promotion{}, fmt.Errorf("%w: promotion identity is not derivable from its frozen tuple", memory.ErrConflict)
	}
	expectedApproval, fpErr := memory.PromotionApprovalFingerprint(promotion.CandidateID, promotion.VariantFingerprint, promotion.EvalSetFingerprint, promotion.SuiteResultDigest, promotion.TargetKind, promotion.Version)
	if fpErr != nil {
		return memory.Promotion{}, fpErr
	}
	if approvalFingerprint != expectedApproval {
		return memory.Promotion{}, fmt.Errorf("%w: promotion approval fingerprint mismatch; expected %s", memory.ErrConflict, expectedApproval)
	}
	// 回退目标语义由激活指针唯一锚定：目标必须精确等于晋升时刻的激活版本
	// 且其自身处于活跃态；激活为基线时目标必须为空。这保证任意版本在回退
	// 后都能继续回退，不存在不可回退的死路版本。
	currentActivation, activationFound, err := lookupEvolutionActivation(ctx, transaction, promotion.TargetKind)
	if err != nil {
		return memory.Promotion{}, err
	}
	if activationFound {
		if promotion.RollbackTargetID != currentActivation.PromotionID {
			return memory.Promotion{}, fmt.Errorf("%w: promotion rollback target must be the currently active version %s", memory.ErrConflict, currentActivation.PromotionID)
		}
		target, found, targetErr := lookupEvolutionPromotion(ctx, transaction, promotion.RollbackTargetID)
		if targetErr != nil {
			return memory.Promotion{}, targetErr
		}
		if !found || target.TargetKind != promotion.TargetKind || target.Status != memory.PromotionStatusActive {
			return memory.Promotion{}, fmt.Errorf("%w: promotion rollback target must be an active version of the same target kind", memory.ErrConflict)
		}
	} else if promotion.RollbackTargetID != "" {
		return memory.Promotion{}, fmt.Errorf("%w: promotion must carry an empty rollback target while the baseline is active", memory.ErrConflict)
	}
	if err := transitionEvolutionCandidate(ctx, transaction, candidate.ID, memory.CandidateStatePromoted); err != nil {
		return memory.Promotion{}, err
	}
	variantJSON, err := json.Marshal(promotion.Variant)
	if err != nil {
		return memory.Promotion{}, fmt.Errorf("encode promotion variant: %w", err)
	}
	metricsJSON, err := json.Marshal(promotion.Metrics)
	if err != nil {
		return memory.Promotion{}, fmt.Errorf("encode promotion metrics: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_promotions(
		promotion_id, schema_version, candidate_id, target_kind, version,
		variant_json, variant_fingerprint, variant_spec_sha256, baseline_fingerprint,
		eval_set_fingerprint, suite_result_digest, comparator_version,
		conclusion, conclusion_trigger, metrics_json, approver,
		rollback_target_promotion_id, status, created_at, rolled_back_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		promotion.ID, promotion.SchemaVersion, promotion.CandidateID, promotion.TargetKind, promotion.Version,
		string(variantJSON), promotion.VariantFingerprint, promotion.VariantSpecSHA256, promotion.BaselineFingerprint,
		promotion.EvalSetFingerprint, promotion.SuiteResultDigest, promotion.ComparatorVersion,
		promotion.Conclusion, promotion.ConclusionTrigger, string(metricsJSON), promotion.Approver,
		nullString(promotion.RollbackTargetID), promotion.Status, promotion.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return memory.Promotion{}, fmt.Errorf("insert promotion row: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_activations(target_kind, promotion_id, activated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(target_kind) DO UPDATE SET promotion_id = excluded.promotion_id, activated_at = excluded.activated_at`,
		promotion.TargetKind, promotion.ID, promotion.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return memory.Promotion{}, fmt.Errorf("advance activation pointer: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, promotion.CandidateID, memory.EvolutionAuditPromoted, "", promotion.CreatedAt.UTC()); err != nil {
		return memory.Promotion{}, err
	}
	readBack, err := finishPromotionReadBack(ctx, transaction, promotion, candidate, evaluation)
	if err != nil {
		return memory.Promotion{}, err
	}
	if err := transaction.Commit(); err != nil {
		return memory.Promotion{}, fmt.Errorf("commit promotion: %w", err)
	}
	return readBack, nil
}

// finishPromotionReadBack 对晋升写集做三重读回：候选终态、版本行全字段、
// 激活指针与审计全史精确序列。
func finishPromotionReadBack(ctx context.Context, transaction *sql.Tx, expected memory.Promotion, before memory.EvolutionCandidate, evaluation memory.Evaluation) (memory.Promotion, error) {
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, expected.CandidateID)
	if err != nil || !found {
		return memory.Promotion{}, fmt.Errorf("read back promoted candidate: %w", err)
	}
	expectedCandidate := before
	expectedCandidate.Status = memory.CandidateStatePromoted
	if !evolutionCandidateReadBackMatches(candidate, expectedCandidate) {
		return memory.Promotion{}, fmt.Errorf("promotion candidate read-back verification failed for %s", expected.CandidateID)
	}
	row, found, err := lookupEvolutionPromotion(ctx, transaction, expected.ID)
	if err != nil || !found {
		return memory.Promotion{}, fmt.Errorf("read back promotion row: %w", err)
	}
	if !evolutionPromotionRowsMatch(row, expected) {
		return memory.Promotion{}, fmt.Errorf("promotion row read-back verification failed for %s", expected.ID)
	}
	activation, found, err := lookupEvolutionActivation(ctx, transaction, expected.TargetKind)
	if err != nil || !found || activation.PromotionID != expected.ID {
		return memory.Promotion{}, fmt.Errorf("activation pointer read-back verification failed for %s", expected.TargetKind)
	}
	audit, err := evolutionAuditFor(ctx, transaction, expected.CandidateID)
	if err != nil {
		return memory.Promotion{}, err
	}
	if err := verifyPromotionAuditSequence(audit, before.CreatedAt, evaluation, expected); err != nil {
		return memory.Promotion{}, err
	}
	return row, nil
}

// verifyPromotionAuditSequence 校验晋升后的审计全史：created、唯一一次
// evaluation_started、结论 passed 与本次 promoted 恰好按各自真实时刻出现。
func verifyPromotionAuditSequence(audit []memory.EvolutionAuditEntry, createdAt time.Time, evaluation memory.Evaluation, expected memory.Promotion) error {
	desired := []memory.EvolutionAuditEntry{
		{CandidateID: expected.CandidateID, Action: memory.EvolutionAuditCreated, OccurredAt: createdAt},
		{CandidateID: expected.CandidateID, Action: memory.EvolutionAuditEvaluationStarted, OccurredAt: evaluation.StartedAt},
		{CandidateID: expected.CandidateID, Action: memory.EvolutionAuditEvaluatedPassed, OccurredAt: evaluation.FinishedAt},
		{CandidateID: expected.CandidateID, Action: memory.EvolutionAuditPromoted, OccurredAt: expected.CreatedAt},
	}
	if len(audit) != len(desired) || !evolutionAuditMatches(audit, desired) {
		return fmt.Errorf("promotion audit read-back verification failed for %s", expected.CandidateID)
	}
	return nil
}

// RollbackPromotion 在单一事务内回退当前激活版本：只接受激活指针所指的
// active 版本；指针移回其回退目标（无目标则删除指针行=回产品基线），版本
// 置 rolled_back 并落审计。回退不改变版本记录正文，也不可对历史版本重复
// 执行。
func (store *Store) RollbackPromotion(ctx context.Context, targetKind, reason string, now time.Time) (memory.Promotion, memory.Activation, error) {
	if targetKind != memory.TargetKindContextCompactionParams {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("unsupported rollback target kind %q", targetKind)
	}
	if reason != "" && !memory.ValidEvolutionRejectionReason(reason) {
		return memory.Promotion{}, memory.Activation{}, errors.New("rollback reason must come from the closed lexicon")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = stripMonotonic(now)
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("start rollback transaction: %w", err)
	}
	defer transaction.Rollback()

	activation, found, err := lookupEvolutionActivation(ctx, transaction, targetKind)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	if !found {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: no active promotion to roll back for %s", memory.ErrConflict, targetKind)
	}
	promotion, found, err := lookupEvolutionPromotion(ctx, transaction, activation.PromotionID)
	if err != nil || !found {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("load active promotion %s: %w", activation.PromotionID, err)
	}
	if promotion.Status != memory.PromotionStatusActive {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: promotion %s is not active", memory.ErrConflict, promotion.ID)
	}
	if promotion.RollbackTargetID == "" {
		if _, err := transaction.ExecContext(ctx, "DELETE FROM evolution_activations WHERE target_kind = ?", targetKind); err != nil {
			return memory.Promotion{}, memory.Activation{}, fmt.Errorf("reset activation to baseline: %w", err)
		}
	} else {
		target, found, err := lookupEvolutionPromotion(ctx, transaction, promotion.RollbackTargetID)
		if err != nil || !found || target.Status != memory.PromotionStatusActive {
			return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: rollback target %s must be an active promotion", memory.ErrConflict, promotion.RollbackTargetID)
		}
		if _, err := transaction.ExecContext(ctx, "UPDATE evolution_activations SET promotion_id = ?, activated_at = ? WHERE target_kind = ?",
			target.ID, now.UTC().Format(time.RFC3339Nano), targetKind); err != nil {
			return memory.Promotion{}, memory.Activation{}, fmt.Errorf("move activation pointer to rollback target: %w", err)
		}
	}
	promotion.Status = memory.PromotionStatusRolledBack
	promotion.RolledBackAt = now
	if _, err := transaction.ExecContext(ctx, "UPDATE evolution_promotions SET status = ?, rolled_back_at = ? WHERE promotion_id = ?",
		promotion.Status, now.UTC().Format(time.RFC3339Nano), promotion.ID); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("mark promotion rolled back: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, promotion.CandidateID, memory.EvolutionAuditPromotionRolledBack, reason, now.UTC()); err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	// 读回：版本行状态、指针落点与审计追加各就各位。
	row, found, err := lookupEvolutionPromotion(ctx, transaction, promotion.ID)
	if err != nil || !found || !evolutionPromotionRowsMatch(row, promotion) {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("rollback promotion read-back verification failed for %s", promotion.ID)
	}
	var rolledActivation memory.Activation
	var rolledFound bool
	if promotion.RollbackTargetID == "" {
		rolledActivation = memory.Activation{TargetKind: targetKind}
	} else {
		rolledActivation, rolledFound, err = lookupEvolutionActivation(ctx, transaction, targetKind)
		if err != nil || !rolledFound || rolledActivation.PromotionID != promotion.RollbackTargetID {
			return memory.Promotion{}, memory.Activation{}, fmt.Errorf("rollback activation read-back verification failed for %s", targetKind)
		}
	}
	audit, err := evolutionAuditFor(ctx, transaction, promotion.CandidateID)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	rollbackEntry := memory.EvolutionAuditEntry{CandidateID: promotion.CandidateID, Action: memory.EvolutionAuditPromotionRolledBack, Reason: reason, OccurredAt: now}
	if len(audit) == 0 || !evolutionAuditEntryMatches(audit[len(audit)-1], rollbackEntry) {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("rollback audit read-back verification failed for %s", promotion.CandidateID)
	}
	if err := transaction.Commit(); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("commit rollback: %w", err)
	}
	return row, rolledActivation, nil
}

// ListPromotions 只读返回指定目标类型的全部版本记录，按版本号升序。
func (store *Store) ListPromotions(targetKind string) ([]memory.Promotion, error) {
	rows, err := store.db.QueryContext(context.Background(), "SELECT promotion_id FROM evolution_promotions WHERE target_kind = ? ORDER BY version ASC", targetKind)
	if err != nil {
		return nil, fmt.Errorf("query promotions: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	promotions := make([]memory.Promotion, 0, len(ids))
	for _, id := range ids {
		promotion, found, err := lookupEvolutionPromotion(context.Background(), store.db, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("promotion %s vanished during listing", id)
		}
		promotions = append(promotions, promotion)
	}
	return promotions, nil
}

// ActivationFor 只读返回目标类型的当前激活指针；无行时第二返回值为假，
// 表示激活产品基线。
func (store *Store) ActivationFor(targetKind string) (memory.Activation, bool, error) {
	return lookupEvolutionActivation(context.Background(), store.db, targetKind)
}

// lookupEvolutionPromotion 按身份读取版本行并复核其结构合同。
func lookupEvolutionPromotion(ctx context.Context, source queryer, promotionID string) (memory.Promotion, bool, error) {
	const query = `SELECT promotion_id, schema_version, candidate_id, target_kind, version,
		variant_json, variant_fingerprint, variant_spec_sha256, baseline_fingerprint,
		eval_set_fingerprint, suite_result_digest, comparator_version,
		conclusion, conclusion_trigger, metrics_json, approver,
		rollback_target_promotion_id, status, created_at, rolled_back_at
		FROM evolution_promotions WHERE promotion_id = ?`
	row := memory.Promotion{}
	var variantJSON, metricsJSON string
	var rollbackTarget, rolledBackAt, createdAt sql.NullString
	err := source.QueryRowContext(ctx, query, promotionID).Scan(
		&row.ID, &row.SchemaVersion, &row.CandidateID, &row.TargetKind, &row.Version,
		&variantJSON, &row.VariantFingerprint, &row.VariantSpecSHA256, &row.BaselineFingerprint,
		&row.EvalSetFingerprint, &row.SuiteResultDigest, &row.ComparatorVersion,
		&row.Conclusion, &row.ConclusionTrigger, &metricsJSON, &row.Approver,
		&rollbackTarget, &row.Status, &createdAt, &rolledBackAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Promotion{}, false, nil
	}
	if err != nil {
		return memory.Promotion{}, false, fmt.Errorf("query promotion: %w", err)
	}
	if err := json.Unmarshal([]byte(variantJSON), &row.Variant); err != nil {
		return memory.Promotion{}, false, fmt.Errorf("decode promotion variant: %w", err)
	}
	if err := json.Unmarshal([]byte(metricsJSON), &row.Metrics); err != nil {
		return memory.Promotion{}, false, fmt.Errorf("decode promotion metrics: %w", err)
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt.String)
	if err != nil {
		return memory.Promotion{}, false, fmt.Errorf("parse promotion created_at: %w", err)
	}
	row.CreatedAt = created
	row.RollbackTargetID = rollbackTarget.String
	if rolledBackAt.Valid {
		rolled, err := time.Parse(time.RFC3339Nano, rolledBackAt.String)
		if err != nil {
			return memory.Promotion{}, false, fmt.Errorf("parse promotion rolled_back_at: %w", err)
		}
		row.RolledBackAt = rolled
	}
	if err := row.Validate(); err != nil {
		return memory.Promotion{}, false, fmt.Errorf("stored promotion violates its contract: %w", err)
	}
	return row, true, nil
}

// lookupEvolutionActivation 读取目标类型的激活指针；无行表示产品基线。
func lookupEvolutionActivation(ctx context.Context, source queryer, targetKind string) (memory.Activation, bool, error) {
	const query = `SELECT target_kind, promotion_id, activated_at FROM evolution_activations WHERE target_kind = ?`
	var activation memory.Activation
	var activatedAt string
	err := source.QueryRowContext(ctx, query, targetKind).Scan(&activation.TargetKind, &activation.PromotionID, &activatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Activation{}, false, nil
	}
	if err != nil {
		return memory.Activation{}, false, fmt.Errorf("query activation: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, activatedAt)
	if err != nil {
		return memory.Activation{}, false, fmt.Errorf("parse activation time: %w", err)
	}
	activation.ActivatedAt = parsed
	return activation, true, nil
}

// evolutionPromotionRowsMatch 比较版本行的全部闭合字段；时间用语义相等。
func evolutionPromotionRowsMatch(actual, expected memory.Promotion) bool {
	return actual.ID == expected.ID && actual.SchemaVersion == expected.SchemaVersion &&
		actual.CandidateID == expected.CandidateID && actual.TargetKind == expected.TargetKind &&
		actual.Version == expected.Version && actual.Variant == expected.Variant &&
		actual.VariantFingerprint == expected.VariantFingerprint && actual.VariantSpecSHA256 == expected.VariantSpecSHA256 &&
		actual.BaselineFingerprint == expected.BaselineFingerprint &&
		actual.EvalSetFingerprint == expected.EvalSetFingerprint &&
		actual.SuiteResultDigest == expected.SuiteResultDigest &&
		actual.ComparatorVersion == expected.ComparatorVersion &&
		actual.Conclusion == expected.Conclusion && actual.ConclusionTrigger == expected.ConclusionTrigger &&
		actual.Metrics == expected.Metrics && actual.Approver == expected.Approver &&
		actual.RollbackTargetID == expected.RollbackTargetID && actual.Status == expected.Status &&
		actual.CreatedAt.Equal(expected.CreatedAt) && actual.RolledBackAt.Equal(expected.RolledBackAt)
}

// evolutionAuditEntryMatches 比较单条审计的闭合字段（含可空理由）。
func evolutionAuditEntryMatches(actual, expected memory.EvolutionAuditEntry) bool {
	return actual.CandidateID == expected.CandidateID && actual.Action == expected.Action &&
		actual.Reason == expected.Reason && actual.OccurredAt.Equal(expected.OccurredAt)
}

// nullString 把空串折叠为 SQL NULL，供可空外键列使用。
func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

// lookupEvolutionPromotionForCandidate 按候选唯一键读取其版本记录。
func lookupEvolutionPromotionForCandidate(ctx context.Context, source queryer, candidateID string) (memory.Promotion, bool, error) {
	const query = `SELECT promotion_id FROM evolution_promotions WHERE candidate_id = ?`
	var promotionID string
	err := source.QueryRowContext(ctx, query, candidateID).Scan(&promotionID)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Promotion{}, false, nil
	}
	if err != nil {
		return memory.Promotion{}, false, fmt.Errorf("query promotion by candidate: %w", err)
	}
	return lookupEvolutionPromotion(ctx, source, promotionID)
}

// checkPromotionInvariants 验证版本账本与激活指针的全库一致性：身份可重
// 派生、版本连续无洞、回退目标合法、指针只指向同类型活跃版本、被回退的
// 版本不再被指向。它在 Check 的演化不变量之后执行。
func checkPromotionInvariants(ctx context.Context, source queryerRows) error {
	rows, err := source.QueryContext(ctx, "SELECT promotion_id FROM evolution_promotions ORDER BY target_kind ASC, version ASC")
	if err != nil {
		return fmt.Errorf("query promotion ledger: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
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
	byKindVersions := map[string][]int{}
	for _, id := range ids {
		promotion, found, err := lookupEvolutionPromotion(ctx, source, id)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("promotion %s vanished during check", id)
		}
		derivedID, deriveErr := memory.PromotionID(promotion.CandidateID, promotion.VariantFingerprint, promotion.Version)
		if deriveErr != nil || derivedID != promotion.ID {
			return fmt.Errorf("promotion %s identity is not derivable", promotion.ID)
		}
		promotedCandidate, foundCandidate, candidateErr := lookupEvolutionCandidate(ctx, source, promotion.CandidateID)
		if candidateErr != nil {
			return candidateErr
		}
		if !foundCandidate || promotedCandidate.Status != memory.CandidateStatePromoted {
			return fmt.Errorf("promotion %s references a candidate outside the promoted state", promotion.ID)
		}
		if promotion.Version == 1 && promotion.RollbackTargetID != "" {
			return fmt.Errorf("promotion %s is the first version but carries a rollback target", promotion.ID)
		}
		// 目标存在时才校验其合法性：空目标表示晋升时激活为基线，任何版本
		// 都合法；非空目标必须存在、同类型且版本更小。
		if promotion.RollbackTargetID != "" {
			target, found, err := lookupEvolutionPromotion(ctx, source, promotion.RollbackTargetID)
			if err != nil {
				return err
			}
			if !found || target.TargetKind != promotion.TargetKind || target.Version >= promotion.Version {
				return fmt.Errorf("promotion %s has an invalid rollback target", promotion.ID)
			}
		}
		byKindVersions[promotion.TargetKind] = append(byKindVersions[promotion.TargetKind], promotion.Version)
	}
	for kind, versions := range byKindVersions {
		for index, version := range versions {
			if version != index+1 {
				return fmt.Errorf("promotion versions for %s are not contiguous", kind)
			}
		}
	}
	activationRows, err := source.QueryContext(ctx, "SELECT target_kind, promotion_id FROM evolution_activations")
	if err != nil {
		return fmt.Errorf("query activations: %w", err)
	}
	defer activationRows.Close()
	type activationRef struct{ kind, promotionID string }
	var activations []activationRef
	for activationRows.Next() {
		var ref activationRef
		if err := activationRows.Scan(&ref.kind, &ref.promotionID); err != nil {
			return err
		}
		activations = append(activations, ref)
	}
	if err := activationRows.Err(); err != nil {
		return err
	}
	for _, ref := range activations {
		promotion, found, err := lookupEvolutionPromotion(ctx, source, ref.promotionID)
		if err != nil {
			return err
		}
		if !found || promotion.TargetKind != ref.kind || promotion.Status != memory.PromotionStatusActive {
			return fmt.Errorf("activation for %s points at a non-active or foreign promotion", ref.kind)
		}
	}
	return nil
}
