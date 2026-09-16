// 本文件承载评测冻结合同的装配编排：从本地忽略资产推导十五字段评测集与凭
// 据策略文件，装配面与执行器逐一同源（任务包、变体展开、bootstrap 合同、镜
// 像自省、verifier 文件摘要），并在落盘前以执行器的启动前检查做一次完整自
// 检，指纹链断裂的产物不得离开本函数。
//
// Author: fxbin
package benchmark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// FreezeEvalSetOptions 描述一次冻结装配的全部输入：本地任务包、冻结基线配
// 置、人工重述的变体参数与凭据策略参数；路径只进私有 verifier 目录与输出目
// 录等必要边界。
type FreezeEvalSetOptions struct {
	Pack                     TaskPack
	TaskIDs                  []string
	AttemptsPerTask          int
	BaselineConfig           config.Config
	Variant                  memory.CompactionParamsVariant
	ComparatorVersion        string
	PolicyProfileID          string
	SuiteMaxWallMilliseconds int64
	MinRequests              int64
	MinInputTokens           int64
	MinOutputTokens          int64
	MinCostMicrousd          int64
	PrivateRoot              string
	OutDir                   string
}

// FreezeEvalSetReport 是冻结装配的结果：两份产物的落盘位置与由其派生的两个
// 指纹。指纹即登记与运行阶段的唯一身份引用。
type FreezeEvalSetReport struct {
	EvalSetPath                 string `json:"eval_set_path"`
	CredentialPolicyPath        string `json:"credential_policy_path"`
	EvalSetFingerprint          string `json:"eval_set_fingerprint"`
	CredentialPolicyFingerprint string `json:"credential_policy_fingerprint"`
}

