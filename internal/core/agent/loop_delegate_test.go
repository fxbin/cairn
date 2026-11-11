package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/fxbin/cairn/internal/ports"
)

// TestClassifyBatchSpecialError 锁定批次错误的特殊分类：结果落盘失败与对账
// 暂停必须原样上抛，不得被批次分支误终态化或降级为普通失败。
func TestClassifyBatchSpecialError(t *testing.T) {
	persistence := &toolResultPersistenceError{cause: fmt.Errorf("journal write failed")}
	if special := classifyBatchSpecialError(persistence); !errors.Is(special, persistence) {
		t.Fatalf("persistence error must pass through: %v", special)
	}
	reconcile := &ports.WaitingReconcileError{RunID: "run", OperationID: "op", Reason: "unconfirmed"}
	if special := classifyBatchSpecialError(reconcile); !errors.Is(special, ports.ErrWaitingReconcile) {
		t.Fatalf("reconcile error must pass through: %v", special)
	}
	if special := classifyBatchSpecialError(fmt.Errorf("plain failure")); special != nil {
		t.Fatalf("plain error must not be classified special: %v", special)
	}
}
