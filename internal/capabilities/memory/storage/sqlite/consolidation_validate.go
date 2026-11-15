// [INPUT]: 依赖封存计划与库内原始候选。
// [OUTPUT]: 提供写入前重算和派生结构不变量校验。
// [POS]: 校验失败拒绝事务，不修补历史事实。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package sqlite 提供能力内的SQLite持久化叶子。
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"

	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

// validatePlannedConsolidation 校验计划自洽：写集摘要与输入一致、冻结支持度
// 满足合同下限、派生数量受上限约束；每条派生候选都必须属于本计划并通过合同
// 校验。源候选的存储态与可重算事实由同一 SQLite 事务中的后续校验负责。
func validatePlannedConsolidation(plan memory.PlannedConsolidation) error {
	if plan.SchemaVersion != memory.ConsolidationSchemaVersion {
		return fmt.Errorf("unsupported consolidation schema version %d", plan.SchemaVersion)
	}
	if plan.Strategy != memory.StrategyTaskOutcomeStability && plan.Strategy != memory.StrategyFailureClassRecurrence {
		return fmt.Errorf("unsupported consolidation strategy %q", plan.Strategy)
	}
	if plan.ConsolidationID != memory.ConsolidationID(plan.Strategy, plan.WorkspaceHash, plan.InputsDigest) {
		return errors.New("consolidation id does not match strategy, workspace and inputs digest")
	}
	if memory.InputsDigest(plan.InputIDs) != plan.InputsDigest {
		return errors.New("consolidation inputs digest does not match input ids")
	}
	if !sortedUnique(plan.InputIDs) {
		return errors.New("consolidation input ids must be sorted and unique")
	}
	if plan.FactAnchor.IsZero() {
		return errors.New("consolidation requires a derived fact anchor")
	}
	if plan.MinPatternSupport < memory.PatternMinSupport || plan.MinHypothesisSupport < memory.HypothesisMinSupport {
		return errors.New("consolidation frozen support is below the minimum")
	}
	if len(plan.Derived) == 0 {
		return errors.New("consolidation requires at least one derived candidate")
	}
	if err := validatePlanRetention(plan); err != nil {
		return err
	}
	if len(plan.Derived) > maxDerivedPerConsolidate {
		return fmt.Errorf("consolidation derives %d candidates exceeding the cap %d", len(plan.Derived), maxDerivedPerConsolidate)
	}
	for index, derived := range plan.Derived {
		if derived.ConsolidationID != plan.ConsolidationID {
			return fmt.Errorf("derived candidate %d belongs to a different consolidation", index)
		}
		if derived.WorkspaceHash != plan.WorkspaceHash {
			return fmt.Errorf("derived candidate %d belongs to a different workspace", index)
		}
		if err := derived.Validate(); err != nil {
			return fmt.Errorf("derived candidate %d: %w", index, err)
		}
	}
	return nil
}

// validatePlanRetention 要求计划内全部派生候选共享同一创建锚点与保留期，
// 使巩固事务可以按单一 TTL 值封存并在验证时重算。
func validatePlanRetention(plan memory.PlannedConsolidation) error {
	first := plan.Derived[0]
	for index, derived := range plan.Derived {
		if !derived.CreatedAt.Equal(plan.FactAnchor) || !derived.CreatedAt.Equal(first.CreatedAt) || derived.ExpiresAt.Sub(derived.CreatedAt) != first.ExpiresAt.Sub(first.CreatedAt) {
			return fmt.Errorf("derived candidate %d retention anchor differs within one consolidation", index)
		}
	}
	return nil
}

