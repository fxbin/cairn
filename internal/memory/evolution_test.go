package memory

import (
	"encoding/json"
	"testing"
	"time"
)

// variantFixture 返回一条合法的闭合变体参数。
func variantFixture() CompactionParamsVariant {
	return CompactionParamsVariant{
		SchemaVersion:           EvolutionSchemaVersion,
		MaxPromptTokens:         50_000,
		MaxSummaryBytes:         16_384,
		InventoryBudgetPercent:  25,
		ThresholdReservePercent: 20,
	}
}

// evolutionSourceFixture 返回一个合法的来源复合键。
func evolutionSourceFixture() EvolutionSourceRef {
	return EvolutionSourceRef{DerivedID: "pat_" + repeatHex(2), ConsolidationID: "cons_" + repeatHex(3)}
}

// TestVariantFingerprintIsStableAndBounded 锁定变体指纹的确定性与参数边界
// 失败关闭：同参数恒同指纹、任一越界字段被拒绝。
func TestVariantFingerprintIsStableAndBounded(t *testing.T) {
	first, err := VariantFingerprint(TargetKindContextCompactionParams, variantFixture())
	if err != nil {
		t.Fatal(err)
	}
	second, err := VariantFingerprint(TargetKindContextCompactionParams, variantFixture())
	if err != nil || first != second {
		t.Fatalf("fingerprint must be deterministic: %q vs %q err=%v", first, second, err)
	}
	if _, err := VariantFingerprint("verification_scheduling", variantFixture()); err == nil {
		t.Fatal("unapproved target kind must fail closed")
	}
	bounds := []CompactionParamsVariant{
		func() CompactionParamsVariant {
			v := variantFixture()
			v.MaxPromptTokens = VariantMinPromptTokens - 1
			return v
		}(),
		func() CompactionParamsVariant {
			v := variantFixture()
			v.MaxSummaryBytes = VariantMinSummaryBytes - 1
			return v
		}(),
		func() CompactionParamsVariant {
			v := variantFixture()
			v.InventoryBudgetPercent = VariantMaxBudgetPercent + 1
			return v
		}(),
		func() CompactionParamsVariant {
			v := variantFixture()
			v.ThresholdReservePercent = VariantMinReservePercent - 1
			return v
		}(),
		func() CompactionParamsVariant {
			v := variantFixture()
			v.SchemaVersion = EvolutionSchemaVersion + 1
			return v
		}(),
	}
	for index, variant := range bounds {
		if err := variant.Validate(); err == nil {
			t.Fatalf("out-of-bound variant %d must fail validation", index)
		}
	}
}

// TestEvolutionCandidateIDIsDeterministic 锁定候选标识派生：同输入恒同 ID，
// 任何组成字段变化产生不同 ID。
func TestEvolutionCandidateIDIsDeterministic(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	source := evolutionSourceFixture()
	fingerprint := "sha256:" + repeatHex(4)
	first := EvolutionCandidateID(workspaceHash, source, TargetKindContextCompactionParams, fingerprint)
	if first == EvolutionCandidateID(workspaceHash, EvolutionSourceRef{DerivedID: source.DerivedID + "x", ConsolidationID: source.ConsolidationID}, TargetKindContextCompactionParams, fingerprint) {
		t.Fatal("source derived id must change the candidate ID")
	}
	if first == EvolutionCandidateID(workspaceHash, source, TargetKindContextCompactionParams, "sha256:"+repeatHex(5)) {
		t.Fatal("variant fingerprint must change the candidate ID")
	}
	if first == EvolutionCandidateID("sha256:"+repeatHex(6), source, TargetKindContextCompactionParams, fingerprint) {
		t.Fatal("workspace must change the candidate ID")
	}
}

// TestEvolutionCandidateValidateLocksContract 锁定候选合同的完整性：
// ID 一致性、指纹格式、保留期、ready 需绑定评测集与封闭状态词表。
func TestEvolutionCandidateValidateLocksContract(t *testing.T) {
	workspaceHash := "sha256:" + repeatHex(1)
	source := evolutionSourceFixture()
	variant := variantFixture()
	variantFingerprint, err := VariantFingerprint(TargetKindContextCompactionParams, variant)
	if err != nil {
		t.Fatal(err)
	}
	variantDigest, err := VariantSpecDigest(variant)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	candidate := EvolutionCandidate{
		ID:                  EvolutionCandidateID(workspaceHash, source, TargetKindContextCompactionParams, variantFingerprint),
		SchemaVersion:       EvolutionSchemaVersion,
		WorkspaceHash:       workspaceHash,
		Source:              source,
		TargetKind:          TargetKindContextCompactionParams,
		VariantSpecSHA256:   variantDigest,
		VariantFingerprint:  variantFingerprint,
		BaselineFingerprint: "sha256:" + repeatHex(7),
		CreatedAt:           now,
		ExpiresAt:           now.Add(DefaultCandidateTTL),
		Status:              CandidateStateDraft,
	}
	if err := candidate.Validate(); err != nil {
		t.Fatal(err)
	}
	ready := candidate
	ready.Status = CandidateStateReady
	if err := ready.Validate(); err == nil {
		t.Fatal("ready without eval set fingerprint must fail")
	}
	ready.EvalSetFingerprint = "sha256:" + repeatHex(8)
	if err := ready.Validate(); err != nil {
		t.Fatal(err)
	}
	tampered := candidate
	tampered.ID = "evo_" + repeatHex(9)
	if err := tampered.Validate(); err == nil {
		t.Fatal("ID mismatch must fail closed")
	}
	expired := candidate
	expired.ExpiresAt = candidate.CreatedAt
	if err := expired.Validate(); err == nil {
		t.Fatal("non-positive retention must fail closed")
	}
	freeText := candidate
	freeText.Status = "maybe"
	if err := freeText.Validate(); err == nil {
		t.Fatal("unknown status must fail closed")
	}
}

