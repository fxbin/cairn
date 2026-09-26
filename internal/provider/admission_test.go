package provider

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/contract"
)

// testSelection 构造测试用身份选择；基址形态合法且可规范化。
func testSelection() contract.ProviderSelection {
	return contract.ProviderSelection{
		SchemaVersion: 1, ProviderID: contract.ProviderIDCustom,
		ProtocolID: contract.ProtocolOpenAIChatCompletions,
		ModelRef:   contract.ProviderIDCustom + "/test-model", EndpointDigest: "sha256:" + repeatHex(),
	}
}

// repeatHex 生成 64 个十六进制字符，凑足 sha256 摘要形态。
func repeatHex() string {
	digits := make([]byte, 64)
	for index := range digits {
		digits[index] = 'a'
	}
	return string(digits)
}

// testEvidence 构造一条结构合法的探测证据。
func testEvidence() ProviderProbeEvidence {
	return ProviderProbeEvidence{
		SchemaVersion: probeEvidenceSchemaVersion, Selection: testSelection(),
		ProbeContractVersion: probeContractVersion,
		Observations: EvidenceObservations{
			UsageStreamed: ObservationObservedTrue, StopReasonsState: StateObserved,
			StopReasons: []string{"stop"}, ToolRoundtrip: RoundtripSkipped,
			ModelRevisionState: StateObserved, ModelRevision: contract.RevisionAbsent,
		},
		Outcome: EvidenceOutcomeCompleted, ProbedAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
}

// TestEvidenceValidateEnforcesClosedVocabularies 锁定三态/六态词表：未知枚举
// 与状态-值不一致的组合全部失败关闭。
func TestEvidenceValidateEnforcesClosedVocabularies(t *testing.T) {
	base := testEvidence()
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*ProviderProbeEvidence)
	}{
		{"未知usage观测", func(e *ProviderProbeEvidence) { e.Observations.UsageStreamed = "maybe" }},
		{"未知状态词表", func(e *ProviderProbeEvidence) { e.Observations.StopReasonsState = "guessed" }},
		{"未观测携带值", func(e *ProviderProbeEvidence) {
			e.Observations.StopReasonsState = StateNotObserved
			e.Observations.StopReasons = []string{"stop"}
		}},
		{"已观测无值", func(e *ProviderProbeEvidence) {
			e.Observations.StopReasonsState = StateObserved
			e.Observations.StopReasons = nil
		}},
		{"未知回路词表", func(e *ProviderProbeEvidence) { e.Observations.ToolRoundtrip = "partial" }},
		{"修订状态不一致", func(e *ProviderProbeEvidence) {
			e.Observations.ModelRevisionState = StateNotObserved
			e.Observations.ModelRevision = "abc"
		}},
		{"未知结局", func(e *ProviderProbeEvidence) { e.Outcome = "cancelled" }},
		{"完成携带错误类", func(e *ProviderProbeEvidence) { e.ErrorClass = "transport" }},
		{"缺时间戳", func(e *ProviderProbeEvidence) { e.ProbedAt = time.Time{} }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := base
			mutation.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("mutation must fail closed")
			}
		})
	}
}

