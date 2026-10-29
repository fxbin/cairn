package decision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// 本文件是决策账本的对照评测装置，不是模型行为评测。评测环境没有 Provider
// 凭据，也没有本地 benchmark 任务集，因此任务与事实都是构造数据：测量对象
// 是决策账本合同本身的信息含量与引用完整性，而不是模型的调查习惯。
// 测量口径、样本与边界记录在 docs/decisions 的 ADR 与 Spike 报告中。

// abFact 是构造世界里的一个事实：Statement 是真实答案，Support 是真正支撑
// 它的证据定位，Decoy 是只提到同一话题但不足以支撑结论的近期证据。
type abFact struct {
	ID        string
	Statement string
	Support   EvidenceRef
	Decoy     EvidenceRef
	HasDecoy  bool
}

// abTask 是一个构造任务：Agent 必须给出事实陈述并给出可核查引用。
type abTask struct {
	ID     string
	FactID string
}

// abLedgerState 描述某个事实在账本里的构造状态。
type abLedgerState string

const (
	// abLedgerCurrent 表示账本中有现行有效记录。
	abLedgerCurrent abLedgerState = "current"
	// abLedgerSuperseded 表示旧结论已被新结论替代。
	abLedgerSuperseded abLedgerState = "superseded"
	// abLedgerRevoked 表示记录已被撤销。
	abLedgerRevoked abLedgerState = "revoked"
	// abLedgerExpired 表示记录保留期已耗尽。
	abLedgerExpired abLedgerState = "expired"
	// abLedgerUnresolvable 表示记录存在但引用无法在事实源中定位。
	abLedgerUnresolvable abLedgerState = "unresolvable"
	// abLedgerAbsent 表示账本中没有该事实的记录。
	abLedgerAbsent abLedgerState = "absent"
)

// abArmConfig 描述一次实验臂的检索配置。
type abArmConfig struct {
	// Label 是该臂的名称。
	Label string
	// Budget 是注入上下文的 token 预算；零值表示使用默认预算。
	Budget int
	// BypassBudget 为真时跳过预算约束，只衡量账本本身的信息含量。
	BypassBudget bool
}

// abMetrics 是单次评测配置的汇总指标。
type abMetrics struct {
	// Label 是该配置的名称。
	Label string
	// Investigations 是实际发起的调查次数。
	Investigations int
	// RepeatedInvestigations 是重复调查次数：调查对象在本次评测中已被确认过。
	RepeatedInvestigations int
	// Citations 是最终给出的引用条数。
	Citations int
	// WrongCitations 是引用不支撑任务结论的条数。
	WrongCitations int
	// Successes 是既得到结论又给出正确引用的任务数。
	Successes int
	// UnresolvedLedgerHits 是账本命中但引用无法核查、因而回退调查的次数。
	UnresolvedLedgerHits int
	// InjectedTokens 是注入上下文的估算 token 总量。
	InjectedTokens int
	// InjectedEntries 是注入上下文的条目数。
	InjectedEntries int
	// Truncated 表示注入是否因预算不足被截断。
	Truncated bool
}

// abNow 是评测的判定时刻：晚于过期记录、又不越过默认保留期。
var abNow = fixtureTime.Add(48 * time.Hour)

// abHash 返回构造用十六进制摘要。
func abHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// abDigest 返回构造用内容寻址摘要。
func abDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// abFacts 返回构造事实集：十条事实，其中五条带干扰证据。
func abFacts() map[string]abFact {
	facts := make(map[string]abFact, 10)
	for index := 1; index <= 10; index++ {
		id := fmt.Sprintf("f%02d", index)
		fact := abFact{
			ID:        id,
			Statement: fmt.Sprintf("关于 %s 的项目结论是：采用既定边界并保留来源", id),
			Support: EvidenceRef{
				Kind:      EvidenceKindJournalEvent,
				RunID:     "run_" + abHash(id)[:12],
				EventSeq:  uint64(index),
				EventHash: abHash("support:" + id),
			},
		}
		if index%2 == 1 {
			fact.HasDecoy = true
			fact.Decoy = EvidenceRef{Kind: EvidenceKindArtifact, Digest: abDigest("decoy:" + id)}
		}
		facts[id] = fact
	}
	return facts
}

// abTasks 返回构造任务集：十二个任务覆盖十条事实，其中两条事实被重复询问。
func abTasks() []abTask {
	return []abTask{
		{ID: "t01", FactID: "f01"},
		{ID: "t02", FactID: "f02"},
		{ID: "t03", FactID: "f02"},
		{ID: "t04", FactID: "f03"},
		{ID: "t05", FactID: "f04"},
		{ID: "t06", FactID: "f05"},
		{ID: "t07", FactID: "f01"},
		{ID: "t08", FactID: "f06"},
		{ID: "t09", FactID: "f07"},
		{ID: "t10", FactID: "f08"},
		{ID: "t11", FactID: "f09"},
		{ID: "t12", FactID: "f10"},
	}
}

