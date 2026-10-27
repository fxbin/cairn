package ports

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/domain"
)

// 本文件承载跨层共享的运行期控制流错误与审批决定合同。它们被 agent 执行
// 循环产生、被 contract 失败分类消费，并由 CLI/TUI 展示；放在此处可避免
// 合同投影与界面反向依赖执行层。错误文本不得包含密钥、原始模型输出或本地路径。

// ErrTurnBudgetExceeded 表示运行耗尽了允许的模型轮次。
var ErrTurnBudgetExceeded = errors.New("turn budget exhausted")

// ErrWaitingReconcile 表示已启动的副作用操作没有可确认的终态，必须人工对账。
var ErrWaitingReconcile = errors.New("run has a dangling side-effect operation that requires reconciliation")

// ErrJournalConflict 表示追加时观察到的事件序号与调用方预期不一致；家族
// 外部追加（子完成回写、跨 run 消息）与运行中 Loop 并发时会出现，是可由
// 重载消除的确定性冲突，不是结果不确定的失败。
var ErrJournalConflict = errors.New("journal append sequence conflict")

// Approval 是 resume 提供的显式、一次性审批决定。
type Approval struct {
	Fingerprint string
	Approve     bool
	Reason      string
}

// ProviderCapabilityMismatchError 表示 Provider 的实际响应违背已冻结的能力
// 卡，例如声明流式 usage 却未返回 usage，或 finish_reason 不在声明集合中。它
// 是不可重试的配置/协议不一致，必须作为稳定错误分类写入事件。
type ProviderCapabilityMismatchError struct {
	Requirement string
	Observed    string
}

// Error 返回不含原始模型内容的稳定不匹配描述，避免把 Provider 响应写入日志。
func (err *ProviderCapabilityMismatchError) Error() string {
	return fmt.Sprintf("provider capability mismatch: %s (%s)", err.Requirement, err.Observed)
}

// ErrorClass 返回持久化与产品合同共用的失败分类。
func (err *ProviderCapabilityMismatchError) ErrorClass() string {
	return "provider_capability_mismatch"
}

// IsRetryable 报告能力合同不一致不能由盲目重试修复。
func (err *ProviderCapabilityMismatchError) IsRetryable() bool { return false }

// BudgetExceededError 标识导致运行停止的持久化预算维度；为兼容调用方，
// 它会解包为 ErrTurnBudgetExceeded。
type BudgetExceededError struct{ Dimension string }

// Error 返回耗尽预算维度的稳定错误文本。
func (err *BudgetExceededError) Error() string {
	return fmt.Sprintf("budget exhausted: %s", err.Dimension)
}

// Unwrap 使预算错误可被 errors.Is 识别为轮次预算耗尽。
func (err *BudgetExceededError) Unwrap() error { return ErrTurnBudgetExceeded }

// WaitingApprovalError 表示运行在尚未启动副作用前等待人工决定。
type WaitingApprovalError struct {
	RunID       domain.RunID
	OperationID domain.OperationID
	Fingerprint string
	Summary     string
}

// Error 返回等待审批操作及其绑定指纹。
func (err *WaitingApprovalError) Error() string {
	return fmt.Sprintf("approval required for %s (%s)", err.OperationID, err.Fingerprint)
}

// WaitingReconcileError 标识一次已持久化 operation_started 的副作用无法确认终态；
// RunID 与 OperationID 供 CLI 和调用方生成精确的恢复指引。
type WaitingReconcileError struct {
	RunID       domain.RunID
	OperationID domain.OperationID
	Reason      string
}

// Error 返回需要人工对账的操作及其非敏感原因。
func (err *WaitingReconcileError) Error() string {
	return fmt.Sprintf("reconciliation required for %s: %s", err.OperationID, err.Reason)
}

// Unwrap 使等待对账错误可映射到公开稳定的 reconciliation required 退出码。
func (err *WaitingReconcileError) Unwrap() error { return ErrWaitingReconcile }

// PolicyDeniedError 表示策略明确拒绝了模型提议的操作。
type PolicyDeniedError struct{ Summary string }

// Error 返回被拒绝操作的策略摘要。
func (err *PolicyDeniedError) Error() string { return "policy denied: " + err.Summary }

// VerificationFailureError 是固定验证命令失败的持久化反馈；它不是终态错误，
// 模型仍可在剩余轮次中修复工作区。
type VerificationFailureError struct {
	CommandID string
	Feedback  string
}

// Error 返回可反馈给模型的验证失败信息。
func (err *VerificationFailureError) Error() string { return err.Feedback }

// LoopGuardExceededError 表示运行因确定性的重复失败、周期或无进展规则而提前终止。
type LoopGuardExceededError struct{ Reason string }

// Error 返回可用于事件和基准分类的稳定熔断原因。
func (err *LoopGuardExceededError) Error() string {
	return fmt.Sprintf("loop guard triggered: %s", err.Reason)
}
