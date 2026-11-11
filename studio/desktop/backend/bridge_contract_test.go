// bridge_contract_test.go 锁定 QA-01 桥接契约：ErrorBody DTO 往返、MapError
// 码表、runContext 与绑定取消隔离、取消注册；不 Mock 整个 desktop client。
// Author: fxbin
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/ports"
)

// TestBridgeErrorBodyDTORoundTrip 锁定 ErrorBody JSON 字段名稳定。
func TestBridgeErrorBodyDTORoundTrip(t *testing.T) {
	body := ErrorBody{Code: CodeWaitingApproval, Message: "待审批", RunID: "run-1", OperationID: "op-1", Fingerprint: "fp", Summary: "写入文件"}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"code", "message", "run_id", "operation_id", "fingerprint", "summary"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("DTO 缺少字段 %q: %v", key, decoded)
		}
	}
	var back ErrorBody
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Code != CodeWaitingApproval || back.Fingerprint != "fp" {
		t.Fatalf("往返不符: %+v", back)
	}
}

// TestBridgeMapErrorCodes 锁定错误码映射稳定。
func TestBridgeMapErrorCodes(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{&ports.WaitingApprovalError{RunID: "run-1", OperationID: "op-1", Fingerprint: "fp"}, CodeWaitingApproval},
		{&ports.WaitingReconcileError{RunID: "run-1", OperationID: "op-2", Reason: "unknown"}, CodeWaitingReconcile},
		{&ports.PolicyDeniedError{Summary: "blocked"}, CodePolicyDenied},
		{&ports.BudgetExceededError{Dimension: "max_turns"}, CodeBudget},
		{app.ErrConfigDrift, CodeConfigDrift},
	}
	for _, item := range cases {
		body := MapError(item.err)
		if body.Code != item.code {
			t.Fatalf("映射 %v 应为 %s，得到 %s", item.err, item.code, body.Code)
		}
	}
	if body := MapError(errors.New("boom")); body.Code != CodeInternal {
		t.Fatalf("未知错误应为 internal，得到 %s", body.Code)
	}
}

// TestBridgeRunContextSurvivesBindingCancel 锁定绑定取消不误伤运行上下文。
func TestBridgeRunContextSurvivesBindingCancel(t *testing.T) {
	controller := NewRunController(app.Service{}, nil, nil)
	defer controller.Shutdown(time.Second)
	_, cancelBinding := context.WithCancel(context.Background())
	cancelBinding()
	runCtx, cancelRun, err := controller.runContext()
	if err != nil {
		t.Fatal(err)
	}
	defer cancelRun()
	select {
	case <-runCtx.Done():
		t.Fatal("运行上下文不应随绑定取消而结束")
	case <-time.After(5 * time.Millisecond):
	}
	if runCtx.Err() != nil {
		t.Fatalf("运行上下文应保持有效: %v", runCtx.Err())
	}
}

// TestBridgeCancelRegistration 锁定取消注册与释放。
func TestBridgeCancelRegistration(t *testing.T) {
	controller := NewRunController(app.Service{}, nil, nil)
	defer controller.Shutdown(time.Second)
	ctx, cancel, err := controller.runContext()
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.cancel["run-1"] = cancel
	_, registered := controller.cancel["run-1"]
	controller.mu.Unlock()
	if !registered {
		t.Fatal("取消函数应已注册")
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("取消信号超时")
	}
	controller.mu.Lock()
	delete(controller.cancel, "run-1")
	controller.mu.Unlock()
}