// FreezeEvaluationAssets 装配并自检冻结产物：任何一步失败都不写出文件。自检
// 复用执行器的启动前检查全套（含预筛与两臂展开指纹比对），宿主环境必须注
// 入占位的沙箱 Key 变量以满足存在性检查；真实运行仍需真实租约文件。
func FreezeEvaluationAssets(ctx context.Context, options FreezeEvalSetOptions) (FreezeEvalSetReport, error) {
	report := FreezeEvalSetReport{}
	if options.PolicyProfileID == "" || options.ComparatorVersion == "" || options.OutDir == "" || options.PrivateRoot == "" {
		return report, errors.New("freeze requires policy profile id, comparator version, private root and output directory")
	}
	if err := options.Pack.Validate(); err != nil {
		return report, fmt.Errorf("task pack invalid: %w", err)
	}
	if len(options.TaskIDs) != sandboxeval.FirstEvalTaskCount || options.AttemptsPerTask != sandboxeval.FirstEvalAttemptsPerTask {
		return report, fmt.Errorf("first evaluation freeze requires exactly %d tasks and %d attempts per task", sandboxeval.FirstEvalTaskCount, sandboxeval.FirstEvalAttemptsPerTask)
	}
	tasks, err := options.Pack.SelectedTasks(options.TaskIDs)
	if err != nil {
		return report, fmt.Errorf("task selection: %w", err)
	}
	taskIDs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		taskIDs = append(taskIDs, task.ID)
	}
	// 冻结身份要求任务清单按字节序升序：任务包声明序不保证这一点，排序同
	// 时决定任务合同的装配位置。
	sort.Strings(taskIDs)
	packFingerprint := options.Pack.Fingerprint()
	baselineConfig := options.BaselineConfig
	if err := config.Validate(baselineConfig); err != nil {
		return report, fmt.Errorf("baseline config invalid: %w", err)
	}
	baselineFingerprint := baselineConfig.Fingerprint()
	variantFingerprint, err := VariantExpandedFingerprint(baselineFingerprint, options.Variant)
	if err != nil {
		return report, err
	}
	planDigest, err := ResolvedExecutionPlanDigest(baselineConfig.Resolution)
	if err != nil {
		return report, fmt.Errorf("resolved execution plan digest: %w", err)
	}
	registryFingerprint, err := app.FrozenToolRegistryFingerprint(config.PresetBenchmark)
	if err != nil {
		return report, fmt.Errorf("tool registry fingerprint: %w", err)
	}
	buildIdentity, err := frozenBuildIdentity()
	if err != nil {
		return report, err
	}
	privateProfile, pricing, err := providerContractFromConfig(baselineConfig)
	if err != nil {
		return report, err
	}
	taskByFrozenID := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		taskByFrozenID[task.ID] = task
	}
	taskContracts := make([]sandboxeval.TaskContract, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		task := taskByFrozenID[taskID]
		digest, err := BootstrapContractDigest(task.Verification.Bootstrap)
		if err != nil {
			return report, err
		}
		imageDigest, err := dockerImageSelfDigest(ctx, task.Runtime.DockerImage)
		if err != nil {
			return report, fmt.Errorf("task %q image digest: %w", taskID, err)
		}
		verifierDigest, err := privateVerifierDigest(options.PrivateRoot, taskID)
		if err != nil {
			return report, fmt.Errorf("task %q verifier digest: %w", taskID, err)
		}
		taskContracts = append(taskContracts, sandboxeval.TaskContract{
			TaskID: taskID, BaselineCommit: task.Repository.BaselineCommit,
			VerifierDigest: verifierDigest, DockerImageDigest: imageDigest,
			BootstrapContractDigest: digest,
		})
	}
	policy := sandboxeval.CredentialPolicy{
		SchemaVersion:               sandboxeval.CredentialPolicySchemaVersionV1,
		EnvName:                     sandboxeval.SandboxProviderKeyEnv,
		AllowedCredentialProfileIDs: []string{options.PolicyProfileID},
		ProviderIdentityFingerprint: privateProfile.BaseURLIdentityDigest,
		SuiteMaxWallMilliseconds:    options.SuiteMaxWallMilliseconds,
		MinRequests:                 options.MinRequests, MinInputTokens: options.MinInputTokens,
		MinOutputTokens: options.MinOutputTokens, MinCostMicrousd: options.MinCostMicrousd,
	}
	policyFingerprint, err := policy.Fingerprint()
	if err != nil {
		return report, fmt.Errorf("credential policy fingerprint: %w", err)
	}
	evalSet := sandboxeval.EvalSet{
		SchemaVersion:       sandboxeval.EvalSetSchemaVersionV1,
		TaskPackFingerprint: packFingerprint,
		TaskIDs:             taskIDs,
		TaskContracts:       taskContracts,
		AttemptsPerTask:     options.AttemptsPerTask,
		ExecutionSchedule: sandboxeval.ExecutionSchedule{
			ScheduleVersion: SandboxScheduleVersion, Concurrency: 1, PairOrder: sandboxeval.PairOrderAlternating,
		},
		BaselineConfigFingerprint:   baselineFingerprint,
		VariantConfigFingerprint:    variantFingerprint,
		ResolvedExecutionPlanDigest: planDigest,
		BuildIdentity:               buildIdentity,
		ToolRegistryFingerprint:     registryFingerprint,
		ProviderRequestProfile:      privateProfile,
		PricingSnapshot:             pricing,
		CredentialPolicyFingerprint: policyFingerprint,
		ComparatorContract:          sandboxeval.PreregisteredComparatorContract(options.ComparatorVersion),
	}
	evalSetFingerprint, err := evalSet.Fingerprint()
	if err != nil {
		return report, fmt.Errorf("eval set fingerprint: %w", err)
	}
	if err := selfCheckFrozenAssets(options, evalSet, policy); err != nil {
		return report, fmt.Errorf("frozen asset self-check failed; nothing was written: %w", err)
	}
	if err := os.MkdirAll(options.OutDir, 0o700); err != nil {
		return report, fmt.Errorf("create freeze output directory: %w", err)
	}
	// 自检失败零写入；写盘为顺序原子替换，第二份失败时回收第一份以维持双产
	// 物成对出现，不留半套指纹产物。
	report.EvalSetPath = filepath.Join(options.OutDir, "eval-set.json")
	report.CredentialPolicyPath = filepath.Join(options.OutDir, "credential-policy.json")
	if err := writeJSON(report.EvalSetPath, evalSet); err != nil {
		return report, err
	}
	if err := writeJSON(report.CredentialPolicyPath, policy); err != nil {
		removeErr := os.Remove(report.EvalSetPath)
		if removeErr != nil {
			return report, fmt.Errorf("write credential policy (%v) and pair-reclaim failed (%v)", err, removeErr)
		}
		return report, err
	}
	report.EvalSetFingerprint = evalSetFingerprint
	report.CredentialPolicyFingerprint = policyFingerprint
	return report, nil
}

