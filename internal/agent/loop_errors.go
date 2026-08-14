package agent

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/domain"
)

// ErrTurnBudgetExceeded 表示运行耗尽了允许的模型轮次。
var ErrTurnBudgetExceeded = errors.New("turn budget exhausted")

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
