package sandboxeval

import (
	"errors"
	"fmt"
	"sort"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/contract"
)

// evalSetSchemaV1 与 comparatorContractSchemaV1 分别是 EvalSet 与比较器合同
// 的当前 schema 版本。
const (
	evalSetSchemaV1            = 1
	comparatorContractSchemaV1 = 1
)

// EvalSetSchemaVersionV1 与 CredentialPolicySchemaVersionV1 是面向装配方的当
// 前评测集与凭据策略 schema 版本导出常量；构造冻结输入必须经此表达版本事实，
// 不得散落字面量。
const (
	EvalSetSchemaVersionV1          = evalSetSchemaV1
	CredentialPolicySchemaVersionV1 = policySchemaV1
	LeaseSchemaVersionV1            = leaseSchemaV1
)

// evalSetFingerprintPrefix 是 EvalSet 指纹的版本化域前缀。
const evalSetFingerprintPrefix = "cairn-eval-set-v1"

// buildIdentityCleanMarker 是构建身份"干净"的合同取值；工作树不干净时
// 禁止创建 EvalSet。
const buildIdentityCleanMarker = "false"

// PairOrderAlternating 是首发唯一的执行顺序策略：按排序后的 task_id 执行，
// 同一任务内奇数 attempt 为 baseline → variant，偶数 attempt 为
// variant → baseline，同一 pair 必须相邻。
const PairOrderAlternating = "alternating"

// 比较器的万分比整数阈值与改善模式常量；全部取值逐条来自 ADR-0012 并在
// 测试中锁值，禁止在评测后调整。判定执行函数属于后续批次。
const (
	// comparatorCostPassBasisPoints 承载 §5.3 第 7 条成本通过门：
	// variant_cost_units × 10000 <= baseline_cost_units × 13000。
	comparatorCostPassBasisPoints = 13000
	// comparatorCostEfficiencyBasisPoints 承载 §5.4 模式 B 的成本节省帽：
	// variant_cost_units × 10000 <= baseline_cost_units × 8000。
	comparatorCostEfficiencyBasisPoints = 8000
	// comparatorWallEfficiencyBasisPoints 承载 §5.4 模式 B 的时延帽：
	// variant_median_wall_ms × 10000 <= baseline_median_wall_ms × 11000。
	comparatorWallEfficiencyBasisPoints = 11000
	// comparatorSuccessImprovementDelta 承载 §5.4 模式 A 的有效性改善门槛：
	// variant_total_successes >= baseline_total_successes + 2。
	comparatorSuccessImprovementDelta = 2
	// comparatorWallPassBasisPoints 承载 §5.3 第 8 条时延通过门上限倍率。
	comparatorWallPassBasisPoints = 15000
	// basisPointsScale 是万分比门的统一换算基数。
	basisPointsScale = 10000
)

// ComparatorContract 是预注册的判定合同：比较器只依据本合同产出评测结论，
// 不得在看到结果后修改阈值。全部门槛以万分比整数显式进入本结构与指纹，
// 使阈值的任何漂移都会改变 eval_set_fingerprint 并被拒绝继续执行；合法
// 取值唯一对应 PreregisteredComparatorContract 返回的注册组合。
type ComparatorContract struct {
	SchemaVersion     int    `json:"schema_version"`
	ComparatorVersion string `json:"comparator_version"`
	// CostPassBasisPoints 承载 §5.3 第 7 条的成本通过门上限倍率。
	CostPassBasisPoints int `json:"cost_pass_basis_points"`
	// WallPassBasisPoints 承载 §5.3 第 8 条的时延通过门上限倍率。
	WallPassBasisPoints int `json:"wall_pass_basis_points"`
	// CostEfficiencyBasisPoints 承载 §5.4 模式 B 的成本节省帽。
	CostEfficiencyBasisPoints int `json:"cost_efficiency_basis_points"`
	// WallEfficiencyBasisPoints 承载 §5.4 模式 B 的时延帽。
	WallEfficiencyBasisPoints int `json:"wall_efficiency_basis_points"`
	// SuccessImprovementDelta 承载 §5.4 模式 A 的有效性改善门槛。
	SuccessImprovementDelta int `json:"success_improvement_delta"`
}

// PreregisteredComparatorContract 返回当前唯一合法的注册判定合同实例：
// 版本标识与全部门槛绑定，EvalSet 冻结必须经由此入口而不是手工拼装。
func PreregisteredComparatorContract(version string) ComparatorContract {
	return ComparatorContract{
		SchemaVersion:             comparatorContractSchemaV1,
		ComparatorVersion:         version,
		CostPassBasisPoints:       comparatorCostPassBasisPoints,
		WallPassBasisPoints:       comparatorWallPassBasisPoints,
		CostEfficiencyBasisPoints: comparatorCostEfficiencyBasisPoints,
		WallEfficiencyBasisPoints: comparatorWallEfficiencyBasisPoints,
		SuccessImprovementDelta:   comparatorSuccessImprovementDelta,
	}
}