// abLedgerStates 返回每条事实在账本中的构造状态。
func abLedgerStates() map[string]abLedgerState {
	return map[string]abLedgerState{
		"f01": abLedgerCurrent,
		"f02": abLedgerCurrent,
		"f03": abLedgerSuperseded,
		"f04": abLedgerRevoked,
		"f05": abLedgerExpired,
		"f06": abLedgerAbsent,
		"f07": abLedgerAbsent,
		"f08": abLedgerUnresolvable,
		"f09": abLedgerCurrent,
		"f10": abLedgerCurrent,
	}
}

// abResolver 返回构造事实源：所有支撑证据与干扰证据都可解析。
func abResolver(facts map[string]abFact) stubResolver {
	known := map[string]string{}
	for _, fact := range facts {
		known[fact.Support.Key()] = abDigest("resolved:" + fact.Support.Key())
		if fact.HasDecoy {
			known[fact.Decoy.Key()] = abDigest("resolved:" + fact.Decoy.Key())
		}
	}
	return stubResolver{known: known}
}

// abPropose 录入一条账本候选并返回其标识。
func abPropose(t *testing.T, ledger *Ledger, fact abFact, decision string, evidence []EvidenceRef, supersedes string, createdAt time.Time) DecisionRecord {
	t.Helper()
	proposal := Proposal{
		Project:    fixtureProject,
		Owner:      fixtureOwner,
		Kind:       KindAuthoritative,
		Context:    "topic " + fact.ID + " 记忆晋升与撤销语义",
		Decision:   decision,
		Rationale:  "结论来自已复核的运行证据，保留来源以便追溯",
		Evidence:   evidence,
		Supersedes: supersedes,
		Now:        createdAt,
	}
	record, err := ledger.Propose(proposal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return record
}

// abConfirm 以人工来源确认一条记录。
func abConfirm(t *testing.T, ledger *Ledger, id string, at time.Time) {
	t.Helper()
	approval, err := NewHumanApproval(fixtureApprover, at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ledger.Confirm(id, approval); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// abBuildLedger 用真实录入接口构造账本：现行、被替代、被撤销、过期与引用
// 无法核查五种状态都通过受控录入与状态迁移产生。
func abBuildLedger(t *testing.T, facts map[string]abFact, states map[string]abLedgerState) *Ledger {
	t.Helper()
	ledger := NewLedger()
	for _, factID := range sortedFactIDs(facts) {
		fact := facts[factID]
		switch states[factID] {
		case abLedgerAbsent:
			continue
		case abLedgerCurrent:
			record := abPropose(t, ledger, fact, fact.Statement, []EvidenceRef{fact.Support}, "", fixtureTime)
			abConfirm(t, ledger, record.ID, fixtureTime.Add(time.Hour))
		case abLedgerSuperseded:
			older := abPropose(t, ledger, fact, "旧结论："+fact.Statement, []EvidenceRef{fact.Decoy}, "", fixtureTime)
			abConfirm(t, ledger, older.ID, fixtureTime.Add(time.Hour))
			newer := abPropose(t, ledger, fact, fact.Statement, []EvidenceRef{fact.Support}, older.ID, fixtureTime.Add(2*time.Hour))
			abConfirm(t, ledger, newer.ID, fixtureTime.Add(3*time.Hour))
		case abLedgerRevoked:
			record := abPropose(t, ledger, fact, fact.Statement, []EvidenceRef{fact.Support}, "", fixtureTime)
			abConfirm(t, ledger, record.ID, fixtureTime.Add(time.Hour))
			if _, err := ledger.Revoke(record.ID, RevokeReasonHumanRequest, fixtureTime.Add(2*time.Hour)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		case abLedgerExpired:
			proposal := Proposal{
				Project:   fixtureProject,
				Owner:     fixtureOwner,
				Kind:      KindAuthoritative,
				Context:   "topic " + fact.ID + " 记忆晋升与撤销语义",
				Decision:  fact.Statement,
				Rationale: "结论来自已复核的运行证据，保留来源以便追溯",
				Evidence:  []EvidenceRef{fact.Support},
				TTL:       time.Hour,
				Now:       fixtureTime,
			}
			record, err := ledger.Propose(proposal)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			abConfirm(t, ledger, record.ID, fixtureTime.Add(30*time.Minute))
		case abLedgerUnresolvable:
			bogus := EvidenceRef{Kind: EvidenceKindArtifact, Digest: abDigest("missing:" + fact.ID)}
			record := abPropose(t, ledger, fact, fact.Statement, []EvidenceRef{bogus}, "", fixtureTime)
			abConfirm(t, ledger, record.ID, fixtureTime.Add(time.Hour))
		}
	}
	return ledger
}

// sortedFactIDs 返回按标识排序的事实地键，保证构造顺序可复现。
func sortedFactIDs(facts map[string]abFact) []string {
	ids := make([]string, 0, len(facts))
	for id := range facts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// abFindEntry 按上下文中的话题键在投影条目中做精确匹配。账本的只读检索
// 不接受自由文本，因此匹配键是结构化的话题标识。
func abFindEntry(entries []ContextEntry, factID string) (ContextEntry, bool) {
	prefix := "topic " + factID + " "
	for _, entry := range entries {
		if strings.HasPrefix(entry.Context, prefix) {
			return entry, true
		}
	}
	return ContextEntry{}, false
}

// abRunBaselineArm 跑没有决策账本的对照臂：Cairn 既有的会话前情卡只覆盖
// 上一次运行，可信记忆只保存结构化运行结果，两者都不携带带可核查引用的
// 项目结论，因此每个任务都必须重新调查，并引用调查到的最近一条相关证据。
func abRunBaselineArm(tasks []abTask, facts map[string]abFact) abMetrics {
	metrics := abMetrics{Label: "A 无决策账本"}
	established := map[string]bool{}
	for _, task := range tasks {
		fact := facts[task.FactID]
		metrics.Investigations++
		if established[task.FactID] {
			metrics.RepeatedInvestigations++
		}
		established[task.FactID] = true
		cited := fact.Support
		if fact.HasDecoy {
			cited = fact.Decoy
		}
		metrics.Citations++
		if cited.Key() != fact.Support.Key() {
			metrics.WrongCitations++
		} else {
			metrics.Successes++
		}
	}
	return metrics
}

// abRunLedgerArm 跑带决策账本的实验臂：先按现行状态检索并按预算注入上下文，
// 命中后先核查引用是否可解析，不可解析则回退调查，与对照臂采用完全相同的
// 调查与引用规则。
func abRunLedgerArm(t *testing.T, ledger *Ledger, tasks []abTask, facts map[string]abFact, resolver stubResolver, config abArmConfig) abMetrics {
	t.Helper()
	metrics := abMetrics{Label: config.Label}
	established := map[string]bool{}
	for _, task := range tasks {
		fact := facts[task.FactID]
		records, err := ledger.Query(Query{
			Project:   fixtureProject,
			Principal: fixtureOwner,
			Statuses:  []string{StatusActive},
			Now:       abNow,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var hit *DecisionRecord
		if config.BypassBudget {
			for index := range records {
				if strings.HasPrefix(records[index].Context, "topic "+fact.ID+" ") {
					hit = &records[index]
					break
				}
			}
		} else {
			projection, err := ProjectContext(records, config.Budget, abNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			metrics.InjectedTokens += projection.EstimatedTokens
			metrics.InjectedEntries += len(projection.Entries)
			if projection.Truncated {
				metrics.Truncated = true
			}
			if entry, found := abFindEntry(projection.Entries, fact.ID); found {
				record, err := ledger.Get(entry.ID)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				hit = &record
			}
		}
		if hit != nil {
			report, err := VerifyEvidence(context.Background(), resolver, []DecisionRecord{*hit}, abNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if report.Unresolved == 0 {
				metrics.Citations++
				if len(hit.Evidence) > 0 && hit.Evidence[0].Key() == fact.Support.Key() {
					metrics.Successes++
				} else {
					metrics.WrongCitations++
				}
				continue
			}
			metrics.UnresolvedLedgerHits++
		}
		metrics.Investigations++
		if established[task.FactID] {
			metrics.RepeatedInvestigations++
		}
		established[task.FactID] = true
		cited := fact.Support
		if fact.HasDecoy {
			cited = fact.Decoy
		}
		metrics.Citations++
		if cited.Key() != fact.Support.Key() {
			metrics.WrongCitations++
		} else {
			metrics.Successes++
		}
	}
	return metrics
}

// abEntryCostBreakdown 分解单条投影条目的 token 成本构成，用于判断账本注入
// 的开销主要来自正文还是来自可追溯性所需的标识与摘要。
func abEntryCostBreakdown(entry ContextEntry) (full int, withoutRationale int, shortIdentifier int, compact int) {
	full, _ = estimateEntryTokens(entry)
	trimmed := entry
	trimmed.Rationale = ""
	withoutRationale, _ = estimateEntryTokens(trimmed)
	shortened := entry
	if len(shortened.ID) > 12 {
		shortened.ID = shortened.ID[:12]
	}
	shortIdentifier, _ = estimateEntryTokens(shortened)
	compactEntry := shortened
	compactEntry.Rationale = ""
	compact, _ = estimateEntryTokens(compactEntry)
	return full, withoutRationale, shortIdentifier, compact
}

// abReport 汇总一次评测配置的指标文本。
func abReport(metrics abMetrics, tasks int) string {
	return fmt.Sprintf(
		"%s | 任务 %d | 调查 %d | 重复调查 %d | 引用 %d | 错误引用 %d (%.1f%%) | 有效成功 %d (%.1f%%) | 账本回退 %d | 注入 %d token / %d 条 | 截断 %t",
		metrics.Label,
		tasks,
		metrics.Investigations,
		metrics.RepeatedInvestigations,
		metrics.Citations,
		metrics.WrongCitations,
		100*float64(metrics.WrongCitations)/float64(tasks),
		metrics.Successes,
		100*float64(metrics.Successes)/float64(tasks),
		metrics.UnresolvedLedgerHits,
		metrics.InjectedTokens,
		metrics.InjectedEntries,
		metrics.Truncated,
	)
}

// TestDecisionLedgerABEvaluation 跑决策账本的对照评测。断言只覆盖合同不
// 变量（过期与被替代记录不得进入注入、账本引用必须先核查、账本臂的错误
// 引用不得多于对照臂），对照结论本身以日志形式给出，不写死为断言。
func TestDecisionLedgerABEvaluation(t *testing.T) {
	facts := abFacts()
	tasks := abTasks()
	states := abLedgerStates()
	resolver := abResolver(facts)
	ledger := abBuildLedger(t, facts, states)

	baseline := abRunBaselineArm(tasks, facts)
	t.Log(abReport(baseline, len(tasks)))

	ledgerDefault := abRunLedgerArm(t, ledger, tasks, facts, resolver, abArmConfig{Label: "B 有决策账本（默认注入预算 400 token）", Budget: DefaultContextTokenBudget})
	t.Log(abReport(ledgerDefault, len(tasks)))

	ledgerCeiling := abRunLedgerArm(t, ledger, tasks, facts, resolver, abArmConfig{Label: "B 有决策账本（注入预算上限 800 token）", Budget: MaxContextTokenBudget})
	t.Log(abReport(ledgerCeiling, len(tasks)))

	ledgerContent := abRunLedgerArm(t, ledger, tasks, facts, resolver, abArmConfig{Label: "C 账本信息含量（不设注入预算）", BypassBudget: true})
	t.Log(abReport(ledgerContent, len(tasks)))

	if len(tasks) < 10 || len(tasks) > 20 {
		t.Fatalf("expected the evaluation to stay within 10 to 20 tasks, got %d", len(tasks))
	}
	current, err := ledger.Query(Query{Project: fixtureProject, Principal: fixtureOwner, Statuses: []string{StatusActive}, Now: abNow})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	projection, err := ProjectContext(current, MaxContextTokenBudget, abNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, entry := range projection.Entries {
		if strings.Contains(entry.Decision, "旧结论：") {
			t.Fatalf("a superseded record entered the projection: %s", entry.ID)
		}
	}
	if _, found := abFindEntry(projection.Entries, "f04"); found {
		t.Fatal("a revoked record entered the projection")
	}
	if _, found := abFindEntry(projection.Entries, "f05"); found {
		t.Fatal("an expired record entered the projection")
	}
	if _, found := abFindEntry(projection.Entries, "f06"); found {
		t.Fatal("a record that was never entered entered the projection")
	}
	if ledgerContent.UnresolvedLedgerHits == 0 {
		t.Fatal("expected the unresolvable citation to be detected and to fall back")
	}
	if ledgerContent.WrongCitations > baseline.WrongCitations {
		t.Fatalf("expected the ledger arm to cite no worse than the baseline, got %d against %d", ledgerContent.WrongCitations, baseline.WrongCitations)
	}
	if ledgerContent.Investigations >= baseline.Investigations {
		t.Fatalf("expected the ledger arm to need fewer investigations, got %d against %d", ledgerContent.Investigations, baseline.Investigations)
	}
	if len(projection.Entries) == 0 {
		t.Fatal("expected the projection to carry at least one current record")
	}
	full, withoutRationale, shortIdentifier, compact := abEntryCostBreakdown(projection.Entries[0])
	t.Logf("单条注入成本 | 完整 %d token | 去理由 %d token | 短标识 %d token | 短标识且去理由 %d token", full, withoutRationale, shortIdentifier, compact)
}
