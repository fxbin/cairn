//go:build faultinject

// workflow_fault_test.go 锁定工作流执行链在故障窗口下的失败关闭语义：副作用已
// started 但未收束时，恢复入口必须要求人工对账，不得自动重试，也不得推进后续
// 步骤。
package fault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	journalsqlite "github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	workflowsqlite "github.com/fxbin/cairn/internal/adapters/workflow/sqlite"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
	"github.com/fxbin/cairn/internal/workflow"
)

// faultWorkflowConfig 返回故障注入用例的固定配置：显式声明测试 Provider 能力，
// 指纹与链上创建事件保持同一口径。
func faultWorkflowConfig() config.Config {
	configuration := config.Defaults()
	configuration.Provider.BaseURL = "https://provider.test/v1"
	configuration.Provider.Model = "test-model"
	configuration.Provider.Admission = config.ProviderAdmissionOptional
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	return configuration
}

// faultWorkspaceHash 以与应用层一致的方式计算工作区状态哈希。
func faultWorkspaceHash(t *testing.T, workspace string, configuration config.Config) string {
	t.Helper()
	view, err := tools.NewWorkspace(workspace, configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
	if err != nil {
		t.Fatalf("create workspace view: %v", err)
	}
	digest := sha256.Sum256([]byte(view.Root))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// seedDanglingWorkspaceRun 写入一个只到 operation_started 的运行：副作用是否生
// 效不可知，运行保持非终态且处于等待对账状态。
func seedDanglingWorkspaceRun(t *testing.T, workspace, stateDir, hash string) domain.RunID {
	t.Helper()
	path := filepath.Join(stateDir, "workspaces", hash, "runs.db")
	journal, err := journalsqlite.Open(path)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer journal.Close()
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	drafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"fault workflow","workspace":"` + workspace + `"}`)},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunStarted},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"patch","tool_name":"apply_patch","arguments":{"path":"x.txt","mode":"create","content":"x"}}`)},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace","planned_path":"x.txt","planned_before":"absent","planned_after":"sha256:after"}`)},
	}
	for index, draft := range drafts {
		if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatalf("append run event: %v", err)
		}
	}
	return runID
}

// seedWorkflowChainBoundToRun 写入一条执行步骤已开始并绑定故障运行的工作流链。
func seedWorkflowChainBoundToRun(t *testing.T, stateDir, hash string, configuration config.Config, runID domain.RunID) workflow.ID {
	t.Helper()
	artifacts, err := artifact.New(filepath.Join(stateDir, "workspaces", hash, "artifacts"), configuration.Output.MaxPreviewBytes)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	taskRef, err := artifacts.Put([]byte("fault workflow task"), "text/plain; charset=utf-8")
	if err != nil {
		t.Fatalf("store task artifact: %v", err)
	}
	store, err := workflowsqlite.Open(filepath.Join(stateDir, "workspaces", hash, "workflow.db"))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	id := workflow.NewID()
	steps := []workflow.StepSpec{
		{ID: "execute", Index: 0, Kind: workflow.StepKindExecute},
		{ID: "verify", Index: 1, Kind: workflow.StepKindVerify},
	}
	created, err := store.Append(context.Background(), 0, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventRunCreated,
		Payload: artifact.MustEncodeRedactedJSON(workflow.RunCreatedPayload{
			Steps:               steps,
			PlanSHA256:          "sha256:fault-plan",
			ConfigFingerprint:   configuration.Fingerprint(),
			AuthorizationSource: "cli:workflow-run",
			Task:                workflow.StepIO{Ref: taskRef.SHA256, SHA: taskRef.SHA256, Summary: "workflow task"},
		})})
	if err != nil {
		t.Fatalf("append created: %v", err)
	}
	if _, err := store.Append(context.Background(), created.Seq, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventStepStarted,
		Payload: artifact.MustEncodeRedactedJSON(workflow.StepStartedPayload{StepID: "execute", RunID: string(runID)})}); err != nil {
		t.Fatalf("append step started: %v", err)
	}
	return id
}

// TestFaultInjectWorkflowDanglingSideEffectRequiresReconciliation 锁定工作流的故
// 障窗口：副作用 started 后进程中断时，恢复入口只登记不确定状态并要求人工对账，
// 不重放副作用、不推进后续步骤。
func TestFaultInjectWorkflowDanglingSideEffectRequiresReconciliation(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := faultWorkflowConfig()
	hash := faultWorkspaceHash(t, workspace, configuration)
	runID := seedDanglingWorkspaceRun(t, workspace, stateDir, hash)
	id := seedWorkflowChainBoundToRun(t, stateDir, hash, configuration, runID)
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return nil, errProviderNotConstructed
	}}
	outcome, err := service.WorkflowResume(context.Background(), app.WorkflowResumeOptions{
		WorkflowID: id, Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err == nil {
		t.Fatal("workflow resume must refuse to advance an unresolved side effect")
	}
	if !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("resume error = %v", err)
	}
	if outcome.View.Finished {
		t.Fatalf("workflow must not finish while a side effect is unresolved: %+v", outcome.View)
	}
	states := map[string]string{}
	for _, step := range outcome.View.Steps {
		states[step.ID] = step.State
	}
	if states["execute"] != string(workflow.StepUncertain) {
		t.Fatalf("execute step = %q", states["execute"])
	}
	if states["verify"] != string(workflow.StepPending) {
		t.Fatalf("verify step = %q", states["verify"])
	}
	// 副作用没有被重放：故障运行的事件链没有新增任何操作启动事件。
	journal, err := journalsqlite.Open(filepath.Join(stateDir, "workspaces", hash, "runs.db"))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer journal.Close()
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run events: %v", err)
	}
	started := 0
	for _, event := range events {
		if event.Type == domain.EventOperationStarted {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("dangling side effect was retried: operation_started count = %d", started)
	}
}

// errProviderNotConstructed 表示恢复路径不应构造模型 Provider。
var errProviderNotConstructed = errorString("provider must not be constructed during workflow resume")

// errorString 是最小错误类型，避免为单一断言引入额外依赖。
type errorString string

// Error 返回固定文本。
func (err errorString) Error() string { return string(err) }
