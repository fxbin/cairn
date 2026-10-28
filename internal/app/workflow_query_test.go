// 工作流只读查询的应用层测试：真实存储往返、未找到与损坏链失败关闭。
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	workflowsqlite "github.com/fxbin/cairn/internal/adapters/workflow/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/workflow"
)

// seedWorkflowStore 在指定工作区状态目录下创建真实工作流链并返回其 ID。
func seedWorkflowStore(t *testing.T, workspacePath, stateDir string) workflow.ID {
	t.Helper()
	configuration := config.Defaults()
	_, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	store, err := workflowsqlite.Open(workflowDBPath(stateDir, hash))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	id := workflow.NewID()
	created, err := store.Append(context.Background(), 0, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventRunCreated,
		Payload: mustJSON(t, workflow.RunCreatedPayload{
			Steps: []workflow.StepSpec{
				{ID: "prepare", Index: 0, Kind: workflow.StepKindExecute},
				{ID: "check", Index: 1, Kind: workflow.StepKindVerify},
			},
			PlanSHA256:          "sha256:plan",
			ConfigFingerprint:   "sha256:config",
			AuthorizationSource: "user-explicit",
		})})
	if err != nil {
		t.Fatalf("append created: %v", err)
	}
	if _, err := store.Append(context.Background(), created.Seq, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventStepStarted,
		Payload: mustJSON(t, workflow.StepStartedPayload{StepID: "prepare"})}); err != nil {
		t.Fatalf("append started: %v", err)
	}
	return id
}

// mustJSON 编码负载并在失败时终止测试。
func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return encoded
}

func TestWorkflowStatusReplaysPersistedChain(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	id := seedWorkflowStore(t, workspace, stateDir)
	view, err := Service{}.WorkflowStatus(context.Background(), workspace, stateDir, config.Defaults(), id)
	if err != nil {
		t.Fatalf("workflow status: %v", err)
	}
	if view.WorkflowID != string(id) || view.Authorization != "user-explicit" {
		t.Fatalf("view identity=%#v", view)
	}
	if len(view.Steps) != 2 || view.Steps[0].State != "running" || view.Steps[1].State != "pending" {
		t.Fatalf("steps=%#v", view.Steps)
	}
	if view.ResumeIndex != 0 || view.Finished {
		t.Fatalf("resume=%d finished=%t", view.ResumeIndex, view.Finished)
	}
}

func TestWorkflowStatusFailsClosedWithoutStore(t *testing.T) {
	_, err := Service{}.WorkflowStatus(context.Background(), t.TempDir(), t.TempDir(), config.Defaults(), workflow.NewID())
	if !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("want not-found, got %v", err)
	}
}

func TestWorkflowStatusUnknownWorkflowID(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	seedWorkflowStore(t, workspace, stateDir)
	_, err := Service{}.WorkflowStatus(context.Background(), workspace, stateDir, config.Defaults(), workflow.NewID())
	if !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("want not-found, got %v", err)
	}
}

func TestWorkflowStatusReportsCorruptChainAsStateCorrupt(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	id := seedWorkflowStore(t, workspace, stateDir)
	configuration := config.Defaults()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	tamper, err := sql.Open("sqlite", workflowDBPath(stateDir, hash))
	if err != nil {
		t.Fatalf("open tamper connection: %v", err)
	}
	defer tamper.Close()
	if _, err := tamper.Exec("UPDATE workflow_events SET event_hash = '0' WHERE workflow_id = ? AND seq = 1", id); err != nil {
		t.Fatalf("tamper event hash: %v", err)
	}
	_, err = Service{}.WorkflowStatus(context.Background(), workspace, stateDir, configuration, id)
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("want state corrupt, got %v", err)
	}
}

func TestWorkflowDBPathStaysInsideWorkspaceStateDirectory(t *testing.T) {
	got := workflowDBPath("/tmp/state", "abc")
	if filepath.Dir(got) != filepath.Join("/tmp/state", "workspaces", "abc") {
		t.Fatalf("workflow db path=%s", got)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("workflow db path must not be created eagerly: %v", err)
	}
}
