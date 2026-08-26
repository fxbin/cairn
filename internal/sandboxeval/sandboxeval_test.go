package sandboxeval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validDigest 是测试共用的合法 sha256 形态摘要。
const validDigest = "sha256:5f9b8f0f0e37dfb26c29a0a21be29d1d97d04be563ad98300ce9d98e197dbe94"

// validLease 返回一个通过全部自检的租约。
func validLease() CredentialLease {
	return CredentialLease{
		SchemaVersion: leaseSchemaV1, LeaseID: "lease-01", CredentialProfileID: "sandbox-subaccount",
		ProviderIdentityFingerprint: validDigest, EnvName: SandboxProviderKeyEnv,
		IssuedAt: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
		MaxRequests: 1000, MaxInputTokens: 10_000_000, MaxOutputTokens: 10_000_000, MaxCostMicrousd: 100_000_000,
		ServerSideQuotaEnabled: true, Revocable: true,
	}
}

// validPolicy 返回一个通过全部自检的策略。
func validPolicy() CredentialPolicy {
	return CredentialPolicy{
		SchemaVersion: policySchemaV1, EnvName: SandboxProviderKeyEnv,
		AllowedCredentialProfileIDs: []string{"sandbox-subaccount", "backup-subaccount"},
		ProviderIdentityFingerprint: validDigest, SuiteMaxWallMilliseconds: 3 * 60 * 60 * 1000,
		MinRequests: 200, MinInputTokens: 5_000_000, MinOutputTokens: 1_000_000, MinCostMicrousd: 50_000_000,
	}
}

// TestCredentialLeaseValidationFailsClosed 表驱动锁定租约自检的全部拒绝条件。
func TestCredentialLeaseValidationFailsClosed(t *testing.T) {
	mutations := map[string]func(*CredentialLease){
		"bad schema":      func(l *CredentialLease) { l.SchemaVersion = 99 },
		"empty lease id":  func(l *CredentialLease) { l.LeaseID = "" },
		"empty profile":   func(l *CredentialLease) { l.CredentialProfileID = "" },
		"digest shape":    func(l *CredentialLease) { l.ProviderIdentityFingerprint = "abc" },
		"wrong env name":  func(l *CredentialLease) { l.EnvName = "OPENAI_API_KEY" },
		"inverted window": func(l *CredentialLease) { l.IssuedAt, l.ExpiresAt = l.ExpiresAt, l.IssuedAt },
		"zero requests":   func(l *CredentialLease) { l.MaxRequests = 0 },
		"negative tokens": func(l *CredentialLease) { l.MaxInputTokens = -1 },
		"zero cost":       func(l *CredentialLease) { l.MaxCostMicrousd = 0 },
		"no server quota": func(l *CredentialLease) { l.ServerSideQuotaEnabled = false },
		"not revocable":   func(l *CredentialLease) { l.Revocable = false },
	}
	for name, mutate := range mutations {
		lease := validLease()
		mutate(&lease)
		if err := lease.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	if err := validLease().Validate(); err != nil {
		t.Fatalf("valid lease rejected: %v", err)
	}
}

// TestCredentialPolicyValidationAndNormalization 验证策略自检与账号标识规范化：
// 空集合与空标识被拒，去重排序稳定输出。
func TestCredentialPolicyValidationAndNormalization(t *testing.T) {
	policy := validPolicy()
	if err := policy.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	policy.AllowedCredentialProfileIDs = []string{"backup-subaccount", "sandbox-subaccount", "backup-subaccount"}
	normalized, err := policy.NormalizedCredentialProfileIDs()
	if err != nil {
		t.Fatalf("normalize ids: %v", err)
	}
	if strings.Join(normalized, ",") != "backup-subaccount,sandbox-subaccount" {
		t.Fatalf("normalization mismatch: %v", normalized)
	}
	empty := validPolicy()
	empty.AllowedCredentialProfileIDs = nil
	if err := empty.Validate(); err == nil {
		t.Fatal("policy without allowed profiles must be rejected")
	}
	negative := validPolicy()
	negative.MinRequests = -1
	if err := negative.Validate(); err == nil {
		t.Fatal("negative minimum quota must be rejected")
	}
	zeroWall := validPolicy()
	zeroWall.SuiteMaxWallMilliseconds = 0
	if err := zeroWall.Validate(); err == nil {
		t.Fatal("non-positive suite wall time must be rejected")
	}
}

// TestPolicyFingerprintIgnoresLeaseInstance 锁定指纹合同：同一策略对任意不同
// 的租约实例身份都得到同一指纹；策略字段漂移改变指纹。
func TestPolicyFingerprintIgnoresLeaseInstance(t *testing.T) {
	policy := validPolicy()
	first, err := policy.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint policy: %v", err)
	}
	reordered := policy
	reordered.AllowedCredentialProfileIDs = []string{"backup-subaccount", "sandbox-subaccount"}
	same, err := reordered.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint reordered: %v", err)
	}
	if same != first {
		t.Fatal("id order must not change the policy fingerprint")
	}
	drifted := policy
	drifted.SuiteMaxWallMilliseconds += 1
	other, err := drifted.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint drifted: %v", err)
	}
	if other == first {
		t.Fatal("suite wall time drift must change the fingerprint")
	}
}

