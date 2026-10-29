package decision

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubResolver 是证据解析器的只读测试替身：按稳定键集合判定证据是否存在，
// 用于验证引用可核查性与错误引用发现，不接触任何真实存储。
type stubResolver struct {
	known map[string]string
}

// Resolve 按证据稳定键返回解析结论；未知证据视为不存在。
func (resolver stubResolver) Resolve(_ context.Context, ref EvidenceRef) (EvidenceResolution, error) {
	digest, found := resolver.known[ref.Key()]
	if !found {
		return EvidenceResolution{}, nil
	}
	return EvidenceResolution{Found: true, Digest: digest}, nil
}

// fixtureApproval 返回一次人工确认凭据。
func fixtureApproval() HumanApproval {
	approval, err := NewHumanApproval(fixtureApprover, fixtureTime.Add(time.Hour))
	if err != nil {
		panic(err)
	}
	return approval
}

// TestLedgerProposeConfirmRevokeKeepsHistory 锁定撤销语义：记录被撤销后不
// 再代表现行事实，但记录本身与审计链都保留，历史可追溯。
func TestLedgerProposeConfirmRevokeKeepsHistory(t *testing.T) {
	ledger := NewLedger()
	proposal := fixtureProposal()
	proposed, err := ledger.Propose(proposal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proposed.Status != StatusProposed {
		t.Fatalf("expected a proposed candidate, got %s", proposed.Status)
	}
	confirmed, err := ledger.Confirm(proposed.ID, fixtureApproval())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if confirmed.Status != StatusActive || confirmed.ReviewedAt == nil {
		t.Fatalf("expected an active record with a review time, got %s", confirmed.Status)
	}
	revoked, err := ledger.Revoke(confirmed.ID, RevokeReasonEvidenceInvalid, fixtureApprover, fixtureTime.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if revoked.Status != StatusRevoked {
		t.Fatalf("expected a revoked record, got %s", revoked.Status)
	}
	if revoked.Decision != confirmed.Decision {
		t.Fatal("expected the revoked record to keep its original statement")
	}
	audits := ledger.Audits(confirmed.ID)
	if len(audits) != 3 {
		t.Fatalf("expected three audit entries, got %d", len(audits))
	}
	if audits[0].Action != ActionProposed || audits[1].Action != ActionConfirmed || audits[2].Action != ActionRevoked {
		t.Fatalf("unexpected audit chain: %+v", audits)
	}
	if audits[2].Reason != RevokeReasonEvidenceInvalid {
		t.Fatalf("expected the revocation reason to be kept, got %q", audits[2].Reason)
	}
	if _, err := ledger.Revoke(confirmed.ID, RevokeReasonEvidenceInvalid, fixtureApprover, fixtureTime.Add(3*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected a revoked record to reject a second revocation, got %v", err)
	}
	if _, err := ledger.Revoke(confirmed.ID, "because_i_said_so", fixtureApprover, fixtureTime.Add(3*time.Hour)); err == nil {
		t.Fatal("expected an unknown revocation reason to be rejected")
	}
}

// TestLedgerSupersedeReplacesActiveDecision 锁定替代语义：新决策确认后旧
// 决策转为已被替代，两条记录与作废链都保留在账本中。
func TestLedgerSupersedeReplacesActiveDecision(t *testing.T) {
	ledger := NewLedger()
	original, err := ledger.Propose(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(original.ID, fixtureApproval()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	successor := fixtureProposal()
	successor.Decision = "晋升必须经人工确认并保留来源与作废链"
	successor.Rationale = "缺少作废链会让被替代结论继续被引用"
	successor.Supersedes = original.ID
	successor.Now = fixtureTime.Add(time.Hour)
	proposed, err := ledger.Propose(successor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(proposed.ID, fixtureApproval()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	replaced, err := ledger.Get(original.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if replaced.Status != StatusSuperseded {
		t.Fatalf("expected the original record to be superseded, got %s", replaced.Status)
	}
	if replaced.EffectiveStatus(fixtureTime.Add(2*time.Hour)) == StatusActive {
		t.Fatal("a superseded record must never stay current")
	}
	audits := ledger.Audits(original.ID)
	if len(audits) != 3 || audits[2].Action != ActionSuperseded || audits[2].Reason != proposed.ID {
		t.Fatalf("expected the supersession chain to name the successor, got %+v", audits)
	}
}

// TestLedgerSupersedeRejectsCrossProjectTarget 锁定作用域：不得跨项目替代
// 既有决策。检查落在最早的写入点，替代声明连录入都不允许。
func TestLedgerSupersedeRejectsCrossProjectTarget(t *testing.T) {
	ledger := NewLedger()
	original, err := ledger.Propose(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(original.ID, fixtureApproval()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	successor := fixtureProposal()
	successor.Project = "proj_other"
	successor.Owner = "user_other"
	successor.Decision = "跨项目替代尝试"
	successor.Supersedes = original.ID
	successor.Now = fixtureTime.Add(time.Hour)
	if _, err := ledger.Propose(successor); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected a cross-project supersession to be rejected, got %v", err)
	}
	replaced, err := ledger.Get(original.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if replaced.Status != StatusActive {
		t.Fatalf("expected the original record to stay active, got %s", replaced.Status)
	}
}

// TestLedgerQueryScopesProjectAndPrincipal 锁定只读查询的边界：跨项目与
// 跨用户的记录一律不返回。
func TestLedgerQueryScopesProjectAndPrincipal(t *testing.T) {
	ledger := NewLedger()
	authoritative := fixtureProposal()
	authoritative.Now = fixtureTime
	if _, err := ledger.Propose(authoritative); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	preference := fixtureProposal()
	preference.Kind = KindPreference
	preference.Owner = "user_private"
	preference.Decision = "个人偏好：优先阅读中文注释"
	preference.Now = fixtureTime
	if _, err := ledger.Propose(preference); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foreign := fixtureProposal()
	foreign.Project = "proj_other"
	foreign.Owner = "user_other"
	foreign.Decision = "另一个项目的决策"
	foreign.Now = fixtureTime
	if _, err := ledger.Propose(foreign); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	records, err := ledger.Query(Query{Project: fixtureProject, Principal: "user_private", Now: fixtureTime.Add(time.Hour)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected two visible records, got %d", len(records))
	}
	for _, record := range records {
		if record.Project != fixtureProject {
			t.Fatalf("a foreign project record leaked into the result: %s", record.ID)
		}
	}
	other, err := ledger.Query(Query{Project: fixtureProject, Principal: "user_other", Now: fixtureTime.Add(time.Hour)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(other) != 1 {
		t.Fatalf("expected only the project decision to be visible, got %d", len(other))
	}
	if other[0].Kind != KindAuthoritative {
		t.Fatalf("expected the personal preference to stay hidden, got %s", other[0].Kind)
	}
}

// TestLedgerQueryHidesExpiredRecords 锁定过期不冒充现行事实：过期记录默认
// 不进入查询结果，只有显式要求历史追溯时才可见。
func TestLedgerQueryHidesExpiredRecords(t *testing.T) {
	ledger := NewLedger()
	proposal := fixtureProposal()
	proposal.TTL = time.Hour
	record, err := ledger.Propose(proposal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	approval, err := NewHumanApproval(fixtureApprover, fixtureTime.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(record.ID, approval); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	current, err := ledger.Query(Query{Project: fixtureProject, Principal: fixtureOwner, Now: fixtureTime.Add(30 * time.Minute)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(current) != 1 {
		t.Fatalf("expected the record to be visible before expiry, got %d", len(current))
	}
	expired, err := ledger.Query(Query{Project: fixtureProject, Principal: fixtureOwner, Now: fixtureTime.Add(2 * time.Hour)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("expected no expired record in the current view, got %d", len(expired))
	}
	history, err := ledger.Query(Query{Project: fixtureProject, Principal: fixtureOwner, Now: fixtureTime.Add(2 * time.Hour), IncludeExpired: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 1 || history[0].EffectiveStatus(fixtureTime.Add(2*time.Hour)) != StatusExpired {
		t.Fatalf("expected the expired record to stay traceable, got %+v", history)
	}
}

// TestLedgerRejectsDuplicateActiveDecision 锁定等价决策：同一结论不能重复
// 占用现行作用域，候选重试则保持幂等。
func TestLedgerRejectsDuplicateActiveDecision(t *testing.T) {
	ledger := NewLedger()
	first, err := ledger.Propose(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	retry, err := ledger.Propose(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if retry.ID != first.ID {
		t.Fatal("expected a retried proposal to return the same candidate")
	}
	if _, err := ledger.Confirm(first.ID, fixtureApproval()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Propose(fixtureProposal()); !errors.Is(err, ErrDuplicateDecision) {
		t.Fatalf("expected an equivalent active decision to be rejected, got %v", err)
	}
}

// TestLedgerRequiresHumanApproval 锁定生效门槛：确认必须携带人工来源，
// 候选不能自行成为现行决策。
func TestLedgerRequiresHumanApproval(t *testing.T) {
	ledger := NewLedger()
	record, err := ledger.Propose(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(record.ID, HumanApproval{}); !errors.Is(err, ErrNotHumanApproval) {
		t.Fatalf("expected a missing approval to be rejected, got %v", err)
	}
	stored, err := ledger.Get(record.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stored.Status != StatusProposed {
		t.Fatalf("expected the candidate to stay proposed, got %s", stored.Status)
	}
}

// TestProjectContextEnforcesTokenBudget 锁定注入预算：超出预算的条目被
// 丢弃并如实计数，不静默截断，也不允许调用方抬高上限。
func TestProjectContextEnforcesTokenBudget(t *testing.T) {
	ledger := NewLedger()
	records := make([]DecisionRecord, 0, 3)
	for index := 0; index < 3; index++ {
		proposal := fixtureProposal()
		proposal.Decision = "晋升必须经人工确认并保留来源与作废链 " + string(rune('A'+index))
		proposal.Now = fixtureTime.Add(time.Duration(index) * time.Hour)
		record, err := ledger.Propose(proposal)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		approval, err := NewHumanApproval(fixtureApprover, proposal.Now.Add(time.Minute))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		confirmed, err := ledger.Confirm(record.ID, approval)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		records = append(records, confirmed)
	}
	projection, err := ProjectContext(fixtureProject, fixtureOwner, records, 1, fixtureTime.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projection.Entries) != 0 || projection.EstimatedTokens != 0 {
		t.Fatalf("expected no entry to fit the budget, got %d entries", len(projection.Entries))
	}
	if !projection.Truncated || projection.DroppedCount != 3 {
		t.Fatalf("expected three dropped entries, got %d truncated=%t", projection.DroppedCount, projection.Truncated)
	}
	if _, err := ProjectContext(fixtureProject, fixtureOwner, records, MaxContextTokenBudget+1, fixtureTime.Add(4*time.Hour)); err == nil {
		t.Fatal("expected an oversized context budget to be rejected")
	}
	full, err := ProjectContext(fixtureProject, fixtureOwner, records, MaxContextTokenBudget, fixtureTime.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(full.Entries) == 0 || full.EstimatedTokens > MaxContextTokenBudget {
		t.Fatalf("expected the projection to fill the budget, got %d entries and %d tokens", len(full.Entries), full.EstimatedTokens)
	}
}

// TestProjectContextExcludesInactiveRecords 锁定投影只承载现行决策：候选、
// 已撤销、已被替代与已过期的记录都不进入上下文。
func TestProjectContextExcludesInactiveRecords(t *testing.T) {
	ledger := NewLedger()
	proposed, err := ledger.Propose(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	revokedProposal := fixtureProposal()
	revokedProposal.Decision = "被撤销的决策陈述"
	revokedProposal.Now = fixtureTime.Add(time.Minute)
	revoked, err := ledger.Propose(revokedProposal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(revoked.ID, fixtureApproval()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Revoke(revoked.ID, RevokeReasonHumanRequest, fixtureApprover, fixtureTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	projection, err := ProjectContext(fixtureProject, fixtureOwner, []DecisionRecord{proposed, revoked}, DefaultContextTokenBudget, fixtureTime.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projection.Entries) != 0 {
		t.Fatalf("expected no inactive record to enter the projection, got %d", len(projection.Entries))
	}
}

// TestVerifyEvidenceDetectsUnresolvedCitation 锁定引用可核查性：证据解析
// 失败与不存在都必须被如实报告，不能默认为已核实。
func TestVerifyEvidenceDetectsUnresolvedCitation(t *testing.T) {
	resolver := stubResolver{known: map[string]string{
		fixtureJournalEvidence().Key(): fixtureEventHash,
	}}
	record, err := NewProposal(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	missing := fixtureProposal()
	missing.Evidence = []EvidenceRef{{Kind: EvidenceKindGitCommit, CommitID: fixtureCommitID}}
	other, err := NewProposal(missing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	report, err := VerifyEvidence(context.Background(), resolver, []DecisionRecord{record, other}, fixtureTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Resolved != 1 || report.Unresolved != 1 {
		t.Fatalf("expected one resolved and one unresolved reference, got %d and %d", report.Resolved, report.Unresolved)
	}
	for _, status := range report.Statuses {
		if status.RecordID == record.ID && status.Status != EvidenceResolutionFound {
			t.Fatalf("expected the journal reference to resolve, got %s", status.Status)
		}
		if status.RecordID == other.ID && status.Status != EvidenceResolutionMissing {
			t.Fatalf("expected the commit reference to be missing, got %s", status.Status)
		}
	}
	if _, err := VerifyEvidence(context.Background(), nil, []DecisionRecord{record}, fixtureTime); err == nil {
		t.Fatal("expected a missing resolver to be rejected")
	}
}

// TestDecisionPackageDoesNotDependOnPolicyLayer 锁定分层保证：决策账本不得
// 依赖 Policy、工具或配置层，因此项目知识无法覆盖上层授权。
func TestDecisionPackageDoesNotDependOnPolicyLayer(t *testing.T) {
	forbidden := []string{
		"github.com/fxbin/cairn/internal/policy",
		"github.com/fxbin/cairn/internal/tools",
		"github.com/fxbin/cairn/internal/config",
		"github.com/fxbin/cairn/internal/agent",
		"github.com/fxbin/cairn/internal/app",
		"github.com/fxbin/cairn/internal/cli",
	}
	packages, err := parser.ParseDir(token.NewFileSet(), ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := 0
	for _, parsed := range packages {
		for path, file := range parsed.Files {
			found++
			for _, importSpec := range file.Imports {
				value, err := strconv.Unquote(importSpec.Path.Value)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				for _, blocked := range forbidden {
					if value == blocked {
						t.Errorf("%s must not import %s", path, blocked)
					}
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("expected the package sources to be parsed")
	}
}

// TestLedgerConcurrentAccessIsSafe 锁定并发边界：并发录入与只读查询不得产生
// 竞态或撕裂的返回结果。
func TestLedgerConcurrentAccessIsSafe(t *testing.T) {
	ledger := NewLedger()
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			proposal := fixtureProposal()
			proposal.Decision = "并发录入的决策陈述 " + strconv.Itoa(index)
			proposal.Now = fixtureTime.Add(time.Duration(index) * time.Second)
			if _, err := ledger.Propose(proposal); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if _, err := ledger.Query(Query{Project: fixtureProject, Principal: fixtureOwner, Now: fixtureTime.Add(time.Hour)}); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}(index)
	}
	group.Wait()
	records, err := ledger.Query(Query{Project: fixtureProject, Principal: fixtureOwner, Now: fixtureTime.Add(time.Hour)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 8 {
		t.Fatalf("expected eight records, got %d", len(records))
	}
}

// TestLedgerRejectsCrossPrincipalSupersedeOfPrivatePreference 锁定写入侧的
// 主体隔离：他人的私有偏好不可被替代，录入与确认两个环节都必须失败，且
// 被针对的偏好保持现行。
func TestLedgerRejectsCrossPrincipalSupersedeOfPrivatePreference(t *testing.T) {
	ledger := NewLedger()
	preference := fixtureProposal()
	preference.Kind = KindPreference
	preference.Owner = "user_private"
	preference.Decision = "个人偏好：优先阅读中文注释"
	preference.Now = fixtureTime
	owned, err := ledger.Propose(preference)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ownerApproval, err := NewHumanApproval("user_private", fixtureTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	active, err := ledger.Confirm(owned.ID, ownerApproval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	successor := fixtureProposal()
	successor.Owner = "user_other"
	successor.Decision = "他人的权威记录试图替代他人私有偏好"
	successor.Supersedes = active.ID
	successor.Now = fixtureTime.Add(time.Hour)
	if _, err := ledger.Propose(successor); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("expected the cross-principal supersede to be rejected, got %v", err)
	}
	unchanged, err := ledger.Get(active.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unchanged.Status != StatusActive {
		t.Fatalf("expected the private preference to stay active, got %s", unchanged.Status)
	}
}

// TestLedgerRejectsCrossPrincipalConfirmAndRevoke 锁定写入侧的主体隔离：
// 私有偏好的确认与撤销都只能由其所有者发起。
func TestLedgerRejectsCrossPrincipalConfirmAndRevoke(t *testing.T) {
	ledger := NewLedger()
	preference := fixtureProposal()
	preference.Kind = KindPreference
	preference.Owner = "user_private"
	preference.Decision = "个人偏好：优先阅读中文注释"
	preference.Now = fixtureTime
	owned, err := ledger.Propose(preference)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	otherApproval, err := NewHumanApproval("user_other", fixtureTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(owned.ID, otherApproval); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("expected a cross-principal confirmation to be rejected, got %v", err)
	}
	if _, err := ledger.Revoke(owned.ID, RevokeReasonHumanRequest, "user_other", fixtureTime.Add(time.Minute)); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("expected a cross-principal revocation to be rejected, got %v", err)
	}
	stored, err := ledger.Get(owned.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stored.Status != StatusProposed {
		t.Fatalf("expected the candidate to stay proposed, got %s", stored.Status)
	}
}

// TestProjectContextFiltersForeignScopeItself 锁定投影的自我过滤：即使传入
// 未过滤的记录集合，跨项目记录与他人私有偏好都不得进入注入上下文。三条
// 记录都先置为现行状态，使它们只能被作用域过滤排除，而不会被状态过滤
// 顺带挡掉。
func TestProjectContextFiltersForeignScopeItself(t *testing.T) {
	ledger := NewLedger()
	authoritative := fixtureProposal()
	authoritative.Now = fixtureTime
	shared, err := ledger.Propose(authoritative)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	preference := fixtureProposal()
	preference.Kind = KindPreference
	preference.Owner = "user_private"
	preference.Decision = "个人偏好正文不得进入他人上下文"
	preference.Now = fixtureTime
	owned, err := ledger.Propose(preference)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foreign := fixtureProposal()
	foreign.Project = "proj_other"
	foreign.Owner = "user_other"
	foreign.Decision = "另一个项目的决策正文不得进入本项目上下文"
	foreign.Now = fixtureTime
	external, err := ledger.Propose(foreign)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sharedApproval, err := NewHumanApproval(fixtureApprover, fixtureTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(shared.ID, sharedApproval); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ownerApproval, err := NewHumanApproval("user_private", fixtureTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(owned.ID, ownerApproval); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	externalApproval, err := NewHumanApproval("user_other", fixtureTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(external.ID, externalApproval); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	now := fixtureTime.Add(time.Hour)
	unfiltered, err := ledger.Query(Query{Project: fixtureProject, Principal: "user_private", Now: now})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	other, err := ledger.Query(Query{Project: "proj_other", Principal: "user_other", Now: now})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	combined := append(append([]DecisionRecord(nil), unfiltered...), other...)
	if len(combined) != 3 {
		t.Fatalf("expected three current records in the unfiltered set, got %d", len(combined))
	}
	projection, err := ProjectContext(fixtureProject, "user_other", combined, MaxContextTokenBudget, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projection.Entries) != 1 {
		t.Fatalf("expected only the project decision to enter the projection, got %d entries", len(projection.Entries))
	}
	for _, entry := range projection.Entries {
		if strings.Contains(entry.Decision, "个人偏好正文") {
			t.Fatal("another principal's private preference entered the projection")
		}
		if strings.Contains(entry.Decision, "另一个项目") {
			t.Fatal("a foreign project record entered the projection")
		}
	}
	if _, err := ProjectContext("", "user_other", combined, MaxContextTokenBudget, now); err == nil {
		t.Fatal("expected an empty project scope to be rejected")
	}
	if _, err := ProjectContext(fixtureProject, "", combined, MaxContextTokenBudget, now); err == nil {
		t.Fatal("expected an empty principal scope to be rejected")
	}
}

// TestConfirmRejectsRecordViolatingItsOwnContract 锁定落库前的合同复核：确认
// 时刻早于创建时刻时记录不合合同，账本不得持有这样的记录。
func TestConfirmRejectsRecordViolatingItsOwnContract(t *testing.T) {
	ledger := NewLedger()
	proposal := fixtureProposal()
	proposal.Now = fixtureTime.Add(time.Hour)
	record, err := ledger.Propose(proposal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	early, err := NewHumanApproval(fixtureApprover, fixtureTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(record.ID, early); err == nil {
		t.Fatal("expected a confirmation earlier than the creation time to be rejected")
	}
	stored, err := ledger.Get(record.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stored.Status != StatusProposed {
		t.Fatalf("expected the candidate to stay proposed, got %s", stored.Status)
	}
	if err := stored.Validate(); err != nil {
		t.Fatalf("expected the stored record to satisfy its own contract, got %v", err)
	}
}
