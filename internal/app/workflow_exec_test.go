// 工作流执行链的应用层合同测试（ADR-0021 片 2）：创建前置失败关闭、顺序推进、
// 人工审批门、恢复唯一性、验证证据边界与配置漂移拒绝。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowsqlite "github.com/fxbin/cairn/internal/adapters/workflow/sqlite"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/workflow"
)

// workflowExecConfig 返回允许写入与执行的应用层测试配置；验证命令为空时
// 不配置任何验证步骤，用例据此验证创建前的失败关闭。
func workflowExecConfig(verificationArgv []string) config.Config {
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write = "allow"
	configuration.Policy.Exec = "allow"
	if len(verificationArgv) > 0 {
		configuration.Verification.Commands = []config.VerificationCommand{{Argv: verificationArgv}}
	}
	return configuration
}

// countingWorkflowProvider 记录模型调用次数；验证步骤与人工门不得触发模型调用。
type countingWorkflowProvider struct {
	calls int
}

// Stream 记录一次模型调用并返回最小完成流。
func (provider *countingWorkflowProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// workflowStepStates 把状态视图压成步骤 ID 到状态的映射，便于断言。
func workflowStepStates(view WorkflowStatusView) map[string]string {
	states := make(map[string]string, len(view.Steps))
	for _, step := range view.Steps {
		states[step.ID] = step.State
	}
	return states
}

// workflowChainEvents 读取工作流链的全部事件，供追溯性断言使用。
func workflowChainEvents(t *testing.T, workspace, stateDir string, configuration config.Config, id workflow.ID) []workflow.Event {
	t.Helper()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	store, err := workflowsqlite.Open(workflowDBPath(stateDir, hash))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	events, err := store.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("load workflow chain: %v", err)
	}
	return events
}

// seedWorkflowVerifyRun 写入一个未收束的工作流验证 run：创建事件绑定验证计划
// 制品，恢复入口据此以零调用脚本化计划继续，不得回落到模型续跑。
func seedWorkflowVerifyRun(t *testing.T, workspace, stateDir string, configuration config.Config, workflowID workflow.ID, stepID string) domain.RunID {
	t.Helper()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	artifacts, err := openArtifactStore(stateDir, hash, configuration.Output)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	configRef := putTestFrozenRunConfig(t, stateDir, hash, configuration)
	planRef, err := artifacts.Put(artifact.MustEncodeRedactedJSON(workflowVerifyPlanArtifact{
		Version: workflowVerifyPlanVersion, Kind: workflowVerifyPlanKind, WorkflowID: string(workflowID), StepID: stepID,
	}), "application/json")
	if err != nil {
		t.Fatalf("store verify plan artifact: %v", err)
	}
	build, err := currentBuildIdentity()
	if err != nil {
		t.Fatalf("build identity: %v", err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer journal.Close()
	runID := domain.NewRunID()
	drafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
			domain.RunCreatedPayload
			ConfigRef artifact.Ref `json:"config_ref"`
			TaskRef   artifact.Ref `json:"task_ref"`
		}{RunCreatedPayload: domain.RunCreatedPayload{
			Task: "workflow verify", Workspace: workspace, ConfigFingerprint: configuration.Fingerprint(), Build: build,
		}, ConfigRef: configRef, TaskRef: planRef})},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunStarted},
	}
	for index, draft := range drafts {
		if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatalf("append run event: %v", err)
		}
	}
	return runID
}