// validatePlanAgainstStoredInputs 在当前 SQLite 事务内从事实源候选表重新加载
// 计划的所有输入，验证它们仍属于同一工作区、active 且未过期，并以计划冻结
// 的 TTL 与最小支持度重算全部派生事实后作字节级比对。
func validatePlanAgainstStoredInputs(ctx context.Context, transaction *sql.Tx, plan memory.PlannedConsolidation, now time.Time) error {
	if err := validatePlannedConsolidation(plan); err != nil {
		return err
	}
	inputs := memory.ConsolidationInputs{
		WorkspaceHash: plan.WorkspaceHash,
		Candidates:    make([]memory.Candidate, 0, len(plan.InputIDs)),
	}
	for _, candidateID := range plan.InputIDs {
		candidate, found, err := lookupCandidate(ctx, transaction, candidateID)
		if err != nil {
			return fmt.Errorf("load input candidate %s: %w", candidateID, err)
		}
		if !found {
			return fmt.Errorf("input candidate %s: %w", candidateID, memory.ErrNotFound)
		}
		candidate.Evidence, err = evidenceFor(ctx, transaction, candidate.ID)
		if err != nil {
			return fmt.Errorf("load input candidate %s evidence: %w", candidateID, err)
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("validate input candidate %s: %w", candidateID, err)
		}
		if candidate.WorkspaceHash != plan.WorkspaceHash {
			return fmt.Errorf("input candidate %s belongs to a different workspace", candidateID)
		}
		if candidate.Status != memory.StatusActive {
			return fmt.Errorf("input candidate %s is not active", candidateID)
		}
		if !candidate.ExpiresAt.After(now) {
			return fmt.Errorf("input candidate %s is expired", candidateID)
		}
		inputs.Candidates = append(inputs.Candidates, candidate)
	}
	for _, derived := range plan.Derived {
		if err := validateDerivedEvidenceSubset(derived, plan.InputIDs); err != nil {
			return err
		}
	}
	recomputed, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{
		Now:                  now,
		TTL:                  plan.Derived[0].ExpiresAt.Sub(plan.Derived[0].CreatedAt),
		MinPatternSupport:    plan.MinPatternSupport,
		MinHypothesisSupport: plan.MinHypothesisSupport,
	})
	if err != nil {
		return fmt.Errorf("recompute consolidation facts: %w", err)
	}
	for _, candidate := range recomputed {
		if candidate.Strategy == plan.Strategy {
			if !plannedFactsEquivalent(candidate, plan) {
				return errors.New("planned consolidation does not match recomputed facts")
			}
			return nil
		}
	}
	return errors.New("planned consolidation strategy is not reproducible from inputs")
}

// validateDerivedEvidenceSubset 要求派生候选的每个间接证据都声明在计划输入
// 集内，避免计划以未重算的外部候选伪造支持度。
func validateDerivedEvidenceSubset(derived memory.DerivedCandidate, inputIDs []string) error {
	allowed := make(map[string]struct{}, len(inputIDs))
	for _, candidateID := range inputIDs {
		allowed[candidateID] = struct{}{}
	}
	for _, reference := range derived.Evidence {
		if _, exists := allowed[reference.CandidateID]; !exists {
			return fmt.Errorf("derived candidate %s evidence is outside plan inputs", derived.ID)
		}
	}
	return nil
}

// plannedFactsEquivalent 比较计划的所有冻结事实；实际提交时间不属于计划，
// 因为它必须由写入事务的真实 UTC 时钟生成并持久化。
func plannedFactsEquivalent(left, right memory.PlannedConsolidation) bool {
	return left.ConsolidationID == right.ConsolidationID && left.SchemaVersion == right.SchemaVersion &&
		left.Strategy == right.Strategy && left.WorkspaceHash == right.WorkspaceHash &&
		left.InputsDigest == right.InputsDigest && reflect.DeepEqual(left.InputIDs, right.InputIDs) &&
		left.FactAnchor.Equal(right.FactAnchor) && left.MinPatternSupport == right.MinPatternSupport &&
		left.MinHypothesisSupport == right.MinHypothesisSupport && reflect.DeepEqual(left.Derived, right.Derived)
}

// sortedUnique 判断输入标识是否按稳定字典序有序且没有重复，避免摘要的集合语义
// 被重复 ID 掩盖而绕过同事务逐项加载。
func sortedUnique(values []string) bool {
	if len(values) == 0 {
		return false
	}
	return sort.SliceIsSorted(values, func(left, right int) bool {
		return values[left] < values[right]
	}) && !hasDuplicate(values)
}

// hasDuplicate 在已经排序的输入标识中检查相邻重复项。
func hasDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] == values[index] {
			return true
		}
	}
	return false
}

