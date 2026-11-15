// [INPUT]: 依赖真实SQLite与父能力结构化合同。
// [OUTPUT]: 提供演化写集读回失配的零残留回归。
// [POS]: 能力存储回归，保留原全部安全与恢复断言。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package sqlite

import (
	"context"

	"testing"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

// TestEvolutionRejectReadBackFailureLeavesNoResidue 锁定拒绝路径的读回
// 零残留：触发器追加隐藏终态审计，使精确审计读回捕获不一致并整体回滚，
// 候选、审计与冷却行全部零残留。
func TestEvolutionRejectReadBackFailureLeavesNoResidue(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER append_reject_audit AFTER INSERT ON evolution_audit_log WHEN NEW.action = 'rejected' BEGIN INSERT INTO evolution_audit_log(candidate_id, action, reason, occurred_at) VALUES (NEW.candidate_id, 'invalidated', NULL, NEW.occurred_at); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now.Add(time.Minute), 0); err == nil {
		t.Fatal("extra audit must fail the exact read-back and roll back rejection")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER append_reject_audit"); err != nil {
		t.Fatal(err)
	}
	inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft || len(inspected.Audit) != 1 {
		t.Fatalf("rejection must leave no residue: %#v err=%v", inspected, err)
	}
	var cooldownRows int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_cooldowns").Scan(&cooldownRows); err != nil {
		t.Fatal(err)
	}
	if cooldownRows != 0 {
		t.Fatalf("rejected rollback must not persist cooldown rows, got %d", cooldownRows)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestEvolutionCreateRejectsMissingAuditReadBack 锁定创建路径的审计读回：
// 创建审计被触发器删除时，候选写入必须整体回滚而不能留下孤立状态行。
func TestEvolutionCreateRejectsMissingAuditReadBack(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER erase_created_audit AFTER INSERT ON evolution_audit_log WHEN NEW.action = 'created' BEGIN DELETE FROM evolution_audit_log WHERE candidate_id = NEW.candidate_id AND action = 'created'; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err == nil {
		t.Fatal("missing creation audit must fail the read-back")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER erase_created_audit"); err != nil {
		t.Fatal(err)
	}
	var candidateRows, auditRows int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_candidates").Scan(&candidateRows); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_audit_log").Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if candidateRows != 0 || auditRows != 0 {
		t.Fatalf("creation read-back failure must leave no residue: candidates=%d audits=%d", candidateRows, auditRows)
	}
}

// TestEvolutionTerminalTransitionsRejectExtraAuditReadBack 锁定作废和到期路径
// 的精确审计读回；任意额外终态审计都必须使整笔状态迁移回滚。
func TestEvolutionTerminalTransitionsRejectExtraAuditReadBack(t *testing.T) {
	tests := []struct {
		name   string
		action string
		run    func(*Store, memory.EvolutionCandidate, time.Time) error
	}{
		{
			name:   "invalidated",
			action: memory.EvolutionAuditInvalidated,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusRejected, candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
					return err
				}
				_, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now)
				return err
			},
		},
		{
			name:   "expired",
			action: memory.EvolutionAuditExpired,
			run: func(store *Store, candidate memory.EvolutionCandidate, _ time.Time) error {
				_, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt)
				return err
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			store, candidate, now := evolutionFixture(t)
			if testCase.name == "expired" {
				candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
			}
			if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
				t.Fatal(err)
			}
			triggerName := "append_" + testCase.name + "_audit"
			statement := "CREATE TEMP TRIGGER " + triggerName + " AFTER INSERT ON evolution_audit_log WHEN NEW.action = '" + testCase.action + "' BEGIN INSERT INTO evolution_audit_log(candidate_id, action, reason, occurred_at) VALUES (NEW.candidate_id, '" + testCase.action + "', NULL, NEW.occurred_at); END"
			if _, err := store.db.ExecContext(context.Background(), statement); err != nil {
				t.Fatal(err)
			}
			if err := testCase.run(store, candidate, now); err == nil {
				t.Fatal("extra terminal audit must fail the exact read-back")
			}
			if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER "+triggerName); err != nil {
				t.Fatal(err)
			}
			inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
			if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft || len(inspected.Audit) != 1 {
				t.Fatalf("terminal transition must leave no residue: %#v err=%v", inspected, err)
			}
		})
	}
}