// TestAuthorizeLeaseAcceptsOnlyFullCoverage 覆盖授权的通过路径与每一条拒绝
// 路径：到期不足、签发晚于套件开始、账号未放行、Provider 身份不一致与任一
// 配额下限缺口都必须在启动前失败。
func TestAuthorizeLeaseAcceptsOnlyFullCoverage(t *testing.T) {
	suiteStart := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := AuthorizeLease(validLease(), validPolicy(), suiteStart); err != nil {
		t.Fatalf("valid pair rejected: %v", err)
	}
	shortLived := validLease()
	shortLived.ExpiresAt = suiteStart.Add(time.Hour)
	if err := AuthorizeLease(shortLived, validPolicy(), suiteStart); err == nil {
		t.Fatal("lease expiring before the suite deadline must be rejected")
	}
	futureIssued := validLease()
	futureIssued.IssuedAt = suiteStart.Add(time.Minute)
	if err := AuthorizeLease(futureIssued, validPolicy(), suiteStart); err == nil {
		t.Fatal("lease issued after the suite start must be rejected")
	}
	foreignProfile := validLease()
	foreignProfile.CredentialProfileID = "unlisted-account"
	if err := AuthorizeLease(foreignProfile, validPolicy(), suiteStart); err == nil {
		t.Fatal("unlisted credential profile must be rejected")
	}
	foreignProvider := validLease()
	foreignProvider.ProviderIdentityFingerprint = validDigest[:len(validDigest)-4] + "0000"
	if err := AuthorizeLease(foreignProvider, validPolicy(), suiteStart); err == nil {
		t.Fatal("provider identity mismatch must be rejected")
	}
	exhausted := validPolicy()
	exhausted.MinOutputTokens = 11_000_000
	if err := AuthorizeLease(validLease(), exhausted, suiteStart); err == nil || !strings.Contains(err.Error(), "output tokens") {
		t.Fatalf("insufficient output quota must be reported: %v", err)
	}
	badPolicy := validPolicy()
	badPolicy.EnvName = "OTHER_KEY"
	if err := AuthorizeLease(validLease(), badPolicy, suiteStart); err == nil {
		t.Fatal("invalid policy must fail before lease checks")
	}
}

// TestBuildEvaluationEnvironmentClosesLeakPaths 锁定启动器环境构造：白名单取
// 值注入并稳定排序、沙箱 Key 经专用通道携带、凭据样白名单项/用户目录/Key
// 名称混入/缺失 Key 全部拒绝。
func TestBuildEvaluationEnvironmentClosesLeakPaths(t *testing.T) {
	hostEnv := map[string]string{"PATH": "/usr/bin:/bin", "LANG": "en_US.UTF-8", "TMPDIR": "/tmp/cairn"}
	source := func(name string) (string, bool) { value, ok := hostEnv[name]; return value, ok }
	environ, err := BuildEvaluationEnvironment(source, []string{"TMPDIR", "PATH", "LANG"}, "sk-sandbox")
	if err != nil {
		t.Fatalf("build environment: %v", err)
	}
	want := []string{"LANG=en_US.UTF-8", "PATH=/usr/bin:/bin", "TMPDIR=/tmp/cairn", SandboxProviderKeyEnv + "=sk-sandbox"}
	if strings.Join(environ, "|") != strings.Join(want, "|") {
		t.Fatalf("environment mismatch: %v", environ)
	}
	if _, err := BuildEvaluationEnvironment(source, []string{"AGENT_API_KEY"}, "k"); err == nil {
		t.Fatal("credential-like allowlist entry must be rejected")
	}
	if _, err := BuildEvaluationEnvironment(source, []string{"HOME"}, "k"); err == nil {
		t.Fatal("user directory allowlist entry must be rejected")
	}
	if _, err := BuildEvaluationEnvironment(source, []string{SandboxProviderKeyEnv}, "k"); err == nil {
		t.Fatal("sandbox key in allowlist must be rejected")
	}
	if _, err := BuildEvaluationEnvironment(source, []string{"PATH"}, ""); err == nil {
		t.Fatal("missing provider key must be rejected")
	}
	minimal, err := BuildEvaluationEnvironment(func(string) (string, bool) { return "", false }, []string{"PATH"}, "sk-x")
	if err != nil || len(minimal) != 1 || !strings.HasPrefix(minimal[0], SandboxProviderKeyEnv+"=") {
		t.Fatalf("absent host variables must leave only the key channel: %v %v", minimal, err)
	}
}