// seedWorkflowChainWithBoundVerifyStep 写入一条执行步骤已成功、验证步骤已开始
// 并绑定验证 run 的工作流链；任务制品绑定齐备，可被恢复入口读取。
func seedWorkflowChainWithBoundVerifyStep(t *testing.T, workspace, stateDir string, configuration config.Config, runID domain.RunID) workflow.ID {
	t.Helper()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	artifacts, err := openArtifactStore(stateDir, hash, configuration.Output)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	taskRef, err := artifacts.Put([]byte("verify the workspace"), "text/plain; charset=utf-8")
	if err != nil {
		t.Fatalf("store task artifact: %v", err)
	}
	store, err := workflowsqlite.Open(workflowDBPath(stateDir, hash))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	id := workflow.NewID()
	steps := workflowFixedPlanSteps()
	created, err := store.Append(context.Background(), 0, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventRunCreated,
		Payload: artifact.MustEncodeRedactedJSON(workflow.RunCreatedPayload{
			Steps:               steps,
			PlanSHA256:          workflowPlanDigest(steps, taskRef.SHA256, configuration.Fingerprint()),
			ConfigFingerprint:   configuration.Fingerprint(),
			AuthorizationSource: workflowAuthorizationRun,
			Task:                workflow.StepIO{Ref: taskRef.SHA256, SHA: taskRef.SHA256, Summary: "workflow task"},
		})})
	if err != nil {
		t.Fatalf("append created: %v", err)
	}
	started, err := store.Append(context.Background(), created.Seq, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventStepStarted,
		Payload: artifact.MustEncodeRedactedJSON(workflow.StepStartedPayload{StepID: workflowStepExecuteID})})
	if err != nil {
		t.Fatalf("append execute started: %v", err)
	}
	finished, err := store.Append(context.Background(), started.Seq, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventStepFinished,
		Payload: artifact.MustEncodeRedactedJSON(workflow.StepFinishedPayload{
			StepID: workflowStepExecuteID, State: workflow.StepSucceeded, Output: workflow.StepIO{Summary: "executed"},
		})})
	if err != nil {
		t.Fatalf("append execute finished: %v", err)
	}
	if _, err := store.Append(context.Background(), finished.Seq, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventStepStarted,
		Payload: artifact.MustEncodeRedactedJSON(workflow.StepStartedPayload{
			StepID: workflowStepVerifyID, RunID: string(runID),
		})}); err != nil {
		t.Fatalf("append verify started: %v", err)
	}
	return id
}

// seedWorkflowChainWithoutTaskBinding 写入缺少任务制品绑定的旧链，用于验证恢复
// 入口失败关闭而不是猜测任务正文。
func seedWorkflowChainWithoutTaskBinding(t *testing.T, workspace, stateDir string, configuration config.Config) workflow.ID {
	t.Helper()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	store, err := workflowsqlite.Open(workflowDBPath(stateDir, hash))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	id := workflow.NewID()
	steps := workflowFixedPlanSteps()
	created, err := store.Append(context.Background(), 0, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventRunCreated,
		Payload: artifact.MustEncodeRedactedJSON(workflow.RunCreatedPayload{
			Steps:               steps,
			PlanSHA256:          workflowPlanDigest(steps, "sha256:legacy", configuration.Fingerprint()),
			ConfigFingerprint:   configuration.Fingerprint(),
			AuthorizationSource: workflowAuthorizationRun,
		})})
	if err != nil {
		t.Fatalf("append created: %v", err)
	}
	if _, err := store.Append(context.Background(), created.Seq, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventStepStarted,
		Payload: artifact.MustEncodeRedactedJSON(workflow.StepStartedPayload{StepID: workflowStepExecuteID})}); err != nil {
		t.Fatalf("append started: %v", err)
	}
	return id
}