// checkDerivedInvariants 验证派生候选与巩固事务的结构不变量：外键、载荷、
// 同 id 唯一 active、替代链、审计时间线，以及按输入重算的派生事实一致性。
func checkDerivedInvariants(ctx context.Context, source queryerRows) error {
	consolidations, err := storeListConsolidations(ctx, source)
	if err != nil {
		return err
	}
	consolidationByID := make(map[string]StoredConsolidation, len(consolidations))
	for _, stored := range consolidations {
		if stored.SchemaVersion != memory.ConsolidationSchemaVersion || stored.Status != memory.ConsolidationStatusCommitted {
			return fmt.Errorf("consolidation %s has an unsupported schema or status", stored.ID)
		}
		if stored.ID != memory.ConsolidationID(stored.Strategy, stored.WorkspaceHash, stored.InputsDigest) || memory.InputsDigest(stored.InputIDs) != stored.InputsDigest || len(stored.InputIDs) != stored.InputCount {
			return fmt.Errorf("consolidation %s does not match its declared inputs", stored.ID)
		}
		if !sortedUnique(stored.InputIDs) || stored.MinPatternSupport < memory.PatternMinSupport || stored.MinHypothesisSupport < memory.HypothesisMinSupport {
			return fmt.Errorf("consolidation %s has invalid frozen parameters", stored.ID)
		}
		consolidationByID[stored.ID] = stored
	}
	derived, err := storeListDerived(ctx, source)
	if err != nil {
		return err
	}
	versionByKey := make(map[DerivedVersionKey]memory.DerivedCandidate, len(derived))
	for _, candidate := range derived {
		key := DerivedVersionKey{ID: candidate.ID, ConsolidationID: candidate.ConsolidationID}
		if _, exists := versionByKey[key]; exists {
			return fmt.Errorf("derived candidate version %s/%s is duplicated", key.ID, key.ConsolidationID)
		}
		versionByKey[key] = candidate
	}
	activeCounts := make(map[string]int)
	successorCounts := make(map[DerivedVersionKey]int)
	for _, candidate := range derived {
		if _, exists := consolidationByID[candidate.ConsolidationID]; !exists {
			return fmt.Errorf("derived candidate %s references a missing consolidation", candidate.ID)
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("validate derived candidate: %w", err)
		}
		for _, reference := range candidate.Evidence {
			sourceCandidate, found, err := lookupCandidate(ctx, source, reference.CandidateID)
			if err != nil || !found {
				return fmt.Errorf("derived candidate %s references missing source %s: %v", candidate.ID, reference.CandidateID, err)
			}
			if sourceCandidate.WorkspaceHash != candidate.WorkspaceHash {
				return fmt.Errorf("derived candidate %s evidence crosses workspaces", candidate.ID)
			}
		}
		if candidate.Status == memory.DerivedStatusActive {
			activeCounts[candidate.ID]++
		}
		if candidate.SupersedesID != "" {
			predecessorKey := DerivedVersionKey{ID: candidate.SupersedesID, ConsolidationID: candidate.SupersedesConsolidationID}
			predecessor, found := versionByKey[predecessorKey]
			if !found || (predecessorKey.ID == candidate.ID && predecessorKey.ConsolidationID == candidate.ConsolidationID) ||
				predecessor.Status != memory.DerivedStatusSuperseded || predecessor.WorkspaceHash != candidate.WorkspaceHash ||
				predecessor.Kind != candidate.Kind || predecessor.ScopeDigest != candidate.ScopeDigest {
				return fmt.Errorf("derived candidate %s has an invalid predecessor version key", candidate.ID)
			}
			successorCounts[predecessorKey]++
		}
		audit, err := derivedAuditFor(ctx, source, candidate.ID, candidate.ConsolidationID)
		if err != nil {
			return err
		}
		consolidated, rejected, superseded := 0, 0, 0
		stored := consolidationByID[candidate.ConsolidationID]
		for _, entry := range audit {
			if entry.OccurredAt.Before(candidate.CreatedAt) {
				return fmt.Errorf("derived candidate %s has an invalid audit timeline", candidate.ID)
			}
			switch entry.Action {
			case memory.DerivedAuditConsolidated:
				if !entry.OccurredAt.Equal(stored.CommittedAt) || entry.Reason != "" {
					return fmt.Errorf("derived candidate %s has an invalid consolidation audit", candidate.ID)
				}
				consolidated++
			case memory.DerivedAuditRejected:
				if !memory.ValidRejectionReason(entry.Reason) {
					return fmt.Errorf("derived candidate %s has an invalid rejection audit", candidate.ID)
				}
				rejected++
			case memory.DerivedAuditSuperseded:
				superseded++
			default:
				return fmt.Errorf("derived candidate %s has an unknown audit action", candidate.ID)
			}
		}
		if consolidated != 1 {
			return fmt.Errorf("derived candidate %s must have exactly one consolidation audit", candidate.ID)
		}
		switch candidate.Status {
		case memory.DerivedStatusActive:
			if rejected != 0 || superseded != 0 {
				return fmt.Errorf("active derived candidate %s has terminal audit", candidate.ID)
			}
		case memory.DerivedStatusRejected:
			if rejected != 1 || superseded != 0 {
				return fmt.Errorf("rejected derived candidate %s has invalid audit state", candidate.ID)
			}
		case memory.DerivedStatusSuperseded:
			if rejected != 0 || superseded != 1 {
				return fmt.Errorf("superseded derived candidate %s has invalid audit state", candidate.ID)
			}
		}
	}
	for id, count := range activeCounts {
		if count > 1 {
			return fmt.Errorf("derived candidate %s has %d active versions", id, count)
		}
	}
	for _, candidate := range derived {
		if candidate.Status == memory.DerivedStatusSuperseded && successorCounts[DerivedVersionKey{ID: candidate.ID, ConsolidationID: candidate.ConsolidationID}] != 1 {
			return fmt.Errorf("superseded derived candidate %s must have exactly one successor version", candidate.ID)
		}
	}
	return checkDerivedFactsRecompute(ctx, source, consolidations)
}

