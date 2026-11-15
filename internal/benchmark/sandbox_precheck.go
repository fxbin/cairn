// [INPUT]: 依赖Memory评测合同、冻结资产与受控执行入口。
// [OUTPUT]: 提供启动前核对身份、租约、凭据和冻结任务。
// [POS]: 带外评测工具链，不进入最小Agent内核。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package benchmark 提供隔离评测与冻结证据。
// Author: fxbin
package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"

	"github.com/fxbin/cairn/internal/capabilities/memory"
	"github.com/fxbin/cairn/internal/runtime/sandboxeval"
)

// precheck 结构化承载启动前检查的派生产物：展开后两臂配置、变体展开指纹与
// 确定性评测身份。任何一项失败都保持候选 ready 且零持久化。
type precheckResult struct {
	baselineConfig              config.Config
	variantConfig               config.Config
	variantFingerprint          string
	variantSpecDigest           string
	candidateVariantFingerprint string
	evaluationID                string
	evalSetFingerprint          string
	baselineFingerprint         string
}

// prescreenFailure 把离线预筛失败从普通错误中区分出来：它代表冻结合同破坏，
// 走 invalid 结论轨而不是保持 ready 的拒绝轨。
type prescreenFailure struct {
	issues []string
}

// Error 汇总预筛失败面；条目本身已是封闭的比对结论文本。
func (failure *prescreenFailure) Error() string {
	return "offline prescreen failed: " + strings.Join(failure.issues, "; ")
}

// evaluationDirectory 返回评测状态目录：所有沙箱评测本地产物都以它为唯一归
// 档根，Memory 侧只保留其内套件结果的字节摘要引用。
func evaluationDirectory(stateRoot, evaluationID string) string {
	return filepath.Join(stateRoot, "evaluations", safePathComponent(evaluationID))
}

// evaluationLockPath 返回 Evaluation ID 级进程锁路径；锁锚定在候选的权威
// Memory 状态根而非可替换的套件输出根，进程退出后可由后继恢复者安全重获。
func evaluationLockPath(memoryStateDir, evaluationID string) string {
	if strings.TrimSpace(memoryStateDir) == "" {
		memoryStateDir = app.DefaultStateDir()
	}
	return filepath.Join(memoryStateDir, "evaluation-locks", safePathComponent(evaluationID)+".lock")
}

// suiteSegmentForArm 生成按臂分离的套件段标识：Workspace、状态目录与任务身
// 份投影都以它区分基线臂与变体臂，两臂永不共享可写边界。
func suiteSegmentForArm(evaluationID string, arm sandboxeval.SuiteArm) string {
	return "sandboxeval-" + sandboxArmTag(arm) + "-" + safePathComponent(evaluationID)
}