// seedWorkflowChainWithRunningStep 写入一条执行步骤已开始、但绑定的 run 从未
// 创建的工作流链，用于覆盖「记步骤开始」与「创建 run」之间的中断窗口。
func seedWorkflowChainWithRunningStep(t *testing.T, workspace, stateDir string, configuration config.Config, runID string) workflow.ID {
	t.Helper()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	artifacts, err := openArtifactStore(stateDir, hash, configuration.Output)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	taskRef, err := artifacts.Put([]byte("execute the task"), "text/plain; charset=utf-8")
	if err != nil {
		t.Fatalf("store task artifact: %v", err)
	}
	store, err := workflowsqlite.Open(workflowDBPath(stateDir, hash))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	id := workflow.NewID()
	steps := workflowFixedPlanSteps()
	created, err := store.Append(context.Background(), 0, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventRunCreated,
		Payload: artifact.MustEncodeRedactedJSON(workflow.RunCreatedPayload{
			Steps:               steps,
			PlanSHA256:          workflowPlanDigest(steps, taskRef.SHA256, configuration.Fingerprint()),
			ConfigFingerprint:   configuration.Fingerprint(),
			AuthorizationSource: workflowAuthorizationRun,
			Task:                workflow.StepIO{Ref: taskRef.SHA256, SHA: taskRef.SHA256, Summary: "workflow task"},
		})})
	if err != nil {
		t.Fatalf("append created: %v", err)
	}
	if _, err := store.Append(context.Background(), created.Seq, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventStepStarted,
		Payload: artifact.MustEncodeRedactedJSON(workflow.StepStartedPayload{
			StepID: workflowStepExecuteID, RunID: runID,
		})}); err != nil {
		t.Fatalf("append step started: %v", err)
	}
	return id
}

// TestWorkflowRunFailsClosedWithoutFrozenVerificationCommands 锁定创建前置门：
// 计划含验证步骤而冻结验证命令为空时创建即失败关闭，不写任何状态。
func TestWorkflowRunFailsClosedWithoutFrozenVerificationCommands(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig(nil)
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &countingWorkflowProvider{}, nil }}
	_, err := service.WorkflowRun(context.Background(), WorkflowRunOptions{
		Task: "create the artifact", Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err == nil || !strings.Contains(err.Error(), "frozen verification commands") {
		t.Fatalf("workflow run error = %v", err)
	}
	assertStateDirEmpty(t, stateDir)
}

// TestWorkflowRunRejectsEmptyTask 锁定空任务不进入任何持久化路径。
func TestWorkflowRunRejectsEmptyTask(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &countingWorkflowProvider{}, nil }}
	_, err := service.WorkflowRun(context.Background(), WorkflowRunOptions{
		Task: "   ", Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err == nil || !strings.Contains(err.Error(), "task is required") {
		t.Fatalf("workflow run error = %v", err)
	}
	assertStateDirEmpty(t, stateDir)
}

// TestWorkflowRunDrivesExecuteAndVerifyThenStopsAtAcceptGate 锁定顺序推进合同：
// 执行步骤由普通 run 驱动、验证步骤由零调用脚本化 run 驱动，两者成功后停在
// 人工审批门，工作流不自动进入 accepted。
func TestWorkflowRunDrivesExecuteAndVerifyThenStopsAtAcceptGate(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	outcome, err := service.WorkflowRun(context.Background(), WorkflowRunOptions{
		Task: "create the artifact", Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("workflow run: %v", err)
	}
	if outcome.View.Finished {
		t.Fatalf("workflow must stop at the accept gate: %+v", outcome.View)
	}
	states := workflowStepStates(outcome.View)
	if states[workflowStepExecuteID] != string(workflow.StepSucceeded) {
		t.Fatalf("execute step = %q", states[workflowStepExecuteID])
	}
	if states[workflowStepVerifyID] != string(workflow.StepSucceeded) {
		t.Fatalf("verify step = %q", states[workflowStepVerifyID])
	}
	if states[workflowStepAcceptID] != string(workflow.StepPending) {
		t.Fatalf("accept step = %q", states[workflowStepAcceptID])
	}
	if !strings.Contains(outcome.NextAction, "workflow accept") {
		t.Fatalf("next action = %q", outcome.NextAction)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "created.txt"))
	if err != nil || string(content) != "created" {
		t.Fatalf("created.txt = %q err = %v", content, err)
	}
	// 追溯性：执行与验证步骤各自绑定自己的 run，链上可逐条核对。
	bound := map[string]string{}
	for _, event := range workflowChainEvents(t, workspace, stateDir, configuration, workflow.ID(outcome.View.WorkflowID)) {
		if event.Type != workflow.EventStepStarted {
			continue
		}
		var payload workflow.StepStartedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode step started payload: %v", err)
		}
		bound[payload.StepID] = payload.RunID
	}
	if bound[workflowStepExecuteID] == "" || bound[workflowStepVerifyID] == "" {
		t.Fatalf("step runs are not bound in the chain: %+v", bound)
	}
	if bound[workflowStepExecuteID] == bound[workflowStepVerifyID] {
		t.Fatal("execute and verify steps must not share the same run")
	}
}

