// 本文件实现可信记忆评测事务。
//
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

// StartEvaluation 在单一事务内启动候选评测：候选必须处于 ready 且来源仍为
// active；同 evaluation_id 的既有 running 行按冻结五元组等价幂等返回。事务
// 内完成评测行插入、候选迁移 evaluating、started 审计与三重读回比对，任一
// 失败整体回滚，不留下任何"评测已开始"的痕迹。
func (store *Store) StartEvaluation(ctx context.Context, candidateID string, evaluation memory.Evaluation, binding memory.EvaluationBinding, now time.Time) (memory.Evaluation, error) {
	if evaluation.Status != memory.EvaluationStatusRunning {
		return memory.Evaluation{}, errors.New("evaluation must start in the running state")
	}
	if evaluation.CandidateID != candidateID {
		return memory.Evaluation{}, errors.New("evaluation identity does not match the requested candidate")
	}
	if err := binding.Validate(); err != nil {
		return memory.Evaluation{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// 启动时刻是事务时钟的产物：冻结字段经请求带来，started_at 由这里统一
	// 锚定后再做合同校验。
	evaluation.StartedAt = stripMonotonic(now)
	if err := evaluation.Validate(); err != nil {
		return memory.Evaluation{}, fmt.Errorf("validate evaluation: %w", err)
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Evaluation{}, fmt.Errorf("start evaluation transaction: %w", err)
	}
	defer transaction.Rollback()

	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if !found {
		return memory.Evaluation{}, memory.ErrNotFound
	}
	// 无论新启动还是 running 幂等重入，都先在事务内复核候选不可变事实；
	// 恢复路径不得因已有评测行而绕过 Candidate/EvalSet/实际基线绑定。
	if candidate.EvalSetFingerprint == "" || evaluation.EvalSetFingerprint != candidate.EvalSetFingerprint {
		return memory.Evaluation{}, fmt.Errorf("%w: evaluation eval set fingerprint does not match the registered candidate", memory.ErrConflict)
	}
	if candidate.BaselineFingerprint != binding.BaselineFingerprint || candidate.BaselineFingerprint != evaluation.BaselineConfigFingerprint {
		return memory.Evaluation{}, fmt.Errorf("%w: evaluation baseline fingerprint does not match the registered candidate", memory.ErrConflict)
	}
	if candidate.VariantSpecSHA256 != binding.VariantSpecSHA256 || candidate.VariantFingerprint != binding.CandidateVariantFingerprint {
		return memory.Evaluation{}, fmt.Errorf("%w: evaluation variant specification does not match the registered candidate", memory.ErrConflict)
	}
	if err := requireEvolutionSourceActive(ctx, transaction, candidate, now); err != nil {
		return memory.Evaluation{}, err
	}
	derivedID, deriveErr := memory.EvaluationID(candidate.ID, evaluation.EvalSetFingerprint,
		evaluation.BaselineConfigFingerprint, evaluation.VariantConfigFingerprint, evaluation.ComparatorVersion)
	if deriveErr != nil {
		return memory.Evaluation{}, fmt.Errorf("re-derive evaluation identity: %w", deriveErr)
	}
	if derivedID != evaluation.ID {
		return memory.Evaluation{}, fmt.Errorf("%w: evaluation identity is not derivable from its frozen tuple", memory.ErrConflict)
	}
	existing, exists, err := lookupEvolutionEvaluation(ctx, transaction, evaluation.ID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if exists {
		identical, err := evolutionEvaluationIdempotentEquivalent(existing, evaluation)
		if err != nil {
			return memory.Evaluation{}, err
		}
		if identical && existing.Status == memory.EvaluationStatusRunning {
			if err := transaction.Commit(); err != nil {
				return memory.Evaluation{}, fmt.Errorf("commit idempotent evaluation start: %w", err)
			}
			return existing, nil
		}
		return memory.Evaluation{}, fmt.Errorf("%w: evaluation %s already exists in state %s", memory.ErrConflict, evaluation.ID, existing.Status)
	}
	if candidate.Status != memory.CandidateStateReady {
		return memory.Evaluation{}, fmt.Errorf("%w: evolution candidate %s must be ready before starting an evaluation", memory.ErrConflict, candidate.ID)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_evaluations(
		evaluation_id, schema_version, candidate_id, eval_set_fingerprint,
		baseline_config_fingerprint, variant_config_fingerprint, comparator_version,
		status, suite_result_digest, started_at, finished_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL)`,
		evaluation.ID, evaluation.SchemaVersion, evaluation.CandidateID, evaluation.EvalSetFingerprint,
		evaluation.BaselineConfigFingerprint, evaluation.VariantConfigFingerprint, evaluation.ComparatorVersion,
		memory.EvaluationStatusRunning, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return memory.Evaluation{}, fmt.Errorf("insert evaluation row: %w", err)
	}
	if err := transitionEvolutionCandidate(ctx, transaction, candidate.ID, memory.CandidateStateEvaluating); err != nil {
		return memory.Evaluation{}, err
	}
	insertedAudit := memory.EvolutionAuditEntry{CandidateID: candidate.ID, Action: memory.EvolutionAuditEvaluationStarted, OccurredAt: now.UTC()}
	if err := insertEvolutionAudit(ctx, transaction, insertedAudit.CandidateID, insertedAudit.Action, insertedAudit.Reason, insertedAudit.OccurredAt); err != nil {
		return memory.Evaluation{}, err
	}
	readBack, err := finishStartEvaluationReadBack(ctx, transaction, evaluation, candidate, insertedAudit)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if err := transaction.Commit(); err != nil {
		return memory.Evaluation{}, fmt.Errorf("commit evaluation start: %w", err)
	}
	return readBack, nil
}

// finishStartEvaluationReadBack 对启动写集做三重读回：候选状态、评测行全字
// 段、审计全史精确序列。触发器或旁路写入导致的偏差都会让整个事务回滚。
func finishStartEvaluationReadBack(ctx context.Context, transaction *sql.Tx, expected memory.Evaluation, before memory.EvolutionCandidate, auditEntry memory.EvolutionAuditEntry) (memory.Evaluation, error) {
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, expected.CandidateID)
	if err != nil || !found {
		return memory.Evaluation{}, fmt.Errorf("read back evaluated candidate: %w", err)
	}
	expectedCandidate := before
	expectedCandidate.Status = memory.CandidateStateEvaluating
	if !evolutionCandidateReadBackMatches(candidate, expectedCandidate) {
		return memory.Evaluation{}, fmt.Errorf("evaluation start read-back verification failed for %s", expected.CandidateID)
	}
	row, found, err := lookupEvolutionEvaluation(ctx, transaction, expected.ID)
	if err != nil || !found {
		return memory.Evaluation{}, fmt.Errorf("read back evaluation row: %w", err)
	}
	expectedRow := expected
	if !evolutionEvaluationRowsMatch(row, expectedRow) {
		return memory.Evaluation{}, fmt.Errorf("evaluation row read-back verification failed for %s", expected.ID)
	}
	audit, err := evolutionAuditFor(ctx, transaction, expected.CandidateID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	prior := []memory.EvolutionAuditEntry{{CandidateID: expected.CandidateID, Action: memory.EvolutionAuditCreated, OccurredAt: before.CreatedAt}}
	desired := append(prior, auditEntry)
	if len(audit) != 2 || !evolutionAuditMatches(audit, desired) {
		return memory.Evaluation{}, fmt.Errorf("evaluation start audit read-back verification failed for %s", expected.CandidateID)
	}
	return row, nil
}

// FinishEvaluation 在单一事务内闭合评测：结论态只能落在 running 行上；
// invalid 映射为候选 invalidated 并复用作废审计动作，其余结论各有专属迁移
// 与审计词。评审行、候选终态、审计行与读回在同一事务提交后即永久固定。
func (store *Store) FinishEvaluation(ctx context.Context, evaluationID string, status string, suiteResultDigest string, now time.Time) (memory.Evaluation, error) {
	conclusionCandidate, err := memory.CandidateStatusForEvaluation(status)
	if err != nil {
		return memory.Evaluation{}, err
	}
	conclusionAction, err := memory.ConclusionAuditActionForEvaluation(status)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if !memory.DigestHexShape(suiteResultDigest) {
		return memory.Evaluation{}, errors.New("suite result digest must be a sha256 digest reference")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Evaluation{}, fmt.Errorf("finish evaluation transaction: %w", err)
	}
	defer transaction.Rollback()
	current, found, err := lookupEvolutionEvaluation(ctx, transaction, evaluationID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if !found {
		return memory.Evaluation{}, memory.ErrNotFound
	}
	if current.Status == status {
		// 终态重放的幂等锚点只有"同一结论与同一证据摘要"；提交时钟属于
		// 首次闭合的事实，重试携带的新时钟不得改写它。
		idempotent := current.SuiteResultDigest == suiteResultDigest
		if idempotent {
			if err := transaction.Commit(); err != nil {
				return memory.Evaluation{}, fmt.Errorf("commit idempotent evaluation finish: %w", err)
			}
			return current, nil
		}
		return memory.Evaluation{}, fmt.Errorf("%w: evaluation %s is already concluded as %s", memory.ErrConflict, evaluationID, current.Status)
	}
	if current.Status != memory.EvaluationStatusRunning {
		return memory.Evaluation{}, fmt.Errorf("%w: evaluation %s in state %s cannot conclude to %s", memory.ErrConflict, evaluationID, current.Status, status)
	}
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, current.CandidateID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if !found {
		return memory.Evaluation{}, fmt.Errorf("evaluated evolution candidate %s has disappeared", current.CandidateID)
	}
	if candidate.Status != memory.CandidateStateEvaluating {
		return memory.Evaluation{}, fmt.Errorf("%w: evolution candidate %s must be evaluating before conclusion", memory.ErrConflict, candidate.ID)
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE evolution_evaluations SET status = ?, suite_result_digest = ?, finished_at = ? WHERE evaluation_id = ?`,
		status, suiteResultDigest, now.UTC().Format(time.RFC3339Nano), evaluationID); err != nil {
		return memory.Evaluation{}, fmt.Errorf("conclude evaluation row: %w", err)
	}
	if err := transitionEvolutionCandidate(ctx, transaction, candidate.ID, conclusionCandidate); err != nil {
		return memory.Evaluation{}, err
	}
	conclusionAudit := memory.EvolutionAuditEntry{CandidateID: candidate.ID, Action: conclusionAction, OccurredAt: now.UTC()}
	if err := insertEvolutionAudit(ctx, transaction, conclusionAudit.CandidateID, conclusionAudit.Action, conclusionAudit.Reason, conclusionAudit.OccurredAt); err != nil {
		return memory.Evaluation{}, err
	}
	readBack, err := finishConclusionReadBack(ctx, transaction, current, candidate, status, suiteResultDigest, conclusionAudit)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if err := transaction.Commit(); err != nil {
		return memory.Evaluation{}, fmt.Errorf("commit evaluation conclusion: %w", err)
	}
	return readBack, nil
}

// finishConclusionReadBack 对闭合写集做双向读回：评测行四字段更新、候选终
// 态迁移与"原有全部审计 + 新结论一条"的精确全史比对。
func finishConclusionReadBack(ctx context.Context, transaction *sql.Tx, prior memory.Evaluation, before memory.EvolutionCandidate, status, suiteResultDigest string, auditEntry memory.EvolutionAuditEntry) (memory.Evaluation, error) {
	row, found, err := lookupEvolutionEvaluation(ctx, transaction, prior.ID)
	if err != nil || !found {
		return memory.Evaluation{}, fmt.Errorf("read back concluded evaluation row: %w", err)
	}
	expected := prior
	expected.Status = status
	expected.SuiteResultDigest = suiteResultDigest
	expected.FinishedAt = stripMonotonic(auditEntry.OccurredAt)
	if !evolutionEvaluationRowsMatch(row, expected) {
		return memory.Evaluation{}, fmt.Errorf("evaluation conclusion read-back verification failed for %s", prior.ID)
	}
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, before.ID)
	if err != nil || !found {
		return memory.Evaluation{}, fmt.Errorf("read back concluded candidate: %w", err)
	}
	expectedCandidate := before
	expectedCandidate.Status, _ = memory.CandidateStatusForEvaluation(status)
	if !evolutionCandidateReadBackMatches(candidate, expectedCandidate) {
		return memory.Evaluation{}, fmt.Errorf("conclusion candidate read-back verification failed for %s", before.ID)
	}
	audit, err := evolutionAuditFor(ctx, transaction, before.ID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	// 结论路径的历史恒定为三段：created、唯一一次 started、本次结论；任何
	// 插入、缺失或替换都会在精确全史比对中暴露。
	desired := []memory.EvolutionAuditEntry{
		{CandidateID: before.ID, Action: memory.EvolutionAuditCreated, OccurredAt: before.CreatedAt},
		{CandidateID: before.ID, Action: memory.EvolutionAuditEvaluationStarted, OccurredAt: prior.StartedAt},
	}
	auditEntry.OccurredAt = stripMonotonic(auditEntry.OccurredAt)
	desired = append(desired, auditEntry)
	if !evolutionAuditMatches(audit, desired) {
		return memory.Evaluation{}, fmt.Errorf("conclusion audit read-back verification failed for %s", before.ID)
	}
	return row, nil
}

// LookupEvolutionEvaluation 按 ID 只读返回评测行。
func (store *Store) LookupEvolutionEvaluation(ctx context.Context, evaluationID string) (memory.Evaluation, error) {
	row, found, err := lookupEvolutionEvaluation(ctx, store.db, evaluationID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if !found {
		return memory.Evaluation{}, memory.ErrNotFound
	}
	return row, nil
}

// LookupEvolutionEvaluationByCandidate 返回候选绑定的唯一评测行；尚无评测
// 时返回 ErrNotFound。
func (store *Store) LookupEvolutionEvaluationByCandidate(ctx context.Context, candidateID string) (memory.Evaluation, error) {
	row, found, err := lookupEvolutionEvaluationForCandidate(ctx, store.db, candidateID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if !found {
		return memory.Evaluation{}, memory.ErrNotFound
	}
	return row, nil
}

// lookupEvolutionEvaluationForCandidate 按候选唯一键读取其绑定的评测行。
func lookupEvolutionEvaluationForCandidate(ctx context.Context, source queryer, candidateID string) (memory.Evaluation, bool, error) {
	const query = `SELECT evaluation_id, schema_version, candidate_id, eval_set_fingerprint,
		baseline_config_fingerprint, variant_config_fingerprint, comparator_version,
		status, suite_result_digest, started_at, finished_at
		FROM evolution_evaluations WHERE candidate_id = ?`
	row, err := scanEvolutionEvaluation(source.QueryRowContext(ctx, query, candidateID))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Evaluation{}, false, nil
	}
	if err != nil {
		return memory.Evaluation{}, false, fmt.Errorf("query evaluation by candidate: %w", err)
	}
	return row, true, nil
}

// lookupEvolutionEvaluation 从事务或连接读取评测行；第二返回值标识存在性。
func lookupEvolutionEvaluation(ctx context.Context, source queryer, evaluationID string) (memory.Evaluation, bool, error) {
	const query = `SELECT evaluation_id, schema_version, candidate_id, eval_set_fingerprint,
		baseline_config_fingerprint, variant_config_fingerprint, comparator_version,
		status, suite_result_digest, started_at, finished_at
		FROM evolution_evaluations WHERE evaluation_id = ?`
	row, err := scanEvolutionEvaluation(source.QueryRowContext(ctx, query, evaluationID))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Evaluation{}, false, nil
	}
	if err != nil {
		return memory.Evaluation{}, false, fmt.Errorf("query evolution evaluation: %w", err)
	}
	return row, true, nil
}

// scanEvolutionEvaluation 把一行评测数据组装成领域结构并校验其合同形态。
func scanEvolutionEvaluation(scanner interface{ Scan(...any) error }) (memory.Evaluation, error) {
	var evaluation memory.Evaluation
	var digest sql.NullString
	var startedRaw, finishedRaw sql.NullString
	err := scanner.Scan(&evaluation.ID, &evaluation.SchemaVersion, &evaluation.CandidateID, &evaluation.EvalSetFingerprint,
		&evaluation.BaselineConfigFingerprint, &evaluation.VariantConfigFingerprint, &evaluation.ComparatorVersion,
		&evaluation.Status, &digest, &startedRaw, &finishedRaw)
	if err != nil {
		return memory.Evaluation{}, err
	}
	startedAt, parseErr := time.Parse(time.RFC3339Nano, startedRaw.String)
	if parseErr != nil {
		return memory.Evaluation{}, fmt.Errorf("parse evaluation started_at: %w", parseErr)
	}
	evaluation.StartedAt = startedAt
	evaluation.SuiteResultDigest = digest.String
	if finishedRaw.Valid {
		finishedAt, parseErr := time.Parse(time.RFC3339Nano, finishedRaw.String)
		if parseErr != nil {
			return memory.Evaluation{}, fmt.Errorf("parse evaluation finished_at: %w", parseErr)
		}
		evaluation.FinishedAt = finishedAt
	}
	if validationErr := evaluation.Validate(); validationErr != nil {
		return memory.Evaluation{}, fmt.Errorf("stored evaluation violates its contract: %w", validationErr)
	}
	return evaluation, nil
}

// evolutionEvaluationRowsMatch 比较一次写入后应持久化的完整评测行：全部
// 闭合字段逐一相等、时间用语义相等（忽略单调时钟），防止触发器静默改写。
func evolutionEvaluationRowsMatch(actual, expected memory.Evaluation) bool {
	return actual.ID == expected.ID && actual.SchemaVersion == expected.SchemaVersion &&
		actual.CandidateID == expected.CandidateID &&
		actual.EvalSetFingerprint == expected.EvalSetFingerprint &&
		actual.BaselineConfigFingerprint == expected.BaselineConfigFingerprint &&
		actual.VariantConfigFingerprint == expected.VariantConfigFingerprint &&
		actual.ComparatorVersion == expected.ComparatorVersion &&
		actual.Status == expected.Status && actual.SuiteResultDigest == expected.SuiteResultDigest &&
		actual.StartedAt.Equal(expected.StartedAt) && actual.FinishedAt.Equal(expected.FinishedAt)
}

// evolutionEvaluationIdempotentEquivalent 判断既有运行中评测与新请求是否为
// 同一冻结输入的重放：五元组不可变字段必须逐一相等；started_at 由首次提交
// 的时钟锚定，同 ID 重试必然携带不同时钟，刻意不参与比较。
func evolutionEvaluationIdempotentEquivalent(existing, incoming memory.Evaluation) (bool, error) {
	incoming.StartedAt = existing.StartedAt
	if err := incoming.Validate(); err != nil {
		return false, fmt.Errorf("incoming evaluation request: %w", err)
	}
	return existing.SchemaVersion == incoming.SchemaVersion &&
		existing.CandidateID == incoming.CandidateID &&
		existing.EvalSetFingerprint == incoming.EvalSetFingerprint &&
		existing.BaselineConfigFingerprint == incoming.BaselineConfigFingerprint &&
		existing.VariantConfigFingerprint == incoming.VariantConfigFingerprint &&
		existing.ComparatorVersion == incoming.ComparatorVersion, nil
}

// stripMonotonic 去除时钟读取的单调分量：落库走 RFC3339Nano 文本，读回只有
// 墙钟语义，比较两侧必须同一形态才能用语义相等判定一致。
func stripMonotonic(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// transitionEvolutionCandidate 在事务内校验并执行候选状态迁移。
func transitionEvolutionCandidate(ctx context.Context, transaction *sql.Tx, candidateID, target string) error {
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	if err != nil {
		return err
	}
	if !found {
		return memory.ErrNotFound
	}
	if err := memory.ValidCandidateTransition(candidate.Status, target); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE evolution_candidates SET status = ? WHERE id = ?", target, candidateID); err != nil {
		return fmt.Errorf("update evolution candidate state: %w", err)
	}
	return nil
}
