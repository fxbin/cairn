package contract

import (
	"strings"
	"testing"
)

// resolvedCardDeclaration 返回通过合同检查的最小显式能力声明。
func resolvedCardDeclaration() *ProviderCapabilityDeclaration {
	return &ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true, ContextWindowTokens: 131072, MaxOutputTokens: 8192}
}

// TestResolveProviderRequestProfileProjectsClosedContract 锁定请求合同的投影：
// 能力卡指纹进入 profile，修订缺席显式标记，生成参数两态经 canonical 形式
// 可区分；相同声明恒得相同字段。
func TestResolveProviderRequestProfileProjectsClosedContract(t *testing.T) {
	temperature := 700
	declaration := ProviderRequestDeclaration{
		BaseURL: "https://Provider.Example/v4/", Model: "test-model", Capability: resolvedCardDeclaration(),
		Generation: &ProviderGenerationDeclaration{MaxOutputTokens: 4096, TemperatureMilli: &temperature, ReasoningEffort: ReasoningEffortMedium},
	}
	profile, err := ResolveProviderRequestProfile(ProviderKindOpenAICompatible, declaration)
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	if profile.SchemaVersion != providerProfileSchemaV1 || profile.ProviderKind != ProviderKindOpenAICompatible || profile.Model != "test-model" {
		t.Fatalf("identity fields = %#v", profile)
	}
	if !strings.HasPrefix(profile.BaseURLIdentityDigest, "sha256:") || !strings.HasPrefix(profile.CapabilityFingerprint, "sha256:") {
		t.Fatalf("digests = %q / %q", profile.BaseURLIdentityDigest, profile.CapabilityFingerprint)
	}
	if profile.ProviderModelRevision != RevisionAbsent {
		t.Fatalf("missing revision must be explicitly absent, got %q", profile.ProviderModelRevision)
	}
	if profile.GenerationOptions.MaxOutputTokens != 4096 || profile.GenerationOptions.TemperatureMilli == nil || *profile.GenerationOptions.TemperatureMilli != 700 || profile.GenerationOptions.ReasoningEffort != ReasoningEffortMedium {
		t.Fatalf("generation options = %#v", profile.GenerationOptions)
	}
	other, err := ResolveProviderRequestProfile(ProviderKindOpenAICompatible, declaration)
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if other != profile {
		t.Fatal("same declaration must resolve to an identical profile")
	}
	revised := declaration
	revised.ModelRevision = "2026-08-26"
	withRevision, err := ResolveProviderRequestProfile(ProviderKindOpenAICompatible, revised)
	if err != nil {
		t.Fatalf("resolve revision: %v", err)
	}
	if withRevision.ProviderModelRevision != "2026-08-26" {
		t.Fatalf("explicit revision lost: %q", withRevision.ProviderModelRevision)
	}
}

// TestResolveProviderRequestProfileFailsClosed 表驱动锁定失败关闭路径：未知
// 品类、空模型、未声明能力卡、内嵌凭据地址与越界 Milli 都不得产出 profile。
func TestResolveProviderRequestProfileFailsClosed(t *testing.T) {
	cases := map[string]ProviderRequestDeclaration{
		"unknown kind":        {BaseURL: "https://p.test/v1", Model: "m"},
		"empty model":         {BaseURL: "https://p.test/v1", Capability: resolvedCardDeclaration()},
		"capability absent":   {BaseURL: "https://p.test/v1", Model: "m"},
		"false capability":    {BaseURL: "https://p.test/v1", Model: "m", Capability: &ProviderCapabilityDeclaration{}},
		"userinfo base url":   {BaseURL: "https://key@p.test/v1", Model: "m", Capability: resolvedCardDeclaration()},
		"query in base url":   {BaseURL: "https://p.test/v1?k=1", Model: "m", Capability: resolvedCardDeclaration()},
		"empty base url":      {Model: "m", Capability: resolvedCardDeclaration()},
		"temperature outlier": genCase(intPtr(2001), nil),
		"top p outlier":       genCase(nil, intPtr(1001)),
		"output over card":    {BaseURL: "https://p.test/v1", Model: "m", Capability: resolvedCardDeclaration(), Generation: &ProviderGenerationDeclaration{MaxOutputTokens: 8193}},
	}
	for name, declaration := range cases {
		if _, err := ResolveProviderRequestProfile(ProviderKindOpenAICompatible, declaration); err == nil {
			t.Fatalf("%s must fail closed", name)
		}
	}
}