// checkDerivedFactsRecompute 按每个巩固事务声明的输入候选重算确定性归纳，
// 并与已派生事实比对；存储的载荷一旦与源事实漂移即失败关闭。
func checkDerivedFactsRecompute(ctx context.Context, source queryerRows, consolidations []StoredConsolidation) error {
	for _, stored := range consolidations {
		inputs := memory.ConsolidationInputs{WorkspaceHash: stored.WorkspaceHash, Candidates: make([]memory.Candidate, 0, len(stored.InputIDs))}
		for _, candidateID := range stored.InputIDs {
			candidate, found, err := lookupCandidate(ctx, source, candidateID)
			if err == nil && !found {
				err = memory.ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("consolidation %s input candidate %s is missing: %v", stored.ID, candidateID, err)
			}
			candidate.Evidence, err = evidenceFor(ctx, source, candidate.ID)
			if err != nil {
				return err
			}
			inputs.Candidates = append(inputs.Candidates, candidate)
		}
		recomputed, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: stored.CommittedAt, TTL: stored.DerivedTTL, MinPatternSupport: stored.MinPatternSupport, MinHypothesisSupport: stored.MinHypothesisSupport})
		if err != nil {
			return fmt.Errorf("recompute consolidation %s: %w", stored.ID, err)
		}
		derived, err := derivedForConsolidation(ctx, source, stored.ID)
		if err != nil {
			return err
		}
		expected := make([]memory.DerivedCandidate, 0)
		for _, plan := range recomputed {
			if plan.Strategy == stored.Strategy {
				expected = append(expected, plan.Derived...)
			}
		}
		if len(expected) != len(derived) {
			return fmt.Errorf("consolidation %s derived %d candidates but facts recompute to %d", stored.ID, len(derived), len(expected))
		}
		expectedByID := make(map[string]memory.DerivedCandidate, len(expected))
		for _, candidate := range expected {
			expectedByID[candidate.ID] = candidate
		}
		for _, candidate := range derived {
			expectedCandidate, exists := expectedByID[candidate.ID]
			if !exists {
				return fmt.Errorf("consolidation %s derived candidate %s is not reproducible from inputs", stored.ID, candidate.ID)
			}
			if expectedCandidate.Kind != candidate.Kind || expectedCandidate.ScopeDigest != candidate.ScopeDigest ||
				expectedCandidate.WorkspaceHash != candidate.WorkspaceHash || string(expectedCandidate.Payload) != string(candidate.Payload) ||
				!expectedCandidate.CreatedAt.Equal(candidate.CreatedAt) || !expectedCandidate.ExpiresAt.Equal(candidate.ExpiresAt) ||
				!reflect.DeepEqual(expectedCandidate.Evidence, candidate.Evidence) {
				return fmt.Errorf("consolidation %s derived candidate %s does not match recomputed facts", stored.ID, candidate.ID)
			}
		}
	}
	return nil
}