// TestCandidateTransitionMatrixClosesM4LateralMoves 锁定状态机：本切片只允许
// draft/ready 到三个终态与 evaluating 中断作废；evaluated_* 与 promoted 的
// 迁移入口在存储层之前就在合同层失败关闭。
func TestCandidateTransitionMatrixClosesM4LateralMoves(t *testing.T) {
	permitted := [][2]string{
		{CandidateStateDraft, CandidateStateRejected},
		{CandidateStateDraft, CandidateStateInvalidated},
		{CandidateStateDraft, CandidateStateExpired},
		{CandidateStateReady, CandidateStateRejected},
		{CandidateStateReady, CandidateStateInvalidated},
		{CandidateStateReady, CandidateStateExpired},
		{CandidateStateEvaluating, CandidateStateInvalidated},
	}
	for _, pair := range permitted {
		if err := ValidCandidateTransition(pair[0], pair[1]); err != nil {
			t.Fatalf("expected %s -> %s to be permitted: %v", pair[0], pair[1], err)
		}
	}
	forbidden := [][2]string{
		{CandidateStateDraft, CandidateStateReady},
		{CandidateStateDraft, CandidateStateEvaluating},
		{CandidateStateReady, CandidateStateEvaluating},
		{CandidateStateEvaluating, CandidateStateEvaluatedPassed},
		{CandidateStateEvaluatedPassed, CandidateStatePromoted},
		{CandidateStateRejected, CandidateStateDraft},
		{CandidateStatePromoted, CandidateStateRejected},
	}
	for _, pair := range forbidden {
		if err := ValidCandidateTransition(pair[0], pair[1]); err == nil {
			t.Fatalf("expected %s -> %s to be closed in this slice", pair[0], pair[1])
		}
	}
	for _, state := range []string{CandidateStateRejected, CandidateStateInvalidated, CandidateStateExpired, CandidateStatePromoted} {
		if !IsTerminalCandidateState(state) {
			t.Fatalf("%s must be terminal", state)
		}
	}
}

// TestCooldownAndRejectionLexiconAreClosed 锁定冷却规范化与拒绝理由词表。
func TestCooldownAndRejectionLexiconAreClosed(t *testing.T) {
	if _, err := NormalizeCandidateTTL(0); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeCandidateTTL(MaxTTL + time.Second); err == nil {
		t.Fatal("over-limit candidate TTL must fail closed")
	}
	if _, err := NormalizeCooldown(0); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeCooldown(MaxCooldown + time.Second); err == nil {
		t.Fatal("over-limit cooldown must fail closed")
	}
	if _, err := NormalizeCooldown(-time.Second); err == nil {
		t.Fatal("negative cooldown must fail closed")
	}
	if !ValidEvolutionRejectionReason(RejectReasonHumanRequest) || !ValidEvolutionRejectionReason(RejectReasonEvaluationFailed) {
		t.Fatal("declared rejection reasons must be valid")
	}
	if ValidEvolutionRejectionReason("not_in_the_lexicon") {
		t.Fatal("free-text rejection reason must fail closed")
	}
	// ADR-0012 §5.5 取消"不确定累计"语义后，inconclusive_limit 必须从拒绝
	// 词表永久移除；人工拒绝 inconclusive 时改用 insufficient_evidence。
	if ValidEvolutionRejectionReason("inconclusive_limit") {
		t.Fatal("inconclusive_limit must stay removed from the rejection lexicon")
	}
	key := CooldownKey{Source: evolutionSourceFixture(), TargetKind: TargetKindContextCompactionParams, VariantFingerprint: "sha256:" + repeatHex(4)}
	if err := key.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := key
	bad.TargetKind = "workspace_ignore_view"
	if err := bad.Validate(); err == nil {
		t.Fatal("unapproved cooldown target kind must fail closed")
	}
	encoded, err := json.Marshal(candidateJSONProbe())
	if err != nil || len(encoded) == 0 {
		t.Fatalf("candidate must stay serializable: %v", err)
	}
}

// candidateJSONProbe 构造一条可序列化的合法候选供序列化探针使用。
func candidateJSONProbe() EvolutionCandidate {
	workspaceHash := "sha256:" + repeatHex(1)
	source := evolutionSourceFixture()
	fingerprint := "sha256:" + repeatHex(4)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	return EvolutionCandidate{
		ID:                  EvolutionCandidateID(workspaceHash, source, TargetKindContextCompactionParams, fingerprint),
		SchemaVersion:       EvolutionSchemaVersion,
		WorkspaceHash:       workspaceHash,
		Source:              source,
		TargetKind:          TargetKindContextCompactionParams,
		VariantSpecSHA256:   "sha256:" + repeatHex(9),
		VariantFingerprint:  fingerprint,
		BaselineFingerprint: "sha256:" + repeatHex(7),
		CreatedAt:           now,
		ExpiresAt:           now.Add(DefaultCandidateTTL),
		Status:              CandidateStateDraft,
	}
}