// TestEvolutionWriteReadBackRejectsCandidateMutation 为四条写路径注入状态与
// 等幅时间篡改。任何逐字段候选读回失配都必须在提交前整体回滚。
func TestEvolutionWriteReadBackRejectsCandidateMutation(t *testing.T) {
	tests := []struct {
		name   string
		action string
		run    func(*Store, memory.EvolutionCandidate, time.Time) error
	}{
		{
			name:   "create",
			action: memory.EvolutionAuditCreated,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.CreateEvolutionCandidate(context.Background(), candidate, now)
				return err
			},
		},
		{
			name:   "reject",
			action: memory.EvolutionAuditRejected,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0)
				return err
			},
		},
		{
			name:   "invalidated",
			action: memory.EvolutionAuditInvalidated,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now)
				return err
			},
		},
		{
			name:   "expired",
			action: memory.EvolutionAuditExpired,
			run: func(store *Store, candidate memory.EvolutionCandidate, _ time.Time) error {
				_, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt)
				return err
			},
		},
	}
	for _, testCase := range tests {
		for _, mutation := range []string{"status", "time"} {
			t.Run(testCase.name+"_"+mutation, func(t *testing.T) {
				store, candidate, now := evolutionFixture(t)
				if testCase.name == "expired" {
					candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
				}
				if testCase.name != "create" {
					if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
						t.Fatal(err)
					}
				}
				if testCase.name == "invalidated" {
					mutateEvolutionSource(t, store, candidate, now, evolutionSourceInactive)
				}
				triggerName := "mutate_" + testCase.name + "_" + mutation
				statement := evolutionCandidateMutationTrigger(triggerName, testCase.name, testCase.action, mutation, candidate)
				if _, err := store.db.ExecContext(context.Background(), statement); err != nil {
					t.Fatal(err)
				}
				if err := testCase.run(store, candidate, now); err == nil {
					t.Fatal("candidate mutation must fail exact read-back")
				}
				if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER "+triggerName); err != nil {
					t.Fatal(err)
				}
				if testCase.name == "create" {
					var candidateRows, auditRows int
					if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_candidates").Scan(&candidateRows); err != nil {
						t.Fatal(err)
					}
					if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_audit_log").Scan(&auditRows); err != nil {
						t.Fatal(err)
					}
					if candidateRows != 0 || auditRows != 0 {
						t.Fatalf("create mutation must leave no residue: candidates=%d audits=%d", candidateRows, auditRows)
					}
					return
				}
				assertEvolutionDraftOnly(t, store, candidate.ID)
				if evolutionCooldownRowCount(t, store) != 0 {
					t.Fatal("failed transition must not persist cooldown")
				}
			})
		}
	}
}

// TestEvolutionRejectReadBackRejectsCooldownMutation 锁定拒绝写集的最后一项：
// 冷却行被触发器改写后，候选、审计与冷却必须作为单一事务全部回滚。
func TestEvolutionRejectReadBackRejectsCooldownMutation(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER mutate_reject_cooldown AFTER INSERT ON evolution_cooldowns BEGIN UPDATE evolution_cooldowns SET cooldown_until = NEW.cooldown_until || '.tampered' WHERE source_derived_id = NEW.source_derived_id AND source_consolidation_id = NEW.source_consolidation_id AND target_kind = NEW.target_kind AND variant_fingerprint = NEW.variant_fingerprint; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0); err == nil {
		t.Fatal("cooldown mutation must fail exact read-back")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER mutate_reject_cooldown"); err != nil {
		t.Fatal(err)
	}
	assertEvolutionDraftOnly(t, store, candidate.ID)
	if evolutionCooldownRowCount(t, store) != 0 {
		t.Fatal("cooldown read-back failure must leave no cooldown residue")
	}
}