// TestWorkflowAcceptCompletesWorkflowAtAcceptGate 锁定人工门：只有停在 accept
// 步骤时显式授权才写入步骤成功并收束工作流。
func TestWorkflowAcceptCompletesWorkflowAtAcceptGate(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &patchThenFinalProvider{}, nil }}
	outcome, err := service.WorkflowRun(context.Background(), WorkflowRunOptions{
		Task: "create the artifact", Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("workflow run: %v", err)
	}
	id := workflow.ID(outcome.View.WorkflowID)
	accepted, err := service.WorkflowAccept(context.Background(), WorkflowAcceptOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("workflow accept: %v", err)
	}
	if !accepted.View.Finished || accepted.View.FinishState != workflow.RunStateCompleted {
		t.Fatalf("accepted view = %+v", accepted.View)
	}
	if workflowStepStates(accepted.View)[workflowStepAcceptID] != string(workflow.StepSucceeded) {
		t.Fatalf("accept step = %q", workflowStepStates(accepted.View)[workflowStepAcceptID])
	}
	// 终态工作流不再接受第二次授权。
	if _, err := service.WorkflowAccept(context.Background(), WorkflowAcceptOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); !errors.Is(err, ErrWorkflowFinished) {
		t.Fatalf("second accept error = %v", err)
	}
}

// TestWorkflowAcceptRefusesWhenNotAtAcceptGate 锁定人工门没有自动通过路径：工作
// 流尚未推进到 accept 步骤时拒绝授权。
func TestWorkflowAcceptRefusesWhenNotAtAcceptGate(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &patchThenFinalProvider{}, nil }}
	outcome, err := service.WorkflowRun(context.Background(), WorkflowRunOptions{
		Task: "create the artifact", Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("workflow run: %v", err)
	}
	// 未开始的工作流不在 accept 门：直接对新建链授权必须被拒绝。
	id := seedWorkflowChainWithoutTaskBinding(t, workspace, stateDir, configuration)
	if _, err := service.WorkflowAccept(context.Background(), WorkflowAcceptOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); err == nil {
		t.Fatal("accept must be refused when the workflow is not at the accept gate")
	} else if !strings.Contains(err.Error(), "not waiting at the accept gate") {
		t.Fatalf("accept error = %v", err)
	}
	if !strings.Contains(outcome.NextAction, "workflow accept") {
		t.Fatalf("next action = %q", outcome.NextAction)
	}
}

// TestWorkflowResumeRejectsConfigurationDrift 锁定恢复前置门：配置指纹漂移时
// 失败关闭，不按旧指纹继续执行。
func TestWorkflowResumeRejectsConfigurationDrift(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &patchThenFinalProvider{}, nil }}
	outcome, err := service.WorkflowRun(context.Background(), WorkflowRunOptions{
		Task: "create the artifact", Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("workflow run: %v", err)
	}
	drifted := configuration
	drifted.Provider.Model = "drifted-model"
	_, err = service.WorkflowResume(context.Background(), WorkflowResumeOptions{
		WorkflowID: workflow.ID(outcome.View.WorkflowID), Workspace: workspace, StateDir: stateDir, Config: drifted,
	})
	if !errors.Is(err, ErrConfigDrift) {
		t.Fatalf("resume error = %v", err)
	}
}