// Validate 校验比较器合同与注册组合逐位一致：阈值偏离注册值（哪怕一个基
// 点）都使合同非法，让"评测后调阈值"在任何阶段都不可能通过校验。
func (comparator ComparatorContract) Validate() error {
	if comparator.SchemaVersion != comparatorContractSchemaV1 {
		return fmt.Errorf("comparator contract schema_version %d is not supported", comparator.SchemaVersion)
	}
	if comparator.ComparatorVersion == "" {
		return errors.New("comparator version must not be empty")
	}
	if comparator != PreregisteredComparatorContract(comparator.ComparatorVersion) {
		return errors.New("comparator contract deviates from the preregistered thresholds")
	}
	return nil
}

// TaskContract 是单个任务的冻结执行合同：基线提交、verifier 摘要、任务
// 镜像摘要与 bootstrap 合同摘要。只存摘要不存正文或路径。
type TaskContract struct {
	TaskID                  string `json:"task_id"`
	BaselineCommit          string `json:"baseline_commit"`
	VerifierDigest          string `json:"verifier_digest"`
	DockerImageDigest       string `json:"docker_image_digest"`
	BootstrapContractDigest string `json:"bootstrap_contract_digest"`
}

// ExecutionSchedule 冻结执行调度：首轮固定单并发与成对交错顺序，防止共享
// 配额和提供方限流相互污染。
type ExecutionSchedule struct {
	ScheduleVersion string `json:"schedule_version"`
	Concurrency     int    `json:"concurrency"`
	PairOrder       string `json:"pair_order"`
}

// EvalBuildIdentity 是参与指纹的 Cairn 构建身份：dirty 必须为 false，
// revision 只作解释信息，完整身份由 fingerprint 钉住。
type EvalBuildIdentity struct {
	Revision    string `json:"revision"`
	Dirty       string `json:"dirty"`
	Flavor      string `json:"flavor"`
	Fingerprint string `json:"fingerprint"`
}

// EvalSet 是一次候选评测的完整冻结输入：生成指纹后任何字段不得修改，
// 变更任一字段必须创建新 EvalSet。路径、任务正文、verifier 正文、argv、
// 环境值和密钥不得进入本结构。
type EvalSet struct {
	SchemaVersion               int                             `json:"schema_version"`
	TaskPackFingerprint         string                          `json:"task_pack_fingerprint"`
	TaskIDs                     []string                        `json:"task_ids"`
	TaskContracts               []TaskContract                  `json:"task_contracts"`
	AttemptsPerTask             int                             `json:"attempts_per_task"`
	ExecutionSchedule           ExecutionSchedule               `json:"execution_schedule"`
	BaselineConfigFingerprint   string                          `json:"baseline_config_fingerprint"`
	VariantConfigFingerprint    string                          `json:"variant_config_fingerprint"`
	ResolvedExecutionPlanDigest string                          `json:"resolved_execution_plan_digest"`
	BuildIdentity               EvalBuildIdentity               `json:"cairn_build_identity"`
	ToolRegistryFingerprint     string                          `json:"tool_registry_fingerprint"`
	ProviderRequestProfile      contract.ProviderRequestProfile `json:"provider_request_profile"`
	PricingSnapshot             contract.PricingSnapshot        `json:"pricing_snapshot"`
	CredentialPolicyFingerprint string                          `json:"credential_policy_fingerprint"`
	ComparatorContract          ComparatorContract              `json:"comparator_contract"`
}

