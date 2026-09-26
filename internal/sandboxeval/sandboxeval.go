// Package sandboxeval 承载 M4-2 沙箱评测的可信控制面合同：专用凭据租约、
// 冻结凭据策略、可信启动器环境构造与持久化表面泄漏扫描。本包全部为纯
// 合同与检测函数——不发起网络请求、不启动进程、不持有密钥之外的状态；
// 任何校验不过都必须让候选保持在 ready，不得留下"评测已开始"的痕迹。
//
// Author: fxbin
package sandboxeval

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

// SandboxProviderKeyEnv 是沙箱 Provider Key 的固定注入变量名：冻结 Agent 配置
// 的 provider.api_key_env 必须精确等于该名称；它不由候选、任务合同或外部
// 输入选择。
const SandboxProviderKeyEnv = "CAIRN_SANDBOX_PROVIDER_KEY"

// leaseSchemaVersion 与 policySchemaVersion 分别是租约描述与凭据策略的当前 schema 版本。
const (
	leaseSchemaVersion  = 1
	policySchemaVersion = 1
)

// credentialPolicyPrefix 是凭据策略指纹的版本化域前缀。
const credentialPolicyPrefix = "cairn-sandbox-credential-policy-v1"

// CredentialLease 是不含密钥的租约描述：Key 本体只存在于可信父进程内存与
// 评测子进程环境；租约实例身份（lease_id）只记录在本地忽略的评测状态，
// 绝不进入 Memory、事件或制品。数值上限构成整套评测的第二道总量边界。
type CredentialLease struct {
	SchemaVersion               int       `json:"schema_version"`
	LeaseID                     string    `json:"lease_id"`
	CredentialProfileID         string    `json:"credential_profile_id"`
	ProviderIdentityFingerprint string    `json:"provider_identity_fingerprint"`
	EnvName                     string    `json:"env_name"`
	IssuedAt                    time.Time `json:"issued_at"`
	ExpiresAt                   time.Time `json:"expires_at"`
	MaxRequests                 int64     `json:"max_requests"`
	MaxInputTokens              int64     `json:"max_input_tokens"`
	MaxOutputTokens             int64     `json:"max_output_tokens"`
	MaxCostMicrousd             int64     `json:"max_cost_microusd"`
	ServerSideQuotaEnabled      bool      `json:"server_side_quota_enabled"`
	Revocable                   bool      `json:"revocable"`
}

// CredentialPolicy 是 EvalSet 冻结的凭据策略：固定注入名、允许的沙箱账号
// 标识、Provider 身份、套件最长运行时间与完成全部 Run 所需的最小配额。
// 它不含租约实例身份与时间戳，指纹即由本结构计算。
type CredentialPolicy struct {
	SchemaVersion               int      `json:"schema_version"`
	EnvName                     string   `json:"env_name"`
	AllowedCredentialProfileIDs []string `json:"allowed_credential_profile_ids"`
	ProviderIdentityFingerprint string   `json:"provider_identity_fingerprint"`
	SuiteMaxWallMilliseconds    int64    `json:"suite_max_wall_milliseconds"`
	MinRequests                 int64    `json:"min_requests"`
	MinInputTokens              int64    `json:"min_input_tokens"`
	MinOutputTokens             int64    `json:"min_output_tokens"`
	MinCostMicrousd             int64    `json:"min_cost_microusd"`
}