// TestAuditEvaluationEnvironmentReportsInheritance 验证隔离审计：用户目录、代
// 理与凭据样继承逐一名单化，沙箱 Key 自身豁免，输出保持排序稳定。
func TestAuditEvaluationEnvironmentReportsInheritance(t *testing.T) {
	environ := []string{
		SandboxProviderKeyEnv + "=sk-ok",
		"PATH=/usr/bin",
		"HOME=/Users/someone",
		"HTTPS_PROXY=http://127.0.0.1:7890",
		"AWS_SECRET_ACCESS_KEY=leak",
	}
	violations := AuditEvaluationEnvironment(environ)
	want := "AWS_SECRET_ACCESS_KEY,HOME,HTTPS_PROXY"
	if strings.Join(violations, ",") != want {
		t.Fatalf("violations = %v, want %s", violations, want)
	}
	cleanEnviron := []string{"PATH=/usr/bin", SandboxProviderKeyEnv + "=sk-ok"}
	if got := AuditEvaluationEnvironment(cleanEnviron); len(got) != 0 {
		t.Fatalf("clean environment flagged: %v", got)
	}
}

// TestTaskEnvironmentViolationGuardsSandboxKey 验证任务进程环境禁止携带沙箱
// Key，其他显式注入不受影响。
func TestTaskEnvironmentViolationGuardsSandboxKey(t *testing.T) {
	if err := TaskEnvironmentViolation([]string{"TMPDIR", SandboxProviderKeyEnv}); err == nil {
		t.Fatal("sandbox key visible to task processes must be a violation")
	}
	if err := TaskEnvironmentViolation([]string{"TMPDIR", "GOFLAGS"}); err != nil {
		t.Fatalf("ordinary injection rejected: %v", err)
	}
}

// TestScanPersistenceSurfacesClosure 覆盖泄漏扫描三态：干净表面返回 clean，
// 命中按文件计数且只出现文件名与次数，无法读取的表面整体失败关闭。
func TestScanPersistenceSurfacesClosure(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "events.sqlite")
	if err := os.WriteFile(journal, []byte("payload with sk-live-secret inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(dir, "artifacts.jsonl")
	if err := os.WriteFile(artifacts, []byte("{\"text\":\"no secret here\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := ScanPersistenceSurfaces([]string{journal, artifacts}, []byte("sk-live-secret"))
	if err != nil {
		t.Fatalf("scan surfaces: %v", err)
	}
	if report.Outcome != LeakDetected || report.Hits["events.sqlite"] != 1 || len(report.Hits) != 1 {
		t.Fatalf("unexpected report: %#v", report)
	}
	clean, err := ScanPersistenceSurfaces([]string{artifacts}, []byte("sk-live-secret"))
	if err != nil || clean.Outcome != LeakClean || len(clean.Hits) != 0 {
		t.Fatalf("clean scan mismatch: %#v %v", clean, err)
	}
	missing := filepath.Join(dir, "does-not-exist")
	if _, err := ScanPersistenceSurfaces([]string{missing}, []byte("x")); err == nil {
		t.Fatal("unreadable surface must fail closed instead of claiming clean")
	}
	if _, err := ScanPersistenceSurfaces(nil, nil); err == nil {
		t.Fatal("scan without an in-memory secret must be rejected")
	}
}
