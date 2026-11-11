// studio_evaluation.go 为 CLI 提供正式评测装载与结论判定的 Application
// 门面；Studio 层不得直接依赖 internal/sandboxeval。
// Author: fxbin
package app

import "github.com/fxbin/cairn/internal/sandboxeval"

// 评测合同类型与常量别名：CLI 仅作装载与结论判定。
type (
	// SandboxConclusion 是一次评测的闭合结论。
	SandboxConclusion = sandboxeval.Conclusion
	// SandboxEvalSet 是冻结评测任务集。
	SandboxEvalSet = sandboxeval.EvalSet
	// SandboxCredentialLease 是沙箱凭据租约。
	SandboxCredentialLease = sandboxeval.CredentialLease
	// SandboxCredentialPolicy 是沙箱凭据策略。
	SandboxCredentialPolicy = sandboxeval.CredentialPolicy
)

// SandboxArmBaseline 与 SandboxArmVariant 是评测两臂标识。
const (
	SandboxArmBaseline = sandboxeval.ArmBaseline
	SandboxArmVariant  = sandboxeval.ArmVariant
)

// SandboxConclusionPassed 是评测通过结论状态。
const SandboxConclusionPassed = sandboxeval.ConclusionPassed

// SandboxLoadEvalSet 装载冻结评测任务集。
func SandboxLoadEvalSet(path string) (sandboxeval.EvalSet, error) {
	return sandboxeval.LoadEvalSet(path)
}

// SandboxLoadCredentialLease 装载沙箱凭据租约。
func SandboxLoadCredentialLease(path string) (sandboxeval.CredentialLease, error) {
	return sandboxeval.LoadCredentialLease(path)
}

// SandboxLoadCredentialPolicy 装载沙箱凭据策略。
func SandboxLoadCredentialPolicy(path string) (sandboxeval.CredentialPolicy, error) {
	return sandboxeval.LoadCredentialPolicy(path)
}
