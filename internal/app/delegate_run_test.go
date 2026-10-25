package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// delegateThenFinalProvider 按调用序编排父委派与子/父收尾：第 1 次父提议
// delegate_task，第 2 次子收尾，其后父收尾。
type delegateThenFinalProvider struct{ calls int }

func (provider *delegateThenFinalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	switch provider.calls {
	case 1:
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "delegate-1", Name: "delegate_task", Arguments: json.RawMessage(`{"task":"child work"}`)}}); err != nil {
			return err
		}
	case 2:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "child done"}); err != nil {
			return err
		}
	default:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "parent done"}); err != nil {
			return err
		}
	}
	return emitTestCompletion(emit)
}

// delegatePauseProvider 编排审批暂停路径：父委派 → 子提议写入触发审批暂停 →
// 父继续收尾 → 子经恢复后收尾。
type delegatePauseProvider struct{ calls int }

func (provider *delegatePauseProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	switch provider.calls {
	case 1:
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "delegate-1", Name: "delegate_task", Arguments: json.RawMessage(`{"task":"patch child","max_turns":5}`)}}); err != nil {
			return err
		}
	case 2:
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"child.txt","mode":"create","content":"child"}`)}}); err != nil {
			return err
		}
	case 3:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "parent continues"}); err != nil {
			return err
		}
	default:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "child finished"}); err != nil {
			return err
		}
	}
	return emitTestCompletion(emit)
}

// delegateTestConfig 返回委派测试通用配置：exec 放行、含通过式验证命令。
func delegateTestConfig() config.Config {
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	return configuration
}

// onlyChildRun 返回父投影中的唯一子运行标识。
func onlyChildRun(t *testing.T, projection domain.RunProjection) string {
	t.Helper()
	if len(projection.Children) != 1 {
		t.Fatalf("expected exactly one delegated child, got %d", len(projection.Children))
	}
	for childID := range projection.Children {
		return childID
	}
	return ""
}

// TestDelegateTaskRunsChildRunToCompletion 端到端验证模型侧委派：父经
// delegate_task 受控启动子 run，子完成后父 Journal 同时含 delegated 与
// completed 事实，父自身正常收尾。
func TestDelegateTaskRunsChildRunToCompletion(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := delegateTestConfig()
	provider := &delegateThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	childID := onlyChildRun(t, inspected.Projection)
	record := inspected.Projection.Children[childID]
	if record.State != "completed" || record.Outcome != "ok" {
		t.Fatalf("child record = %+v", record)
	}
	delegated, completed := 0, 0
	for _, event := range inspected.Events {
		switch event.Type {
		case domain.EventSubagentDelegated:
			delegated++
		case domain.EventSubagentCompleted:
			completed++
		}
	}
	if delegated != 1 || completed != 1 {
		t.Fatalf("delegated=%d completed=%d", delegated, completed)
	}
}

// TestDelegateTaskSurfacesChildApprovalPauseAndResumeCoversNotification 验证
// 子审批暂停不是父错误：父继续收尾；子经恢复完成后，完成事实仍回写已终态
// 的父 Journal。
func TestDelegateTaskSurfacesChildApprovalPauseAndResumeCoversNotification(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := delegateTestConfig()
	provider := &delegatePauseProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	childID := onlyChildRun(t, inspected.Projection)
	if record := inspected.Projection.Children[childID]; record.State != "delegated" {
		t.Fatalf("paused child record = %+v", record)
	}
	// 子运行冻结的是裁剪后预算（max_turns=5），后续检查与恢复须用同构配置。
	childConfiguration := configuration
	childConfiguration.Budget.MaxTurns = 5
	childInspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, childConfiguration, domain.RunID(childID), true)
	if err != nil {
		t.Fatal(err)
	}
	pending := childInspected.Projection.PendingApproval
	if pending == nil {
		t.Fatalf("child is not waiting for approval: %+v", childInspected.Projection)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: domain.RunID(childID), Workspace: workspace, StateDir: stateDir, Config: childConfiguration, Approval: ports.Approval{Fingerprint: pending.Fingerprint, Approve: true}}); err != nil {
		t.Fatal(err)
	}
	assertLatestChildState(t, workspace, stateDir, configuration, result.ID, domain.RunID(childID), "completed", "ok")
}
