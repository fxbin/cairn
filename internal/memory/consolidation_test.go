package memory

import (
	"encoding/json"
	"testing"
	"time"
)

// consolidationFixtureCandidate 构造一条满足 M1 合同的 run_outcome 输入候选。
func consolidationFixtureCandidate(t *testing.T, workspaceHash, taskDigest, runID, outcome, failureClass string, trusted bool, createdAt time.Time) Candidate {
	t.Helper()
	evidence := []EventEvidenceRef{{RunID: runID, EventSeq: 3, EventHash: "sha256:" + repeatHex(2), EventType: "run_failed"}}
	candidate, err := NewRunOutcomeCandidate(workspaceHash, runID, taskDigest, outcome, failureClass, trusted, createdAt, DefaultTTL, evidence, "")
	if err != nil {
		t.Fatalf("fixture candidate %s: %v", runID, err)
	}
	return candidate
}

// TestInputsDigestIsOrderAndDuplicateInsensitive 锁定 write-set 封存摘要：
// 顺序与重复不影响摘要，集合变化必然改变摘要。
func TestInputsDigestIsOrderAndDuplicateInsensitive(t *testing.T) {
	first := InputsDigest([]string{"b", "a"})
	if first != InputsDigest([]string{"a", "b"}) || first != InputsDigest([]string{"a", "a", "b"}) {
		t.Fatal("inputs digest must ignore order and duplicates")
	}
	if first == InputsDigest([]string{"a", "b", "c"}) {
		t.Fatal("inputs digest must change with the input set")
	}
}

// TestDeriveConsolidationsProducesStablePattern 锁定 pattern 归纳：同任务摘要、
// 事实一致的足够样本派生 pattern，标识、载荷与证据可由输入完全重算。
func TestDeriveConsolidationsProducesStablePattern(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	taskDigest := "sha256:" + repeatHex(3)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, taskDigest, "run_a", "failed", "budget_turns", false, now.Add(-2*time.Hour)),
		consolidationFixtureCandidate(t, workspaceHash, taskDigest, "run_b", "failed", "budget_turns", false, now.Add(-1*time.Hour)),
	}}
	plans, err := DeriveConsolidations(inputs, ConsolidationOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Strategy != StrategyTaskOutcomeStability {
		t.Fatalf("plans = %#v", plans)
	}
	plan := plans[0]
	if plan.InputsDigest != InputsDigest([]string{plan.Derived[0].Evidence[0].CandidateID, plan.Derived[0].Evidence[1].CandidateID}) {
		t.Fatal("plan inputs digest must match derived evidence set")
	}
	if len(plan.Derived) != 1 || plan.Derived[0].Kind != KindPattern {
		t.Fatalf("derived = %#v", plan.Derived)
	}
	pattern := plan.Derived[0]
	if pattern.ID != PatternID(workspaceHash, taskDigest) || pattern.ScopeDigest != taskDigest {
		t.Fatalf("pattern identity = %s scope = %s", pattern.ID, pattern.ScopeDigest)
	}
	var payload PatternPayload
	if err := json.Unmarshal(pattern.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Outcome != "failed" || payload.FailureClass != "budget_turns" || payload.Support != 2 || payload.TrustedVerification {
		t.Fatalf("payload = %#v", payload)
	}
	if !payload.FirstCreatedAt.Equal(now.Add(-2*time.Hour)) || !payload.LastCreatedAt.Equal(now.Add(-1*time.Hour)) {
		t.Fatalf("payload bounds = %#v", payload)
	}
	if err := pattern.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestDeriveConsolidationsRejectsInconsistentPatternGroup 锁定失败关闭：
// 事实不一致的组不派生 pattern，即使样本充足。
func TestDeriveConsolidationsRejectsInconsistentPatternGroup(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	taskDigest := "sha256:" + repeatHex(3)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, taskDigest, "run_a", "failed", "budget_turns", false, now),
		consolidationFixtureCandidate(t, workspaceHash, taskDigest, "run_b", "completed", "", true, now),
	}}
	plans, err := DeriveConsolidations(inputs, ConsolidationOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range plans {
		for _, derived := range plan.Derived {
			if derived.Kind == KindPattern {
				t.Fatal("inconsistent group must not produce a pattern")
			}
		}
	}
}