// Validate 失败关闭地复核 EvalSet 的全部结构约束：字段非空与摘要形态、
// 任务清单升序去重并与合同一一对应、首轮单并发与交替 pair 顺序、干净构建
// 身份以及嵌套合同的递归自检。
func (evalSet EvalSet) Validate() error {
	if evalSet.SchemaVersion != evalSetSchemaV1 {
		return fmt.Errorf("eval set schema_version %d is not supported", evalSet.SchemaVersion)
	}
	if !fingerprintShape(evalSet.TaskPackFingerprint) || !fingerprintShape(evalSet.ResolvedExecutionPlanDigest) || !fingerprintShape(evalSet.ToolRegistryFingerprint) || !fingerprintShape(evalSet.CredentialPolicyFingerprint) {
		return errors.New("eval set fingerprints and digests must be sha256 digests")
	}
	if !fingerprintShape(evalSet.BaselineConfigFingerprint) || !fingerprintShape(evalSet.VariantConfigFingerprint) {
		return errors.New("config fingerprints must be sha256 digests")
	}
	if len(evalSet.TaskIDs) == 0 {
		return errors.New("eval set must freeze at least one task id")
	}
	if !sort.StringsAreSorted(evalSet.TaskIDs) {
		return errors.New("task ids must be frozen in ascending order")
	}
	seen := make(map[string]struct{}, len(evalSet.TaskIDs))
	for _, id := range evalSet.TaskIDs {
		if id == "" {
			return errors.New("task ids cannot be empty")
		}
		if _, duplicated := seen[id]; duplicated {
			return fmt.Errorf("task id %q is duplicated", id)
		}
		seen[id] = struct{}{}
	}
	if len(evalSet.TaskContracts) != len(evalSet.TaskIDs) {
		return fmt.Errorf("task contracts (%d) must map one-to-one onto task ids (%d)", len(evalSet.TaskContracts), len(evalSet.TaskIDs))
	}
	for index, taskContract := range evalSet.TaskContracts {
		if taskContract.TaskID != evalSet.TaskIDs[index] {
			return fmt.Errorf("task contract %d maps to %q but %q was frozen at this position", index, taskContract.TaskID, evalSet.TaskIDs[index])
		}
		if taskContract.BaselineCommit == "" {
			return fmt.Errorf("task %q requires a baseline commit", taskContract.TaskID)
		}
		if !fingerprintShape(taskContract.VerifierDigest) || !fingerprintShape(taskContract.DockerImageDigest) || !fingerprintShape(taskContract.BootstrapContractDigest) {
			return fmt.Errorf("task %q contracts require sha256 digests", taskContract.TaskID)
		}
	}
	if evalSet.AttemptsPerTask <= 0 {
		return errors.New("attempts_per_task must be positive")
	}
	if err := evalSet.ExecutionSchedule.Validate(); err != nil {
		return err
	}
	if evalSet.BuildIdentity.Dirty != buildIdentityCleanMarker {
		return fmt.Errorf("eval set requires a clean build identity, got dirty=%q", evalSet.BuildIdentity.Dirty)
	}
	if evalSet.BuildIdentity.Revision == "" || evalSet.BuildIdentity.Flavor == "" || !fingerprintShape(evalSet.BuildIdentity.Fingerprint) {
		return errors.New("build identity requires revision, flavor and a sha256 fingerprint")
	}
	if err := evalSet.ProviderRequestProfile.Validate(); err != nil {
		return fmt.Errorf("provider request profile: %w", err)
	}
	if err := evalSet.PricingSnapshot.Validate(); err != nil {
		return fmt.Errorf("pricing snapshot: %w", err)
	}
	if err := evalSet.ComparatorContract.Validate(); err != nil {
		return fmt.Errorf("comparator contract: %w", err)
	}
	return nil
}

// Validate 校验执行调度的闭合取值：版本非空、首轮并发恒为一、pair 顺序为
// 固定的交替策略。
func (schedule ExecutionSchedule) Validate() error {
	if schedule.ScheduleVersion == "" {
		return errors.New("execution schedule requires a version")
	}
	if schedule.Concurrency != 1 {
		return fmt.Errorf("first eval set generation requires concurrency=1, got %d", schedule.Concurrency)
	}
	if schedule.PairOrder != PairOrderAlternating {
		return fmt.Errorf("unsupported pair order %q (supported: %s)", schedule.PairOrder, PairOrderAlternating)
	}
	return nil
}

// Fingerprint 以版本化前缀计算 EvalSet 指纹：前置校验失败即拒绝产出，
// 保证任何可计算指纹的结构都先满足全部合同约束。
func (evalSet EvalSet) Fingerprint() (string, error) {
	if err := evalSet.Validate(); err != nil {
		return "", fmt.Errorf("cannot fingerprint invalid eval set: %w", err)
	}
	return canonicaljson.Fingerprint(evalSetFingerprintPrefix, evalSet)
}

// LoadEvalSet 从本地忽略目录中的冻结评测集文件装载 EvalSet：解码走包内唯一
// 的严格实现（单文档、禁未知字段、拒绝尾随内容），解析后立即复核全部合同
// 约束。执行链只接受经该入口进入的冻结输入，半解析状态不得驱动评测决策。
func LoadEvalSet(path string) (EvalSet, error) {
	var evalSet EvalSet
	if err := decodeFrozenJSONFile(path, &evalSet); err != nil {
		return EvalSet{}, fmt.Errorf("eval set: %w", err)
	}
	if err := evalSet.Validate(); err != nil {
		return EvalSet{}, fmt.Errorf("eval set %q rejected: %w", path, err)
	}
	return evalSet, nil
}