// TestWorkflowResumeFailsClosedForChainWithoutTaskBinding 锁定旧链兼容边界：
// 缺少任务制品绑定时失败关闭，不猜测任务正文。
func TestWorkflowResumeFailsClosedForChainWithoutTaskBinding(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	id := seedWorkflowChainWithoutTaskBinding(t, workspace, stateDir, configuration)
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &countingWorkflowProvider{}, nil }}
	_, err := service.WorkflowResume(context.Background(), WorkflowResumeOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err == nil || !strings.Contains(err.Error(), "task artifact binding") {
		t.Fatalf("resume error = %v", err)
	}
}

// TestWorkflowResumeFailsClosedWithoutStore 锁定存储缺失失败关闭，不隐式建库。
func TestWorkflowResumeFailsClosedWithoutStore(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &countingWorkflowProvider{}, nil }}
	_, err := service.WorkflowResume(context.Background(), WorkflowResumeOptions{
		WorkflowID: workflow.NewID(), Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("resume error = %v", err)
	}
}

// TestWorkflowResumeKeepsAcceptGateStable 锁定恢复唯一性：进程重启后在人工门
// 恢复时唯一下一步仍是人工门，不重复执行任何步骤。
func TestWorkflowResumeKeepsAcceptGateStable(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	provider := &countingWorkflowProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	outcome, err := service.WorkflowRun(context.Background(), WorkflowRunOptions{
		Task: "answer the question", Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("workflow run: %v", err)
	}
	id := workflow.ID(outcome.View.WorkflowID)
	before := len(workflowChainEvents(t, workspace, stateDir, configuration, id))
	resumed, err := service.WorkflowResume(context.Background(), WorkflowResumeOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("workflow resume: %v", err)
	}
	if resumed.View.Finished {
		t.Fatalf("resume must not finish the workflow: %+v", resumed.View)
	}
	if states := workflowStepStates(resumed.View); states[workflowStepAcceptID] != string(workflow.StepPending) {
		t.Fatalf("accept step = %q", states[workflowStepAcceptID])
	}
	if after := len(workflowChainEvents(t, workspace, stateDir, configuration, id)); after != before {
		t.Fatalf("resume at the accept gate must not append events: %d -> %d", before, after)
	}
}

// seedCancelledRun 写入一个已取消的运行：取消是终态事实，恢复入口据此把步骤
// 与剩余步骤按取消收束。运行携带冻结配置引用，与真实运行保持同一合同。
func seedCancelledRun(t *testing.T, stateDir, hash string, configuration config.Config) domain.RunID {
	t.Helper()
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer journal.Close()
	configRef := putTestFrozenRunConfig(t, stateDir, hash, configuration)
	runID := domain.NewRunID()
	drafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
			domain.RunCreatedPayload
			ConfigRef artifact.Ref `json:"config_ref"`
		}{RunCreatedPayload: domain.RunCreatedPayload{
			Task: "cancelled run", Workspace: "/workspace", ConfigFingerprint: configuration.Fingerprint(),
		}, ConfigRef: configRef})},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunStarted},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCancelRequested, Payload: json.RawMessage(`{"cause":"user_cancelled"}`)},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCancelled, Payload: json.RawMessage(`{"cause":"user_cancelled"}`)},
	}
	for index, draft := range drafts {
		if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatalf("append run event: %v", err)
		}
	}
	return runID
}

// TestWorkflowRunRejectsCancelledContextBeforeAnyState 锁定驱动前取消：上下文
// 已取消时不创建工作流，也不写任何状态。
func TestWorkflowRunRejectsCancelledContextBeforeAnyState(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &countingWorkflowProvider{}, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.WorkflowRun(ctx, WorkflowRunOptions{
		Task: "answer the question", Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("workflow run error = %v", err)
	}
	assertStateDirEmpty(t, stateDir)
}

// TestWorkflowResumeRecordsCancellationForRemainingSteps 锁定取消后的唯一收束：
// 绑定的运行已取消时，恢复入口把运行中的步骤与剩余步骤按取消事实收束，并写入
// 工作流终态 cancelled。
func TestWorkflowResumeRecordsCancellationForRemainingSteps(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	runID := seedCancelledRun(t, stateDir, hash, configuration)
	id := seedWorkflowChainWithRunningStep(t, workspace, stateDir, configuration, string(runID))
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &countingWorkflowProvider{}, nil }}
	outcome, err := service.WorkflowResume(context.Background(), WorkflowResumeOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("workflow resume error = %v", err)
	}
	if !outcome.View.Finished || outcome.View.FinishState != workflow.RunStateCancelled {
		t.Fatalf("view = %+v", outcome.View)
	}
	states := workflowStepStates(outcome.View)
	if states[workflowStepExecuteID] != string(workflow.StepCancelled) {
		t.Fatalf("execute step = %q", states[workflowStepExecuteID])
	}
	if states[workflowStepVerifyID] != string(workflow.StepCancelled) {
		t.Fatalf("verify step = %q", states[workflowStepVerifyID])
	}
	if states[workflowStepAcceptID] != string(workflow.StepCancelled) {
		t.Fatalf("accept step = %q", states[workflowStepAcceptID])
	}
}