// selfCheckFrozenAssets 在落盘前以执行器的启动前检查做全链自检：租约使用仅
// 存在于进程内存的自洽占位实例，真实运行的授权仍由独立租约文件约束。
func selfCheckFrozenAssets(options FreezeEvalSetOptions, evalSet sandboxeval.EvalSet, policy sandboxeval.CredentialPolicy) error {
	now := time.Now().UTC()
	syntheticLease := sandboxeval.CredentialLease{
		SchemaVersion: sandboxeval.LeaseSchemaVersionV1, LeaseID: "freeze-selfcheck",
		CredentialProfileID:         options.PolicyProfileID,
		ProviderIdentityFingerprint: policy.ProviderIdentityFingerprint,
		EnvName:                     policy.EnvName,
		IssuedAt:                    now.Add(-time.Hour),
		ExpiresAt:                   now.Add(time.Duration(policy.SuiteMaxWallMilliseconds)*time.Millisecond + 24*time.Hour),
		MaxRequests:                 maxInt64(1, policy.MinRequests), MaxInputTokens: maxInt64(1, policy.MinInputTokens),
		MaxOutputTokens: maxInt64(1, policy.MinOutputTokens), MaxCostMicrousd: maxInt64(1, policy.MinCostMicrousd),
		ServerSideQuotaEnabled: true, Revocable: true,
	}
	executorOptions := SandboxExecutorOptions{
		Pack: options.Pack, EvalSet: evalSet, Lease: syntheticLease, Policy: policy,
		CandidateID:    "freeze-selfcheck",
		StateRoot:      filepath.Join(options.OutDir, ".selfcheck-state"),
		WorkspacesRoot: filepath.Join(options.OutDir, ".selfcheck-workspaces"),
		PrivateRoot:    options.PrivateRoot,
		BaselineConfig: options.BaselineConfig, Variant: options.Variant,
		DryRun: true,
	}
	precheck, prescreenIssues, err := (SandboxExecutor{}).runPrecheck(executorOptions)
	failure := &prescreenFailure{}
	if errors.As(err, &failure) || len(prescreenIssues) > 0 {
		issues := prescreenIssues
		if len(issues) == 0 && failure != nil {
			issues = failure.issues
		}
		return &prescreenFailure{issues: issues}
	}
	if err != nil {
		return err
	}
	if precheck.evaluationID == "" {
		return errors.New("self-check could not derive an evaluation identity")
	}
	return nil
}

// frozenBuildIdentityLookup 是构建信息读取的受控边界；生产实现只取可公开的
// VCS 事实，测试以替身提供确定性身份后必须还原。
var frozenBuildIdentityLookup = func() (sandboxeval.EvalBuildIdentity, error) {
	revision, dirty := "", "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				if setting.Value == "true" || setting.Value == "false" {
					dirty = setting.Value
				}
			}
		}
	}
	if dirty != "false" {
		return sandboxeval.EvalBuildIdentity{}, fmt.Errorf("frozen eval set requires a clean build tree, got dirty=%q", dirty)
	}
	identity := domain.BuildIdentity{Revision: revision, Dirty: dirty, Flavor: "plain"}
	return sandboxeval.EvalBuildIdentity{
		Revision: identity.Revision, Dirty: identity.Dirty, Flavor: identity.Flavor,
		Fingerprint: identity.ComputeFingerprint(),
	}, nil
}

// frozenBuildIdentity 转发到受控的构建信息读取边界。
func frozenBuildIdentity() (sandboxeval.EvalBuildIdentity, error) {
	return frozenBuildIdentityLookup()
}

// providerContractFromConfig 由冻结配置声明推导请求合同与定价快照：能力未显
// 式声明即失败关闭，与 Boot 的能力卡解析同一来源。
func providerContractFromConfig(configuration config.Config) (contract.ProviderRequestProfile, contract.PricingSnapshot, error) {
	capability := configuration.Provider.Capability
	if capability == nil {
		return contract.ProviderRequestProfile{}, contract.PricingSnapshot{}, errors.New("frozen config must declare an explicit provider capability")
	}
	profile, err := contract.ResolveProviderRequestProfile(contract.ProviderKindOpenAICompatible, contract.ProviderRequestDeclaration{
		BaseURL: configuration.Provider.BaseURL, Model: configuration.Provider.Model,
		Capability: config.ProviderCapabilityDeclaration(configuration),
		Generation: config.ProviderGenerationDeclaration(configuration),
	})
	if err != nil {
		return contract.ProviderRequestProfile{}, contract.PricingSnapshot{}, fmt.Errorf("provider request profile: %w", err)
	}
	pricing, err := contract.NewPricingSnapshot(configuration.Pricing.InputTokenUSD, configuration.Pricing.OutputTokenUSD)
	if err != nil {
		return contract.ProviderRequestProfile{}, contract.PricingSnapshot{}, fmt.Errorf("pricing snapshot: %w", err)
	}
	return profile, pricing, nil
}

// privateVerifierDigest 计算私有 verifier 补丁文件的字节摘要：与运行期注入
// 使用同一份文件内容口径，冻结值因此必然与运行期取得的摘要逐位一致。
func privateVerifierDigest(privateRoot, taskID string) (string, error) {
	path := filepath.Join(privateRoot, "verifiers", safePathComponent(taskID)+".patch")
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read private verifier patch: %w", err)
	}
	if len(content) == 0 {
		return "", fmt.Errorf("private verifier patch for %q is empty", taskID)
	}
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// maxInt64 返回两者中较大者，供占位租约满足任意非负最小需求。
func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
