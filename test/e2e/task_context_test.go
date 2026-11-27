//go:build e2e

// 本文件回归任务上下文在进程丢失后、对账前的只读投影：未了结的副作用必须明确出现。
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
)

// TestTaskContextReportsUnsettledSideEffectsAfterProcessLoss 锁定跨进程交接的一个关键
// 场景：执行进程在副作用操作进行中被杀、尚未对账时，任务上下文把未了结的操作列为
// 已知事实并标记待对账，交接摘要里同样可见，验证报告则明确是尚无事实。
func TestTaskContextReportsUnsettledSideEffectsAfterProcessLoss(t *testing.T) {
	workspace, state, control := t.TempDir(), t.TempDir(), t.TempDir()
	started, release := filepath.Join(control, "started"), filepath.Join(control, "release")
	check := buildBinary(t, "cairn-task-context-check", "", `package main
import ("os"; "time")
func main() {
	f, err := os.OpenFile(os.Args[1], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil { panic(err) }
	if _, err := f.WriteString("started\n"); err != nil { panic(err) }
	if err := f.Close(); err != nil { panic(err) }
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(os.Args[2]); err == nil { return }
		time.Sleep(25 * time.Millisecond)
	}
}
`)
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	configPath := filepath.Join(control, "config.yaml")
	configuration := baseSignalConfig(autonomousProvider(t)) + fmt.Sprintf(
		"\npolicy:\n  write: allow\n  exec: allow\nverification:\n  commands:\n    - id: check\n      argv: [%q, %q, %q]\n", check, started, release)
	if err := os.WriteFile(configPath, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	fixture := taskCLI{buildCairnBinary(t), workspace, state, configPath}
	var current app.Task
	if err := json.Unmarshal(fixture.run(t, 0, "task", "create", "实现 Go 函数", "--accept", "固定检查通过", "--check", "check"), &current); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := fixture.command(ctx, "task", "automate", string(current.ID), "--revision", "1", "--rework-attempts", "1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	waitForFile(t, started, "program operation wrote its marker")
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("interrupted process unexpectedly exited normally")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}

	// 对账之前读取：未了结的副作用是已知事实，不能被表达成“没有问题”。
	var taskContext contract.TaskContextV1
	if err := json.Unmarshal(fixture.run(t, 0, "task", "context", string(current.ID)), &taskContext); err != nil {
		t.Fatal(err)
	}
	if err := taskContext.Validate(); err != nil {
		t.Fatalf("task context is not self-consistent: %v", err)
	}
	if taskContext.UnresolvedSideEffects.Status != contract.TaskFactKnown || taskContext.UnresolvedSideEffects.Value == nil {
		t.Fatalf("unresolved side effects = %+v", taskContext.UnresolvedSideEffects)
	}
	effects := taskContext.UnresolvedSideEffects.Value
	if len(effects.Unsettled) == 0 || !effects.PendingReconcile {
		t.Fatalf("unsettled side effects must be listed and marked for reconcile: %+v", effects)
	}
	for name, fact := range map[string]contract.TaskFactStatus{
		"goal": taskContext.Goal.Status, "frozen_acceptance": taskContext.FrozenAcceptance.Status,
		"progress": taskContext.Progress.Status, "runs": taskContext.Runs.Status, "permissions": taskContext.Permissions.Status,
	} {
		if fact != contract.TaskFactKnown {
			t.Fatalf("%s = %s, want known", name, fact)
		}
	}
	if taskContext.Progress.Value.WorkflowPhase != "waiting_reconcile" {
		t.Fatalf("workflow phase = %q, want waiting_reconcile", taskContext.Progress.Value.WorkflowPhase)
	}
	if taskContext.Evidence.Status != contract.TaskFactUnknown || taskContext.Evidence.Reason != contract.TaskReasonReportNotGenerated {
		t.Fatalf("evidence = %s (%s), want unknown without a report", taskContext.Evidence.Status, taskContext.Evidence.Reason)
	}

	// 不带 --json 的交接摘要同样必须让人看到未了结的操作。
	readCtx, readCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer readCancel()
	handoffCommand := exec.CommandContext(readCtx, fixture.binary, "task", "context", string(current.ID),
		"--workspace", fixture.workspace, "--state-dir", fixture.state, "--config", fixture.config)
	handoffCommand.Env = append(os.Environ(), "CAIRN_E2E_SIGNAL_KEY=e2e-task-fixture")
	output, err := handoffCommand.Output()
	if err != nil {
		t.Fatalf("handoff command: %v", err)
	}
	handoff := string(output)
	for _, required := range []string{"unresolved side effects started=", "pending_reconcile=true", contract.TaskHandoffDisclaimer} {
		if !strings.Contains(handoff, required) {
			t.Fatalf("handoff missing %q:\n%s", required, handoff)
		}
	}
	if strings.Contains(handoff, "unsettled=none") {
		t.Fatalf("handoff hides the unsettled operation:\n%s", handoff)
	}
}