// TestWorkflowResumeFailsClosedWhenBoundRunIsMissing 锁定中断窗口的失败关闭：
// 步骤已记开始但绑定的 run 从未创建时，恢复入口不猜测、不自动重试，也不推进
// 后续步骤，并指明唯一出路。
func TestWorkflowResumeFailsClosedWhenBoundRunIsMissing(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/true"})
	id := seedWorkflowChainWithRunningStep(t, workspace, stateDir, configuration, string(domain.NewRunID()))
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &countingWorkflowProvider{}, nil }}
	outcome, err := service.WorkflowResume(context.Background(), WorkflowResumeOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err == nil {
		t.Fatal("workflow resume must fail closed when the bound run is missing")
	}
	if !errors.Is(err, ErrStateCorrupt) || !strings.Contains(err.Error(), "references missing run") {
		t.Fatalf("resume error = %v", err)
	}
	if outcome.View.Finished {
		t.Fatalf("workflow must not finish on an unresolvable step: %+v", outcome.View)
	}
	states := workflowStepStates(outcome.View)
	if states[workflowStepExecuteID] != string(workflow.StepRunning) {
		t.Fatalf("execute step = %q", states[workflowStepExecuteID])
	}
	if states[workflowStepVerifyID] != string(workflow.StepPending) {
		t.Fatalf("verify step = %q", states[workflowStepVerifyID])
	}
}

// TestWorkflowResumeFailsWorkflowWhenVerificationFails 锁定验证证据边界：验证未
// 通过时步骤收束为失败、工作流终态为 failed，人工门拒绝通过。
func TestWorkflowResumeFailsWorkflowWhenVerificationFails(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := workflowExecConfig([]string{"/usr/bin/false"})
	provider := &countingWorkflowProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	workflowID := workflow.NewID()
	runID := seedWorkflowVerifyRun(t, workspace, stateDir, configuration, workflowID, workflowStepVerifyID)
	id := seedWorkflowChainWithBoundVerifyStep(t, workspace, stateDir, configuration, runID)
	outcome, err := service.WorkflowResume(context.Background(), WorkflowResumeOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("workflow resume: %v", err)
	}
	if !outcome.View.Finished || outcome.View.FinishState != workflow.RunStateFailed {
		t.Fatalf("view = %+v", outcome.View)
	}
	states := workflowStepStates(outcome.View)
	if states[workflowStepVerifyID] != string(workflow.StepFailed) {
		t.Fatalf("verify step = %q", states[workflowStepVerifyID])
	}
	if states[workflowStepAcceptID] != string(workflow.StepPending) {
		t.Fatalf("accept step = %q", states[workflowStepAcceptID])
	}
	if provider.calls != 0 {
		t.Fatalf("verification step must not call the model, calls = %d", provider.calls)
	}
	// 验证失败不得进入 accepted：人工门必须拒绝已失败的工作流。
	if _, err := service.WorkflowAccept(context.Background(), WorkflowAcceptOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); !errors.Is(err, ErrWorkflowFinished) {
		t.Fatalf("accept error = %v", err)
	}
}