// BootstrapContractDigest 以规范化 JSON 计算 bootstrap 合同摘要：命令序列经
// 排序键稳定编码后取 sha256。冻结评测集制作者与本执行器共用同一实现，双侧
// 摘要不一致即视为冻结合同破坏。
func BootstrapContractDigest(commands []Command) (string, error) {
	view := struct {
		Commands []Command `json:"commands"`
	}{Commands: append([]Command(nil), commands...)}
	encoded, err := json.Marshal(view)
	if err != nil {
		return "", fmt.Errorf("encode bootstrap contract: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// runPrecheck 执行启动事务前的全量失败关闭检查。拒绝轨错误（结构与身份残缺、
// 租约不足等）以 error 返回，候选保持 ready 零持久化；离线预筛的冻结指纹破
// 坏不属拒绝轨，以第二返回值携带失败面——按 ADR 合同它将在开评测行后立即以
// invalid 闭合，绝不调用模型。dry-run 与正式运行共用同一检查面，保证两个入
// 口等价。
func (executor SandboxExecutor) runPrecheck(options SandboxExecutorOptions) (precheckResult, []string, error) {
	precheck := precheckResult{}
	if options.CandidateID == "" || options.StateRoot == "" || options.WorkspacesRoot == "" || options.PrivateRoot == "" {
		return precheck, nil, errors.New("sandbox evaluation requires candidate id and state, workspaces, private roots")
	}
	if err := validatePrepareProxy(options.PrepareProxy); err != nil {
		return precheck, nil, fmt.Errorf("prepare proxy: %w", err)
	}
	if err := options.Pack.Validate(); err != nil {
		return precheck, nil, fmt.Errorf("task pack invalid: %w", err)
	}
	if packFingerprint := options.Pack.Fingerprint(); packFingerprint != options.EvalSet.TaskPackFingerprint {
		return precheck, nil, fmt.Errorf("task pack fingerprint %q diverges from frozen %q", packFingerprint, options.EvalSet.TaskPackFingerprint)
	}
	if _, err := options.Pack.SelectedTasks(options.EvalSet.TaskIDs); err != nil {
		return precheck, nil, fmt.Errorf("frozen task selection: %w", err)
	}
	if err := validateSandboxTaskStructures(options); err != nil {
		return precheck, nil, err
	}
	if strings.TrimSpace(options.BaselineConfig.Provider.APIKeyEnv) != sandboxeval.SandboxProviderKeyEnv {
		return precheck, nil, fmt.Errorf("frozen config must name %s as its provider api_key_env", sandboxeval.SandboxProviderKeyEnv)
	}
	if strings.TrimSpace(os.Getenv(sandboxeval.SandboxProviderKeyEnv)) == "" {
		return precheck, nil, fmt.Errorf("host environment must inject %s for this evaluation", sandboxeval.SandboxProviderKeyEnv)
	}
	if !options.DryRun {
		if os.Getenv(sandboxeval.EvaluationChildMarkerEnv) != "1" {
			return precheck, nil, errors.New("formal sandbox evaluation must run inside the trusted empty-environment child")
		}
		environment := os.Environ()
		if executor.AuditEnvironment != nil {
			environment = executor.AuditEnvironment()
		}
		if violations := sandboxeval.AuditEvaluationEnvironment(environment); len(violations) > 0 {
			return precheck, nil, fmt.Errorf("trusted evaluation environment contains forbidden variables: %s", strings.Join(violations, ","))
		}
	}
	if err := config.Validate(options.BaselineConfig); err != nil {
		return precheck, nil, fmt.Errorf("baseline config invalid: %w", err)
	}
	providerProfile, pricingSnapshot, err := providerContractFromConfig(options.BaselineConfig)
	if err != nil {
		return precheck, nil, err
	}
	if !reflect.DeepEqual(providerProfile, options.EvalSet.ProviderRequestProfile) {
		return precheck, nil, errors.New("provider request profile diverges from the frozen baseline configuration")
	}
	if pricingSnapshot != options.EvalSet.PricingSnapshot {
		return precheck, nil, errors.New("pricing snapshot diverges from the frozen baseline configuration")
	}
	variantConfig, err := ExpandCompactionVariant(options.BaselineConfig, options.Variant)
	if err != nil {
		return precheck, nil, err
	}
	if err := sandboxeval.TaskEnvironmentViolation(sandboxEnvironmentNames(options.BaselineConfig)); err != nil {
		return precheck, nil, err
	}
	if err := sandboxeval.TaskEnvironmentViolation(sandboxEnvironmentNames(variantConfig)); err != nil {
		return precheck, nil, err
	}
	if err := options.EvalSet.Validate(); err != nil {
		return precheck, nil, fmt.Errorf("frozen eval set rejected: %w", err)
	}
	if err := options.Policy.Validate(); err != nil {
		return precheck, nil, fmt.Errorf("credential policy rejected: %w", err)
	}
	policyFingerprint, err := options.Policy.Fingerprint()
	if err != nil {
		return precheck, nil, err
	}
	if policyFingerprint != options.EvalSet.CredentialPolicyFingerprint {
		return precheck, nil, fmt.Errorf("credential policy fingerprint %q diverges from frozen %q", policyFingerprint, options.EvalSet.CredentialPolicyFingerprint)
	}
	suiteStart := executor.now()
	if err := sandboxeval.AuthorizeLease(options.Lease, options.Policy, suiteStart); err != nil {
		return precheck, nil, fmt.Errorf("credential lease authorization failed before any persistence: %w", err)
	}
	evalSetFingerprint, err := options.EvalSet.Fingerprint()
	if err != nil {
		return precheck, nil, err
	}
	baselineFingerprint := options.BaselineConfig.Fingerprint()
	variantFingerprint, err := VariantExpandedFingerprint(baselineFingerprint, options.Variant)
	if err != nil {
		return precheck, nil, err
	}
	variantSpecDigest, err := memory.VariantSpecDigest(options.Variant)
	if err != nil {
		return precheck, nil, err
	}
	candidateVariantFingerprint, err := memory.VariantFingerprint(memory.TargetKindContextCompactionParams, options.Variant)
	if err != nil {
		return precheck, nil, err
	}
	evaluationID, err := memory.EvaluationID(options.CandidateID, evalSetFingerprint,
		baselineFingerprint, variantFingerprint, options.EvalSet.ComparatorContract.ComparatorVersion)
	if err != nil {
		return precheck, nil, err
	}
	precheck = precheckResult{
		baselineConfig: options.BaselineConfig, variantConfig: variantConfig,
		variantFingerprint: variantFingerprint, variantSpecDigest: variantSpecDigest,
		candidateVariantFingerprint: candidateVariantFingerprint, evaluationID: evaluationID,
		evalSetFingerprint: evalSetFingerprint, baselineFingerprint: baselineFingerprint,
	}
	report, err := RunOfflinePrescreen(options.EvalSet, options.BaselineConfig, variantConfig, options.Variant)
	if err != nil {
		return precheck, nil, err
	}
	if !report.Passed() {
		return precheck, report.Issues, nil
	}
	return precheck, nil, nil
}

// validateSandboxTaskStructures 对照冻结任务合同做纯内存的结构性核验：每个
// 冻结任务的基线提交与 bootstrap 合同摘要必须和本地任务合同逐字一致。镜像
// 与 verifier 摘要依赖工作区与环境，只能在运行期逐步复核并归入 invalid 结
// 论轨。
func validateSandboxTaskStructures(options SandboxExecutorOptions) error {
	tasks, err := options.Pack.SelectedTasks(options.EvalSet.TaskIDs)
	if err != nil {
		return fmt.Errorf("frozen task selection: %w", err)
	}
	selected := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		selected[task.ID] = task
		for _, command := range task.Verification.Bootstrap {
			if len(command.Argv) == 0 {
				return fmt.Errorf("task %q carries an empty bootstrap command", task.ID)
			}
		}
	}
	for _, frozen := range options.EvalSet.TaskContracts {
		task, found := selected[frozen.TaskID]
		if !found {
			return fmt.Errorf("frozen task %q is absent from the local task pack", frozen.TaskID)
		}
		if task.Repository.BaselineCommit != frozen.BaselineCommit {
			return fmt.Errorf("frozen baseline commit for task %q diverges from the local task contract", frozen.TaskID)
		}
		digest, err := BootstrapContractDigest(task.Verification.Bootstrap)
		if err != nil {
			return err
		}
		if digest != frozen.BootstrapContractDigest {
			return fmt.Errorf("frozen bootstrap contract digest for task %q diverges from the local task contract", frozen.TaskID)
		}
	}
	return nil
}

// sandboxEnvironmentNames 收集冻结配置中全部会注入任务子进程的环境变量名单：
// 验证命令显式 env 与策略允许名单都属任务可见面，沙箱 Key 名混入任何一处都
// 是隔离破坏。
func sandboxEnvironmentNames(configuration config.Config) []string {
	names := make([]string, 0, len(configuration.Policy.EnvAllow))
	names = append(names, configuration.Policy.EnvAllow...)
	for _, command := range configuration.Verification.Commands {
		for key := range command.Env {
			names = append(names, key)
		}
	}
	return names
}

// prepareEvaluationDirectory 为新评测创建空目录，或为 running 评测接受受控
// 的恢复表面；ready 候选遇到既有残件仍失败关闭，防止孤立文件被冒充历史。
func prepareEvaluationDirectory(directory string, resuming bool) error {
	entries, readErr := os.ReadDir(directory)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return fmt.Errorf("read evaluation directory: %w", readErr)
	}
	if !resuming && readErr == nil && len(entries) > 0 {
		return fmt.Errorf("evaluation directory %q already holds artifacts; interrupted evaluations must be resolved explicitly instead of rerun", filepath.Base(directory))
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create evaluation directory: %w", err)
	}
	return nil
}
