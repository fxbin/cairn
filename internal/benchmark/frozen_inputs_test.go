package benchmark

import (
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// TestResolvedExecutionPlanDigestSensitivity 锁定解析计划摘要合同：空计划拒
// 绝产出，任意记录的值或来源漂移都改变摘要。
func TestResolvedExecutionPlanDigestSensitivity(t *testing.T) {
	base := []config.ResolutionEntry{
		{Field: "budget.max_turns", Value: "80", Source: "profile:complex"},
		{Field: "provider.model", Value: "test-model", Source: "yaml"},
	}
	first, err := ResolvedExecutionPlanDigest(base)
	if err != nil || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("digest fixture: %q err=%v", first, err)
	}
	same, err := ResolvedExecutionPlanDigest(append([]config.ResolutionEntry(nil), base...))
	if err != nil || same != first {
		t.Fatalf("identical plans must share the digest: %q vs %q (%v)", first, same, err)
	}
	valueDrift := append([]config.ResolutionEntry(nil), base...)
	valueDrift[0].Value = "40"
	drifted, err := ResolvedExecutionPlanDigest(valueDrift)
	if err != nil {
		t.Fatalf("value drift digest: %v", err)
	}
	if drifted == first {
		t.Fatal("value drift must change the digest")
	}
	sourceDrift := append([]config.ResolutionEntry(nil), base...)
	sourceDrift[1].Source = "cli"
	sourceChanged, err := ResolvedExecutionPlanDigest(sourceDrift)
	if err != nil {
		t.Fatalf("source drift digest: %v", err)
	}
	if sourceChanged == first {
		t.Fatal("source drift must change the digest")
	}
	if _, err := ResolvedExecutionPlanDigest(nil); err == nil {
		t.Fatal("an empty plan must fail closed")
	}
}

// TestBuildIdentityComputeFingerprintMatchesLegacyShape 锁定构建身份摘要：与
// 既有 Run 冻结事实同式同值，dirty 三态之外的身份变化都会改变摘要。
func TestBuildIdentityComputeFingerprintMatchesLegacyShape(t *testing.T) {
	identity := domain.BuildIdentity{Revision: "abc123", Dirty: "false", Flavor: "plain"}
	first := identity.ComputeFingerprint()
	// legacyBuildIdentityFixtureGolden 是按既有 Run 冻结事实的公式独立预计算
	// 的锚定值：公式或字段序静默漂移都会在此立即失败。
	const legacyBuildIdentityFixtureGolden = "sha256:26d81a221b5790c0ce969fac0bf2d42386993c7519d498e98a7ed2e39589dee4"
	if first != legacyBuildIdentityFixtureGolden {
		t.Fatalf("fingerprint must stay pinned to the legacy formula: %q vs %q", first, legacyBuildIdentityFixtureGolden)
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("fingerprint shape invalid: %q", first)
	}
	if first != identity.ComputeFingerprint() {
		t.Fatal("fingerprint must be deterministic")
	}
	dirty := identity
	dirty.Dirty = "unknown"
	if dirty.ComputeFingerprint() == first {
		t.Fatal("dirty status drift must change the fingerprint")
	}
	revision := identity
	revision.Revision = "def456"
	if revision.ComputeFingerprint() == first {
		t.Fatal("revision drift must change the fingerprint")
	}
}