// TestDeriveConsolidationsProducesHypothesisAcrossTasks 锁定 hypothesis 归纳：
// 同失败类别跨任务重复出现时派生未证明推断，单任务不派生。
func TestDeriveConsolidationsProducesHypothesisAcrossTasks(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_a", "failed", "budget_turns", false, now.Add(-3*time.Hour)),
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(4), "run_b", "failed", "budget_turns", false, now.Add(-2*time.Hour)),
	}}
	plans, err := DeriveConsolidations(inputs, ConsolidationOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	var hypothesis *DerivedCandidate
	for index, plan := range plans {
		if plan.Strategy != StrategyFailureClassRecurrence {
			continue
		}
		for derivedIndex := range plan.Derived {
			if plans[index].Derived[derivedIndex].Kind == KindHypothesis {
				hypothesis = &plans[index].Derived[derivedIndex]
			}
		}
	}
	if hypothesis == nil {
		t.Fatalf("hypothesis missing from plans = %#v", plans)
	}
	if hypothesis.ID != HypothesisID(workspaceHash, "budget_turns") || hypothesis.ScopeDigest != HypothesisScopeDigest(workspaceHash, "budget_turns") {
		t.Fatalf("hypothesis identity = %s", hypothesis.ID)
	}
	var payload HypothesisPayload
	if err := json.Unmarshal(hypothesis.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Support != 2 || payload.DistinctTaskDigests != 2 {
		t.Fatalf("payload = %#v", payload)
	}
	if err := hypothesis.Validate(); err != nil {
		t.Fatal(err)
	}
	singleTask := ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_c", "failed", "budget_turns", false, now),
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_d", "failed", "budget_turns", false, now),
	}}
	plans, err = DeriveConsolidations(singleTask, ConsolidationOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range plans {
		if plan.Strategy == StrategyFailureClassRecurrence {
			t.Fatal("single-task recurrence must not produce a hypothesis")
		}
	}
}

// TestDeriveConsolidationsRequiresMinimumSupport 锁定支持度下限：
// 阈值不足不派生；调用方不得把下限调到常量之下。
func TestDeriveConsolidationsRequiresMinimumSupport(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_a", "failed", "budget_turns", false, now),
	}}
	if plans, err := DeriveConsolidations(inputs, ConsolidationOptions{Now: now}); err != nil || len(plans) != 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	raised := ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_a", "failed", "budget_turns", false, now),
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_b", "failed", "budget_turns", false, now),
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_c", "failed", "budget_turns", false, now),
	}}
	if _, err := DeriveConsolidations(raised, ConsolidationOptions{Now: now, MinPatternSupport: PatternMinSupport - 1}); err == nil {
		t.Fatal("pattern minimum support must not be lowered")
	}
	if plans, err := DeriveConsolidations(raised, ConsolidationOptions{Now: now, MinPatternSupport: 3}); err != nil || len(plans) != 1 {
		t.Fatalf("raised threshold plans = %#v err = %v", plans, err)
	}
}

// TestDeriveConsolidationsValidatesInputsAndClock 锁定输入与时间边界：
// 空集、跨工作区、重复候选与缺失时钟全部失败关闭。
func TestDeriveConsolidationsValidatesInputsAndClock(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	otherHash := "sha256:" + repeatHex(5)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if _, err := DeriveConsolidations(ConsolidationInputs{WorkspaceHash: workspaceHash}, ConsolidationOptions{Now: now}); err == nil {
		t.Fatal("empty input set must fail closed")
	}
	candidate := consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_a", "failed", "budget_turns", false, now)
	foreign := consolidationFixtureCandidate(t, otherHash, "sha256:"+repeatHex(3), "run_b", "failed", "budget_turns", false, now)
	if _, err := DeriveConsolidations(ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{candidate, foreign}}, ConsolidationOptions{Now: now}); err == nil {
		t.Fatal("cross-workspace input must fail closed")
	}
	if _, err := DeriveConsolidations(ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{candidate, candidate}}, ConsolidationOptions{Now: now}); err == nil {
		t.Fatal("duplicate input must fail closed")
	}
	if _, err := DeriveConsolidations(ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{candidate}}, ConsolidationOptions{}); err == nil {
		t.Fatal("missing frozen clock must fail closed")
	}
}