// fingerprintShape 报告字符串是否符合 "sha256:<64 位十六进制>" 的摘要形态。
func fingerprintShape(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	hexPart := strings.TrimPrefix(value, "sha256:")
	if len(hexPart) != 64 {
		return false
	}
	for index := range len(hexPart) {
		c := hexPart[index]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Validate 校验租约的自身一致性：schema 版本、非空身份字段、固定注入名、
// 时间窗方向、正数总量上限与服务端配额/可撤销硬要求。跨对象约束（覆盖
// 策略下限、晚于套件截止）在 AuthorizeLease 中统一失败关闭。
func (lease CredentialLease) Validate() error {
	if lease.SchemaVersion != leaseSchemaVersion {
		return fmt.Errorf("lease schema_version %d is not supported", lease.SchemaVersion)
	}
	if lease.LeaseID == "" || lease.CredentialProfileID == "" {
		return errors.New("lease identity fields must not be empty")
	}
	if !fingerprintShape(lease.ProviderIdentityFingerprint) {
		return fmt.Errorf("lease provider_identity_fingerprint %q is not a sha256 digest", lease.ProviderIdentityFingerprint)
	}
	if lease.EnvName != SandboxProviderKeyEnv {
		return fmt.Errorf("lease env_name must be %s", SandboxProviderKeyEnv)
	}
	if !lease.IssuedAt.Before(lease.ExpiresAt) {
		return errors.New("lease expiry must be later than issuance")
	}
	for name, value := range map[string]int64{
		"max_requests": lease.MaxRequests, "max_input_tokens": lease.MaxInputTokens,
		"max_output_tokens": lease.MaxOutputTokens, "max_cost_microusd": lease.MaxCostMicrousd,
	} {
		if value <= 0 {
			return fmt.Errorf("lease %s must be positive", name)
		}
	}
	if !lease.ServerSideQuotaEnabled {
		return errors.New("lease requires server-side quota enforcement")
	}
	if !lease.Revocable {
		return errors.New("lease requires server-side revocation")
	}
	return nil
}

// Validate 校验凭据策略自身一致性：固定注入名、去重排序后非空的账号标识、
// Provider 身份摘要形态、正数的套件时长与非负的最小需求。
func (policy CredentialPolicy) Validate() error {
	if policy.SchemaVersion != policySchemaVersion {
		return fmt.Errorf("policy schema_version %d is not supported", policy.SchemaVersion)
	}
	if policy.EnvName != SandboxProviderKeyEnv {
		return fmt.Errorf("policy env_name must be %s", SandboxProviderKeyEnv)
	}
	if _, err := policy.NormalizedCredentialProfileIDs(); err != nil {
		return err
	}
	if !fingerprintShape(policy.ProviderIdentityFingerprint) {
		return fmt.Errorf("policy provider_identity_fingerprint %q is not a sha256 digest", policy.ProviderIdentityFingerprint)
	}
	if policy.SuiteMaxWallMilliseconds <= 0 {
		return errors.New("suite_max_wall_milliseconds must be positive")
	}
	for name, value := range map[string]int64{
		"min_requests": policy.MinRequests, "min_input_tokens": policy.MinInputTokens,
		"min_output_tokens": policy.MinOutputTokens, "min_cost_microusd": policy.MinCostMicrousd,
	} {
		if value < 0 {
			return fmt.Errorf("policy %s cannot be negative", name)
		}
	}
	return nil
}

// NormalizedCredentialProfileIDs 返回去重并按字节序排序后的账号标识集合；
// 空集合属于策略损坏，返回错误。
func (policy CredentialPolicy) NormalizedCredentialProfileIDs() ([]string, error) {
	if len(policy.AllowedCredentialProfileIDs) == 0 {
		return nil, errors.New("policy must allow at least one credential profile id")
	}
	seen := make(map[string]struct{}, len(policy.AllowedCredentialProfileIDs))
	sorted := make([]string, 0, len(policy.AllowedCredentialProfileIDs))
	for _, id := range policy.AllowedCredentialProfileIDs {
		if id == "" {
			return nil, errors.New("credential profile ids cannot be empty")
		}
		if _, duplicated := seen[id]; duplicated {
			continue
		}
		seen[id] = struct{}{}
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	return sorted, nil
}

// Fingerprint 以版本化前缀计算凭据策略指纹：账号标识先规范化排序，策略
// 结构本身不含 Key、lease_id 或租约时间戳，因此指纹可安全进入 EvalSet。
func (policy CredentialPolicy) Fingerprint() (string, error) {
	if err := policy.Validate(); err != nil {
		return "", fmt.Errorf("cannot fingerprint invalid policy: %w", err)
	}
	ids, err := policy.NormalizedCredentialProfileIDs()
	if err != nil {
		return "", err
	}
	view := struct {
		SchemaVersion               int      `json:"schema_version"`
		EnvName                     string   `json:"env_name"`
		AllowedCredentialProfileIDs []string `json:"allowed_credential_profile_ids"`
		ProviderIdentityFingerprint string   `json:"provider_identity_fingerprint"`
		SuiteMaxWallMilliseconds    int64    `json:"suite_max_wall_milliseconds"`
		MinRequests                 int64    `json:"min_requests"`
		MinInputTokens              int64    `json:"min_input_tokens"`
		MinOutputTokens             int64    `json:"min_output_tokens"`
		MinCostMicrousd             int64    `json:"min_cost_microusd"`
	}{
		SchemaVersion: policy.SchemaVersion, EnvName: policy.EnvName,
		AllowedCredentialProfileIDs: ids, ProviderIdentityFingerprint: policy.ProviderIdentityFingerprint,
		SuiteMaxWallMilliseconds: policy.SuiteMaxWallMilliseconds, MinRequests: policy.MinRequests,
		MinInputTokens: policy.MinInputTokens, MinOutputTokens: policy.MinOutputTokens,
		MinCostMicrousd: policy.MinCostMicrousd,
	}
	return canonicaljson.Fingerprint(credentialPolicyPrefix, view)
}

// AuthorizeLease 在评测子进程启动前做全量租约授权：策略与租约各自自洽、
// 启动时刻处于租约窗口、到期时间覆盖套件最坏截止、账号标识被允许、
// Provider 身份一致且逐项配额不低于策略最小需求。任一不过都意味着不能
// 以更小租约静默改变评测行为，必须整体拒绝并保持候选 ready。
func AuthorizeLease(lease CredentialLease, policy CredentialPolicy, suiteStart time.Time) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("credential policy rejected: %w", err)
	}
	if err := lease.Validate(); err != nil {
		return fmt.Errorf("credential lease rejected: %w", err)
	}
	suiteDeadline := suiteStart.Add(time.Duration(policy.SuiteMaxWallMilliseconds) * time.Millisecond)
	// ADR 合同要求到期"必须晚于"套件截止：恰等边界同样拒绝，避免把评测
	// 结束时刻钉死在租约失效的最后一瞬。
	if !lease.ExpiresAt.After(suiteDeadline) {
		return fmt.Errorf("lease expires at %s, no later than the suite deadline %s", lease.ExpiresAt.UTC().Format(time.RFC3339), suiteDeadline.UTC().Format(time.RFC3339))
	}
	if lease.IssuedAt.After(suiteStart) {
		return errors.New("lease issued after the suite start")
	}
	allowed, err := policy.NormalizedCredentialProfileIDs()
	if err != nil {
		return err
	}
	permitted := false
	for _, id := range allowed {
		if id == lease.CredentialProfileID {
			permitted = true
			break
		}
	}
	if !permitted {
		return fmt.Errorf("lease credential profile %q is not allowed by policy", lease.CredentialProfileID)
	}
	if lease.ProviderIdentityFingerprint != policy.ProviderIdentityFingerprint {
		return errors.New("lease provider identity does not match the frozen policy")
	}
	shortfalls := make([]string, 0, 4)
	if lease.MaxRequests < policy.MinRequests {
		shortfalls = append(shortfalls, "requests")
	}
	if lease.MaxInputTokens < policy.MinInputTokens {
		shortfalls = append(shortfalls, "input tokens")
	}
	if lease.MaxOutputTokens < policy.MinOutputTokens {
		shortfalls = append(shortfalls, "output tokens")
	}
	if lease.MaxCostMicrousd < policy.MinCostMicrousd {
		shortfalls = append(shortfalls, "cost")
	}
	if len(shortfalls) > 0 {
		return fmt.Errorf("lease quota below policy minimum for: %s", strings.Join(shortfalls, ", "))
	}
	return nil
}
