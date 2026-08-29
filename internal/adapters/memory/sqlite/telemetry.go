// 本文件承载记忆体系聚合遥测：召回调用的聚合事实采集（唯一写入面）与跨
// 表只读快照。路线图可观测性合同（§8）的硬边界：只记录聚合指标，零正文、
// 零路径、零密钥；错误记忆引用率因召回不写回而合同上不可观测，不做伪指
// 标；索引可重建率随 Zvec HOLD 暂缺。
//
// Author: fxbin
package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RecallTelemetryRecord 是一次召回调用的聚合事实；调用方只提供计数与策
// 略，不含任何查询正文或候选正文。
type RecallTelemetryRecord struct {
	OccurredAt           time.Time
	CandidatesReturned   int
	Conflicts            int
	EvidenceInsufficient bool
	PlanStrategy         string
}

// RecordRecallTelemetry 把一行召回聚合事实写入采集表；写入失败原样返回错
// 误，由调用方决定降级（遥测绝不影响召回结果本身）。
func (store *Store) RecordRecallTelemetry(ctx context.Context, record RecallTelemetryRecord) error {
	occurredAt := record.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	strategy := strings.TrimSpace(record.PlanStrategy)
	if strategy == "" {
		strategy = "unspecified"
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO recall_telemetry(
		occurred_at, candidates_returned, conflicts, evidence_insufficient, plan_strategy
	) VALUES (?, ?, ?, ?, ?)`,
		occurredAt.UTC().Format(time.RFC3339Nano), record.CandidatesReturned,
		record.Conflicts, boolToInt(record.EvidenceInsufficient), strategy)
	if err != nil {
		return fmt.Errorf("record recall telemetry: %w", err)
	}
	return nil
}

// RecallTelemetryAggregate 是召回遥测的聚合投影。
type RecallTelemetryAggregate struct {
	TotalCalls               int     `json:"total_calls"`
	AverageCandidates        float64 `json:"average_candidates"`
	ConflictRate             float64 `json:"conflict_rate"`
	EvidenceInsufficientRate float64 `json:"evidence_insufficient_rate"`
}

// TelemetrySnapshot 是记忆体系跨表聚合的一次只读快照：全部数字可由各表
// 独立复核。
type TelemetrySnapshot struct {
	GeneratedAt   time.Time                `json:"generated_at"`
	Memory        MemoryTelemetry          `json:"memory"`
	Consolidation ConsolidationTelemetry   `json:"consolidation"`
	Evolution     EvolutionTelemetry       `json:"evolution"`
	Recall        RecallTelemetryAggregate `json:"recall"`
}

// MemoryTelemetry 是情节记忆侧的聚合。
type MemoryTelemetry struct {
	CandidatesTotal      int `json:"candidates_total"`
	CandidatesActive     int `json:"candidates_active"`
	CandidatesRevoked    int `json:"candidates_revoked"`
	CandidatesSuperseded int `json:"candidates_superseded"`
}

// ConsolidationTelemetry 是巩固侧的聚合。
type ConsolidationTelemetry struct {
	ConsolidationsCommitted int `json:"consolidations_committed"`
	DerivedTotal            int `json:"derived_total"`
	DerivedRejected         int `json:"derived_rejected"`
}

// EvolutionTelemetry 是演化侧的聚合：候选九态分布、评测五态分布、版本状
// 态与回退耗时序列（审计时间对：sandbox_activated → promotion_rolled_back）。
type EvolutionTelemetry struct {
	CandidatesByStatus  map[string]int `json:"candidates_by_status"`
	EvaluationsByStatus map[string]int `json:"evaluations_by_status"`
	PromotionsActive    int            `json:"promotions_active"`
	PromotionsRevoked   int            `json:"promotions_revoked"`
	RollbackDurationsMS []int64        `json:"rollback_durations_ms"`
	CooldownsActive     int            `json:"cooldowns_active"`
}

// TelemetrySnapshot 在单一只读连接上聚合全部记忆侧指标。
func (store *Store) TelemetrySnapshot(ctx context.Context) (*TelemetrySnapshot, error) {
	snapshot := &TelemetrySnapshot{GeneratedAt: time.Now().UTC(), Evolution: EvolutionTelemetry{CandidatesByStatus: map[string]int{}, EvaluationsByStatus: map[string]int{}}}
	if err := store.telemetryMemory(ctx, snapshot); err != nil {
		return nil, err
	}
	if err := store.telemetryConsolidation(ctx, snapshot); err != nil {
		return nil, err
	}
	if err := store.telemetryEvolution(ctx, snapshot); err != nil {
		return nil, err
	}
	if err := store.telemetryRecall(ctx, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// telemetryMemory 聚合情节记忆候选状态分布。
func (store *Store) telemetryMemory(ctx context.Context, snapshot *TelemetrySnapshot) error {
	rows, err := store.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM candidates GROUP BY status")
	if err != nil {
		return fmt.Errorf("query candidate telemetry: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		switch status {
		case "active":
			snapshot.Memory.CandidatesActive = count
		case "revoked":
			snapshot.Memory.CandidatesRevoked = count
		case "superseded":
			snapshot.Memory.CandidatesSuperseded = count
		}
		snapshot.Memory.CandidatesTotal += count
	}
	return rows.Err()
}

// telemetryConsolidation 聚合巩固提交与派生拒绝。
func (store *Store) telemetryConsolidation(ctx context.Context, snapshot *TelemetrySnapshot) error {
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM consolidations").Scan(&snapshot.Consolidation.ConsolidationsCommitted); err != nil {
		return fmt.Errorf("count consolidations: %w", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM derived_candidates").Scan(&snapshot.Consolidation.DerivedTotal); err != nil {
		return fmt.Errorf("count derived candidates: %w", err)
	}
	return store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM derived_audit_log WHERE action = 'rejected'").Scan(&snapshot.Consolidation.DerivedRejected)
}

// telemetryEvolution 聚合演化侧全部分布与回退耗时。
func (store *Store) telemetryEvolution(ctx context.Context, snapshot *TelemetrySnapshot) error {
	rows, err := store.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM evolution_candidates GROUP BY status")
	if err != nil {
		return fmt.Errorf("query evolution candidate telemetry: %w", err)
	}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return err
		}
		snapshot.Evolution.CandidatesByStatus[status] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	evaluationRows, err := store.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM evolution_evaluations GROUP BY status")
	if err != nil {
		return fmt.Errorf("query evaluation telemetry: %w", err)
	}
	for evaluationRows.Next() {
		var status string
		var count int
		if err := evaluationRows.Scan(&status, &count); err != nil {
			evaluationRows.Close()
			return err
		}
		snapshot.Evolution.EvaluationsByStatus[status] = count
	}
	if err := evaluationRows.Err(); err != nil {
		evaluationRows.Close()
		return err
	}
	evaluationRows.Close()

	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM evolution_promotions WHERE status = 'active'").Scan(&snapshot.Evolution.PromotionsActive); err != nil {
		return fmt.Errorf("count active promotions: %w", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM evolution_promotions WHERE status = 'revoked'").Scan(&snapshot.Evolution.PromotionsRevoked); err != nil {
		return fmt.Errorf("count revoked promotions: %w", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM evolution_cooldowns WHERE cooldown_until > ?",
		time.Now().UTC().Format(time.RFC3339Nano)).Scan(&snapshot.Evolution.CooldownsActive); err != nil {
		return fmt.Errorf("count active cooldowns: %w", err)
	}
	return store.telemetryRollbackDurations(ctx, snapshot)
}

// telemetryRollbackDurations 以审计时间对计算每次版本回退耗时：
// promotion_rolled_back 的 occurred_at 减同候选最近一次 sandbox_activated
// 的 occurred_at。激活指针行会被回退覆盖，历史耗时只能来自审计。
func (store *Store) telemetryRollbackDurations(ctx context.Context, snapshot *TelemetrySnapshot) error {
	rows, err := store.db.QueryContext(ctx, `SELECT candidate_id, action, occurred_at FROM evolution_audit_log
		WHERE action IN ('sandbox_activated', 'promotion_rolled_back') ORDER BY candidate_id ASC, id ASC`)
	if err != nil {
		return fmt.Errorf("query rollback audit pairs: %w", err)
	}
	defer rows.Close()
	var lastActivated time.Time
	var lastCandidate string
	for rows.Next() {
		var candidateID, action, occurredAtRaw string
		if err := rows.Scan(&candidateID, &action, &occurredAtRaw); err != nil {
			return err
		}
		occurredAt, parseErr := time.Parse(time.RFC3339Nano, occurredAtRaw)
		if parseErr != nil {
			return fmt.Errorf("parse audit time: %w", parseErr)
		}
		if candidateID != lastCandidate {
			lastCandidate, lastActivated = candidateID, time.Time{}
		}
		if action == "sandbox_activated" {
			lastActivated = occurredAt
			continue
		}
		if !lastActivated.IsZero() && !occurredAt.Before(lastActivated) {
			snapshot.Evolution.RollbackDurationsMS = append(snapshot.Evolution.RollbackDurationsMS, occurredAt.Sub(lastActivated).Milliseconds())
			lastActivated = time.Time{}
		}
	}
	return rows.Err()
}

// telemetryRecall 聚合召回遥测：总调用、平均命中、冲突率与证据不足率。
func (store *Store) telemetryRecall(ctx context.Context, snapshot *TelemetrySnapshot) error {
	var totalCalls, totalCandidates, totalConflicts, totalInsufficient int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM recall_telemetry").Scan(&totalCalls); err != nil {
		return fmt.Errorf("count recall telemetry: %w", err)
	}
	if totalCalls == 0 {
		return nil
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(candidates_returned),0), COALESCE(SUM(conflicts),0), COALESCE(SUM(evidence_insufficient),0) FROM recall_telemetry").Scan(
		&totalCandidates, &totalConflicts, &totalInsufficient); err != nil {
		return fmt.Errorf("sum recall telemetry: %w", err)
	}
	snapshot.Recall = RecallTelemetryAggregate{
		TotalCalls:               totalCalls,
		AverageCandidates:        float64(totalCandidates) / float64(totalCalls),
		ConflictRate:             float64(totalConflicts) / float64(totalCalls),
		EvidenceInsufficientRate: float64(totalInsufficient) / float64(totalCalls),
	}
	return nil
}

// boolToInt 折叠布尔为整数计数。
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