// genCase 构造带越界 Milli 值的声明，供表驱动复用。
func genCase(temperature *int, topP *int) ProviderRequestDeclaration {
	return ProviderRequestDeclaration{
		BaseURL: "https://p.test/v1", Model: "m", Capability: resolvedCardDeclaration(),
		Generation: &ProviderGenerationDeclaration{TemperatureMilli: temperature, TopPMilli: topP},
	}
}

// intPtr 返回整数指针的小工具。
func intPtr(value int) *int { return &value }

// TestBaseURLIdentityDigestNormalization 锁定身份摘要的归一化语义：scheme 与
// host 大小写、尾斜杠不改变端点身份，路径差异必须改变摘要。
func TestBaseURLIdentityDigestNormalization(t *testing.T) {
	lower, err := BaseURLIdentityDigest("https://provider.example/v4")
	if err != nil {
		t.Fatalf("digest lower: %v", err)
	}
	upper, err := BaseURLIdentityDigest("HTTPS://PROVIDER.EXAMPLE/v4/")
	if err != nil {
		t.Fatalf("digest upper: %v", err)
	}
	if lower != upper {
		t.Fatalf("normalization diverges: %s vs %s", lower, upper)
	}
	otherPath, err := BaseURLIdentityDigest("https://provider.example/api/paas")
	if err != nil {
		t.Fatalf("digest other path: %v", err)
	}
	if otherPath == lower {
		t.Fatal("different endpoint paths must not share identity")
	}
}

// TestCapabilityCardFingerprintSensitivity 验证能力卡指纹对停止原因集合敏感，
// 且空声明回退后的卡与显式协议全集卡同指纹。
func TestCapabilityCardFingerprintSensitivity(t *testing.T) {
	first, err := ResolveProviderCard(ProviderKindOpenAICompatible, "m", resolvedCardDeclaration())
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	digestOne, err := first.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint one: %v", err)
	}
	digestAgain, err := first.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint again: %v", err)
	}
	if digestOne != digestAgain {
		t.Fatal("same card fingerprint must be deterministic")
	}
	second := first
	second.StopReasons = []string{"stop"}
	digestTwo, err := second.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint two: %v", err)
	}
	if digestTwo == digestOne {
		t.Fatal("stop reason changes must change the capability fingerprint")
	}
}

// TestNewPricingSnapshotConvertsExactly 锁定 USD/token 到 micro-USD/M token 的
// 精确换算：常规价、零价与超低价都必须无损折算。
func TestNewPricingSnapshotConvertsExactly(t *testing.T) {
	snapshot, err := NewPricingSnapshot(0.00000105, 0.0000022)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.InputPriceMicrousdPerMillionTokens != 1_050_000 || snapshot.OutputPriceMicrousdPerMillionTokens != 2_200_000 {
		t.Fatalf("conversion mismatch: %#v", snapshot)
	}
	free, err := NewPricingSnapshot(0, 0)
	if err != nil {
		t.Fatalf("zero pricing: %v", err)
	}
	if free.InputPriceMicrousdPerMillionTokens != 0 {
		t.Fatalf("zero price must convert to zero, got %d", free.InputPriceMicrousdPerMillionTokens)
	}
	digest, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("fingerprint format: %q", digest)
	}
}

// TestNewPricingSnapshotRejectsInexactOrInvalid 验证负价与非十进制结果、以及
// 位移后无法得到整数的超小单价全部失败关闭，绝不舍入。
func TestNewPricingSnapshotRejectsInexactOrInvalid(t *testing.T) {
	subMicro := 1e-13
	cases := []float64{-0.5, subMicro}
	for _, rate := range cases {
		if _, err := NewPricingSnapshot(rate, rate); err == nil {
			t.Fatalf("rate %v must be rejected", rate)
		}
	}
}