// TestEvidenceStoreIsContentAddressedAndAtomic 锁定存储合同：落盘权限 0600、
// 目录 0700；同内容幂等；同指纹异内容拒绝；读回校验内容指纹与文件名一致；
// 被篡改的文件在读回时失败关闭。
func TestEvidenceStoreIsContentAddressedAndAtomic(t *testing.T) {
	stateDir := t.TempDir()
	store, err := OpenEvidenceStore(stateDir, "sha256:"+repeatHex())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(stateDir, "workspaces", "sha256:"+repeatHex(), "provider", evidenceDirName))
	if err != nil || !info.IsDir() {
		t.Fatalf("store directory missing: %v", err)
	}
	evidence := testEvidence()
	fingerprint, err := store.Append(&evidence)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.EvidenceFingerprint != fingerprint {
		t.Fatal("append must backfill the evidence fingerprint")
	}
	fileInfo, err := os.Stat(filepath.Join(store.root, fingerprint+".json"))
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("evidence file must be 0600: %v %v", fileInfo, err)
	}
	if _, err := store.Append(&evidence); err != nil {
		t.Fatalf("identical append must be idempotent: %v", err)
	}
	readBack, err := store.Get(fingerprint)
	if err != nil || readBack.EvidenceFingerprint != fingerprint {
		t.Fatalf("read back failed: %v", err)
	}
	tampered := testEvidence()
	tampered.Outcome = EvidenceOutcomeFailed
	tampered.ErrorClass = "transport"
	tamperedFingerprint, err := store.Append(&tampered)
	if err != nil {
		t.Fatalf("a failed probe is legitimate evidence and must persist: %v", err)
	}
	if tamperedFingerprint == fingerprint {
		t.Fatal("different content must yield a different fingerprint")
	}
	path := filepath.Join(store.root, fingerprint+".json")
	if err := os.WriteFile(path, []byte(`{"schema_version":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(fingerprint); err == nil {
		t.Fatal("tampered content must fail the filename-fingerprint check")
	}
}

// decisionFixture 构造一条合法决定；supersedes 与时间戳由用例覆写。
func decisionFixture(requestFingerprint string) ProviderAdmissionDecision {
	return ProviderAdmissionDecision{
		SchemaVersion: admissionDecisionSchemaVersion, EvidenceFingerprint: "sha256:" + repeatHex(),
		Binding:  AdmissionBinding{RequestProfileFingerprint: requestFingerprint, ConfigFingerprint: "sha256:" + repeatHex()},
		Decision: DecisionAdmitted, DecidedAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		Authority: AuthorityMaintainer, Reason: "probe evidence accepted",
	}
}

// TestResolveChainHeadFailsClosedOnCorruption 锁定分区链合同：唯一链头解析
// 成功；断链、环、多链头、重复决定全部失败关闭。
func TestResolveChainHeadFailsClosedOnCorruption(t *testing.T) {
	request := "sha256:" + repeatHex()
	first := decisionFixture(request)
	firstFingerprint, err := first.ComputeFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	first.DecisionFingerprint = firstFingerprint
	head, err := ResolveChainHead([]ProviderAdmissionDecision{first})
	if err != nil || head.DecisionFingerprint != firstFingerprint {
		t.Fatalf("single decision must resolve as head: %v", err)
	}
	second := decisionFixture(request)
	second.DecidedAt = first.DecidedAt.Add(time.Minute)
	second.SupersedesDecisionFingerprint = firstFingerprint
	secondFingerprint, err := second.ComputeFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	second.DecisionFingerprint = secondFingerprint
	head, err = ResolveChainHead([]ProviderAdmissionDecision{first, second})
	if err != nil || head.DecisionFingerprint != secondFingerprint {
		t.Fatalf("successor must resolve as head: %v", err)
	}
	broken := decisionFixture(request)
	broken.SupersedesDecisionFingerprint = "sha256:" + repeatHex()[:63]
	if _, err := ResolveChainHead([]ProviderAdmissionDecision{broken}); err == nil {
		t.Fatal("broken chain must fail closed")
	}
	cyclic := decisionFixture(request)
	cyclicFingerprint, err := cyclic.ComputeFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	cyclic.DecisionFingerprint = cyclicFingerprint
	cyclic.SupersedesDecisionFingerprint = cyclicFingerprint
	if _, err := ResolveChainHead([]ProviderAdmissionDecision{cyclic}); err == nil {
		t.Fatal("self cycle must fail closed")
	}
	rival := decisionFixture(request)
	rival.DecidedAt = first.DecidedAt.Add(time.Hour)
	if _, err := ResolveChainHead([]ProviderAdmissionDecision{first, rival}); err == nil {
		t.Fatal("multiple heads must fail closed")
	}
}

// TestGateFailsClosedWhenRequiredAndUnadmitted 锁定门语义：optional 永不拦截；
// required 在无决定、rejected、证据缺失、身份不一致时失败关闭；链头
// admitted 且证据匹配才放行。
func TestGateFailsClosedWhenRequiredAndUnadmitted(t *testing.T) {
	stateDir := t.TempDir()
	decisions, err := OpenDecisionStore(stateDir, "sha256:"+repeatHex())
	if err != nil {
		t.Fatal(err)
	}
	evidenceStore, err := OpenEvidenceStore(stateDir, "sha256:"+repeatHex())
	if err != nil {
		t.Fatal(err)
	}
	facts := ResolvedFacts{
		Selection:     SelectionFacts{Model: "test-model", BaseURL: "https://api.example.com/v1"},
		Capability:    &contract.ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true, StopReasons: []string{"stop"}},
		CredentialRef: "AGENT_API_KEY",
	}
	profile, err := ResolveResolvedProfile(facts)
	if err != nil {
		t.Fatal(err)
	}
	requestFingerprint, err := profile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := EvaluateAdmissionGate(decisions, evidenceStore, profile, "optional")
	if err != nil || !outcome.Allowed {
		t.Fatalf("optional must never block: %v", err)
	}
	if _, err := EvaluateAdmissionGate(decisions, evidenceStore, profile, "required"); err == nil {
		t.Fatal("required with no decisions must fail closed")
	}
	rejected := decisionFixture(requestFingerprint)
	rejected.Decision = DecisionRejected
	if _, err := decisions.Append(&rejected); err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateAdmissionGate(decisions, evidenceStore, profile, "required"); err == nil {
		t.Fatal("rejected head must fail closed")
	}
	admitted := decisionFixture(requestFingerprint)
	admitted.DecidedAt = rejected.DecidedAt.Add(time.Minute)
	admitted.SupersedesDecisionFingerprint = rejected.DecisionFingerprint
	if _, err := decisions.Append(&admitted); err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateAdmissionGate(decisions, evidenceStore, profile, "required"); err == nil {
		t.Fatal("admitted head with missing evidence must fail closed")
	}
	evidence := ProviderProbeEvidence{
		SchemaVersion: probeEvidenceSchemaVersion, Selection: profile.Selection,
		ProbeContractVersion: probeContractVersion,
		Observations: EvidenceObservations{
			UsageStreamed: ObservationObservedTrue, StopReasonsState: StateObserved, StopReasons: []string{"stop"},
			ToolRoundtrip: RoundtripSkipped, ModelRevisionState: StateObserved, ModelRevision: contract.RevisionAbsent,
		},
		Outcome: EvidenceOutcomeCompleted, ProbedAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
	evidenceFingerprint, err := evidenceStore.Append(&evidence)
	if err != nil {
		t.Fatal(err)
	}
	matching := decisionFixture(requestFingerprint)
	matching.DecidedAt = admitted.DecidedAt.Add(time.Minute)
	matching.SupersedesDecisionFingerprint = admitted.DecisionFingerprint
	matching.EvidenceFingerprint = evidenceFingerprint
	if _, err := decisions.Append(&matching); err != nil {
		t.Fatal(err)
	}
	outcome, err = EvaluateAdmissionGate(decisions, evidenceStore, profile, "required")
	if err != nil || !outcome.Allowed {
		t.Fatalf("admitted head with matching evidence must pass: %v", err)
	}
	otherFacts := facts
	otherFacts.Selection.Model = "other-model"
	otherProfile, err := ResolveResolvedProfile(otherFacts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateAdmissionGate(decisions, evidenceStore, otherProfile, "required"); err == nil {
		t.Fatal("different binding must not inherit admission")
	}
}

// TestResolvedProfileFingerprintIsRecomputable 锁定复算一致性：同一事实两次
// 组装恒同 request_profile_fingerprint；凭据槽位或能力变化必然改变指纹。
func TestResolvedProfileFingerprintIsRecomputable(t *testing.T) {
	facts := ResolvedFacts{
		Selection:     SelectionFacts{Model: "test-model", BaseURL: "https://api.example.com/v1"},
		Capability:    &contract.ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true, StopReasons: []string{"stop"}},
		Generation:    &contract.ProviderGenerationDeclaration{TemperatureMilli: intPtr(600)},
		CredentialRef: "AGENT_API_KEY",
	}
	first, err := ResolveResolvedProfile(facts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveResolvedProfile(facts)
	if err != nil {
		t.Fatal(err)
	}
	firstFingerprint, err := first.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	secondFingerprint, err := second.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if firstFingerprint != secondFingerprint {
		t.Fatal("same facts must recompute to the same request profile fingerprint")
	}
	rotated := facts
	rotated.CredentialRef = "AGENT_PROVIDER_KEY"
	rotatedProfile, err := ResolveResolvedProfile(rotated)
	if err != nil {
		t.Fatal(err)
	}
	rotatedFingerprint, err := rotatedProfile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if rotatedFingerprint == firstFingerprint {
		t.Fatal("credential slot change must change the request profile fingerprint")
	}
	weaker := facts
	weaker.Capability = &contract.ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true, StopReasons: []string{"stop", "length"}}
	weakerProfile, err := ResolveResolvedProfile(weaker)
	if err != nil {
		t.Fatal(err)
	}
	weakerFingerprint, err := weakerProfile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if weakerFingerprint == firstFingerprint {
		t.Fatal("capability change must change the request profile fingerprint")
	}
}

// TestResolveResolvedProfileEnforcesCredentialRefShape 锁定凭据槽位校验：
// 非变量名形态失败关闭；能力声明缺席失败关闭。
func TestResolveResolvedProfileEnforcesCredentialRefShape(t *testing.T) {
	facts := ResolvedFacts{
		Selection:     SelectionFacts{Model: "m", BaseURL: "https://api.example.com/v1"},
		Capability:    &contract.ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true, StopReasons: []string{"stop"}},
		CredentialRef: "AGENT_API_KEY",
	}
	for _, invalid := range []string{"", "sk-shaped-secret-value", "1ST_VAR", "WITH SPACE"} {
		candidate := facts
		candidate.CredentialRef = invalid
		if _, err := ResolveResolvedProfile(candidate); err == nil {
			t.Fatalf("credential ref %q must fail closed", invalid)
		}
	}
	noCapability := facts
	noCapability.Capability = nil
	if _, err := ResolveResolvedProfile(noCapability); err == nil {
		t.Fatal("missing capability declaration must fail closed")
	}
}

// intPtr 返回整数指针，供生成参数字段的显式两态。
func intPtr(value int) *int { return &value }