// TestDerivedCandidateValidateRejectsTampering 锁定载荷与证据防篡改：
// 未知字段、支持度与证据不一致、乱序证据全部拒绝。
func TestDerivedCandidateValidateRejectsTampering(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	taskDigest := "sha256:" + repeatHex(3)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, taskDigest, "run_a", "failed", "budget_turns", false, now),
		consolidationFixtureCandidate(t, workspaceHash, taskDigest, "run_b", "failed", "budget_turns", false, now),
	}}
	plans, err := DeriveConsolidations(inputs, ConsolidationOptions{Now: now})
	if err != nil || len(plans) != 1 || len(plans[0].Derived) != 1 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	pattern := plans[0].Derived[0]
	tamperedPayload := json.RawMessage(`{"task_prompt_digest":"` + taskDigest + `","outcome":"failed","support":2,"first_created_at":"2026-08-25T10:00:00Z","last_created_at":"2026-08-25T11:00:00Z","note":"free text"}`)
	withExtra := pattern
	withExtra.Payload = tamperedPayload
	if err := withExtra.Validate(); err == nil {
		t.Fatal("unknown payload field must be rejected")
	}
	dropped := pattern
	dropped.Evidence = pattern.Evidence[:1]
	if err := dropped.Validate(); err == nil {
		t.Fatal("evidence count below support must be rejected")
	}
	unsorted := pattern
	unsorted.Evidence = []SourceCandidateRef{pattern.Evidence[1], pattern.Evidence[0]}
	if err := unsorted.Validate(); err == nil {
		t.Fatal("unsorted evidence must be rejected")
	}
	if _, err := DeriveConsolidations(inputs, ConsolidationOptions{Now: now, TTL: MaxTTL + time.Second}); err == nil {
		t.Fatal("over-limit TTL must fail closed")
	}
}

// TestDeriveConsolidationsIsOrderInsensitive 锁定归纳确定性：打乱输入顺序
// 两次调用产生完全一致的巩固计划与派生候选（含标识与载荷字节）。
func TestDeriveConsolidationsIsOrderInsensitive(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	first := []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_a", "failed", "budget_turns", false, now.Add(-2*time.Hour)),
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(4), "run_b", "failed", "budget_turns", false, now.Add(-1*time.Hour)),
		consolidationFixtureCandidate(t, workspaceHash, "sha256:"+repeatHex(3), "run_c", "failed", "budget_turns", false, now),
	}
	second := []Candidate{first[2], first[0], first[1]}
	left, err := DeriveConsolidations(ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: first}, ConsolidationOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	right, err := DeriveConsolidations(ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: second}, ConsolidationOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatal(err)
	}
	if string(leftJSON) != string(rightJSON) {
		t.Fatalf("consolidation output is order sensitive:\n%s\n%s", leftJSON, rightJSON)
	}
}

// TestPatternPayloadRejectsInvalidOutcome 锁定载荷枚举负例：
// pattern 载荷携带未知 outcome 时 Validate 失败关闭。
func TestPatternPayloadRejectsInvalidOutcome(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	taskDigest := "sha256:" + repeatHex(3)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []Candidate{
		consolidationFixtureCandidate(t, workspaceHash, taskDigest, "run_a", "failed", "budget_turns", false, now),
		consolidationFixtureCandidate(t, workspaceHash, taskDigest, "run_b", "failed", "budget_turns", false, now),
	}}
	plans, err := DeriveConsolidations(inputs, ConsolidationOptions{Now: now})
	if err != nil || len(plans) != 1 || len(plans[0].Derived) != 1 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	tampered := plans[0].Derived[0]
	tampered.Payload = json.RawMessage(`{"task_prompt_digest":"` + taskDigest + `","outcome":"succeeded","support":2,"trusted_verification":false,"first_created_at":"` + now.Format(time.RFC3339Nano) + `","last_created_at":"` + now.Format(time.RFC3339Nano) + `"}`)
	if err := tampered.Validate(); err == nil {
		t.Fatal("invalid payload outcome must be rejected")
	}
}

// repeatHex 生成由种子偏移的 64 位十六进制测试摘要，满足 sha256 摘要格式。
func repeatHex(seed int) string {
	const hexChars = "0123456789abcdef"
	result := make([]byte, 0, 64)
	for index := 0; index < 64; index++ {
		result = append(result, hexChars[(index+seed)%len(hexChars)])
	}
	return string(result)
}
