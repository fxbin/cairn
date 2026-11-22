// 本文件回归自主任务绑定、受控返工与显式验收。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/capabilities/task"
	tasksqlite "github.com/fxbin/cairn/internal/capabilities/task/sqlite"
	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

func newTaskWorkflowFixture(t *testing.T) (Service, TaskScope, Task) {
	t.Helper()
	cfg := workflowExecConfig(nil)
	cfg.Verification.Commands = []config.VerificationCommand{{ID: "check", Argv: []string{"/usr/bin/true"}}}
	scope := TaskScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: cfg}
	reviewer := &workflowReviewFixture{output: `{"findings":[]}`}
	worker := &patchThenFinalProvider{}
	service := Service{NewProvider: func(cfg config.Config) (ports.Provider, error) {
		if cfg.Tools.VerificationFilesOnly {
			return reviewer, nil
		}
		return worker, nil
	}}
	current, err := service.CreateTask(context.Background(), scope, TaskDefinition{
		Goal:     "Create a file with one known marker",
		Criteria: []contract.AcceptanceCriterion{{ID: "build", Statement: "验证已创建的文件", Required: true, Checks: []string{"check"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, scope, current
}

func TestTaskWorkflowBindingAndExplicitAcceptance(t *testing.T) {
	ctx := context.Background()
	service, scope, current := newTaskWorkflowFixture(t)
	options := TaskWorkflowOptions{Revision: current.Revision, ReworkAttempts: 1}
	view, err := service.StartTaskWorkflow(ctx, scope, current.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != task.Active || view.WorkflowBinding == nil || !view.WorkflowBinding.Attached ||
		view.Workflow == nil || view.Workflow.Verification == nil ||
		view.Workflow.Verification.Evaluation.Verdict != contract.VerdictPassed {
		t.Fatalf("task not awaiting explicit acceptance: %+v", view)
	}
	again, err := service.StartTaskWorkflow(ctx, scope, current.ID, options)
	if err != nil || again.WorkflowBinding.WorkflowID != view.WorkflowBinding.WorkflowID || again.Workflow.Sequence != view.Workflow.Sequence {
		t.Fatalf("duplicate start mutated state: %+v, %v", again, err)
	}
	if _, err := service.StartTaskWorkflow(ctx, scope, current.ID, TaskWorkflowOptions{Revision: current.Revision, ReworkAttempts: 2}); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("second grant changed rework budget: %v", err)
	}
	if _, err := service.RunTask(ctx, scope, current.ID, TaskRunOptions{Revision: view.Revision, AssignmentID: "r", SessionID: "s"}); !errors.Is(err, ErrTaskTransition) {
		t.Fatalf("manual assignment bypassed task workflow: %v", err)
	}
	if _, err := service.StartWorkflow(ctx, StartWorkflowOptions{Scope: WorkflowScope(scope), DefinitionID: WorkflowDefinitionID(view.WorkflowBinding.DefinitionID), Revision: 1}); err == nil {
		t.Fatal("private task definition runnable as a secondary workflow")
	}
	report := view.Workflow.Verification
	if _, err := service.CompleteTask(ctx, scope, current.ID, view.Revision, report.WorkerRunID); !errors.Is(err, ErrTaskUnverified) {
		t.Fatalf("task accepted Worker evidence: %v", err)
	}
	updated, err := service.UpdateTaskProgress(ctx, scope, current.ID, view.Revision, TaskProgress{
		Remaining: append(append([]string(nil), view.Progress.Remaining...), "需要人工确认额外发布检查"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteTask(ctx, scope, current.ID, updated.Revision, report.ReviewRunID); !errors.Is(err, ErrTaskTransition) {
		t.Fatalf("task accepted despite extra human action: %v", err)
	}
	updated, err = service.UpdateTaskProgress(ctx, scope, current.ID, updated.Revision, TaskProgress{Remaining: view.Progress.Remaining})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(scope.Workspace, "created.txt")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("drift"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteTask(ctx, scope, current.ID, updated.Revision, report.ReviewRunID); !errors.Is(err, ErrVerifierTarget) {
		t.Fatalf("task accepted drifted workspace: %v", err)
	}
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	completed, err := service.CompleteTask(ctx, scope, current.ID, updated.Revision, report.ReviewRunID)
	if err != nil || completed.State != task.Completed || completed.Completion == nil || completed.Completion.WorkflowID != view.Workflow.WorkflowID {
		t.Fatalf("explicit acceptance failed: %+v, %v", completed, err)
	}
	cold, err := (Service{}).GetTask(ctx, scope, current.ID)
	if err != nil || cold.State != task.Completed || !cold.Completion.Equal(*completed.Completion) {
		t.Fatalf("completion not replayed: %+v, %v", cold, err)
	}
}

func TestTaskWorkflowColdAttachment(t *testing.T) {
	for _, created := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_instance", true: "after_instance"}[created], func(t *testing.T) {
			ctx := context.Background()
			service, scope, current := newTaskWorkflowFixture(t)
			scope, hash, err := scope.resolve()
			if err != nil {
				t.Fatal(err)
			}
			binding, err := freezeTaskWorkflow(scope, current, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := current.PrepareWorkflow(binding); err != nil {
				t.Fatal(err)
			}
			store, err := tasksqlite.Open(ctx, scope.databasePath(hash), false)
			if err != nil {
				t.Fatal(err)
			}
			if err := commitTaskWorkflowBinding(ctx, store, &current); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if created {
				if err := ensureTaskWorkflowInstance(ctx, scope, current); err != nil {
					t.Fatal(err)
				}
				_, err := service.WorkflowResume(ctx, WorkflowResumeOptions{WorkflowID: workflow.ID(binding.WorkflowID), Workspace: scope.Workspace, StateDir: scope.StateDir, Config: scope.Config})
				if !errors.Is(err, ErrTaskTransition) {
					t.Fatalf("unconfirmed instance advanced: %v", err)
				}
			}
			view, err := service.ContinueTaskWorkflow(ctx, scope, current.ID, nil)
			if err != nil || view.WorkflowBinding == nil || !view.WorkflowBinding.Attached ||
				view.WorkflowBinding.WorkflowID != binding.WorkflowID || view.Workflow.Verification == nil {
				t.Fatalf("cold attachment did not recover identical instance: %+v, %v", view, err)
			}
			chainDB := workflowDBPath(scope.StateDir, hash)
			if err := os.Rename(chainDB, chainDB+".retained"); err != nil {
				t.Fatal(err)
			}
			if _, err := service.ContinueTaskWorkflow(ctx, scope, current.ID, nil); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("confirmed missing instance did not fail closed: %v", err)
			}
			if _, err := os.Stat(chainDB); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("confirmed instance was recreated: %v", err)
			}
		})
	}
}

// taskReworkProvider 第一次写入错误内容。在返工任务中按夹具结果修复或继续失败。
type taskReworkProvider struct {
	initial, rework bool
	repair          bool
}

func (provider *taskReworkProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	var rework bool
	for _, message := range request.Messages {
		if message.Role == "user" && strings.Contains(message.Content, "前次固定程序检查。") {
			rework = true
		}
	}
	var patch json.RawMessage
	switch {
	case !rework && !provider.initial:
		provider.initial = true
		patch = json.RawMessage(`{"path":"created.txt","mode":"create","content":"incorrect"}`)
	case rework && !provider.rework:
		provider.rework = true
		content := "still incorrect"
		if provider.repair {
			content = "expected"
		}
		patch = json.RawMessage(fmt.Sprintf(`{"path":"created.txt","mode":"update","expected_sha256":"sha256:%x","replacements":[{"old":"incorrect","new":%q}]}`, sha256.Sum256([]byte("incorrect")), content))
	}
	if patch != nil {
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "record-change", Name: "apply_patch", Arguments: patch}}); err != nil {
			return err
		}
	} else if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "完成当前文件修改"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

func TestTaskWorkflowReworksConfirmedProgramFailureWithinBudget(t *testing.T) {
	for _, test := range []struct {
		name    string
		repair  bool
		verdict contract.EvaluationVerdict
		reviews int
	}{
		{"repaired", true, contract.VerdictPassed, 1},
		{"budget_exhausted", false, contract.VerdictFailed, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			configuration := workflowExecConfig(nil)
			configuration.Verification.Commands = []config.VerificationCommand{{ID: "check", Argv: []string{"/usr/bin/cmp", "-s", "created.txt", "expected.txt"}}}
			scope := TaskScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: configuration}
			if err := os.WriteFile(filepath.Join(scope.Workspace, "expected.txt"), []byte("expected"), 0o600); err != nil {
				t.Fatal(err)
			}
			worker := &taskReworkProvider{repair: test.repair}
			reviewer := &workflowReviewFixture{output: `{"findings":[]}`}
			service := Service{NewProvider: func(cfg config.Config) (ports.Provider, error) {
				if cfg.Tools.VerificationFilesOnly {
					return reviewer, nil
				}
				return worker, nil
			}}
			current, err := service.CreateTask(ctx, scope, TaskDefinition{
				Goal: "创建与参照文件一致的产物", Criteria: []contract.AcceptanceCriterion{
					{ID: "content", Statement: "内容与参照文件相同", Required: true, Checks: []string{"check"}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.StartTaskWorkflow(ctx, scope, current.ID, TaskWorkflowOptions{Revision: current.Revision, ReworkAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			instance := view.Workflow
			if instance == nil || !instance.Finished || instance.Verification == nil || !worker.rework ||
				instance.Verification.Evaluation.Verdict != test.verdict || view.State != task.Active {
				t.Fatalf("unexpected final task or report: %+v; instance %+v", view.Task, instance)
			}
			steps := instance.Steps
			if len(steps[0].Attempts) != 2 || len(steps[1].Attempts) != 2 || len(steps[2].Attempts) != test.reviews ||
				steps[1].Attempts[0].State != workflow.StepFailed || steps[1].Attempts[0].Output.Ref == "" {
				t.Fatalf("rework history was not preserved: %+v", steps)
			}
			if !test.repair {
				_, err := service.CompleteTask(ctx, scope, current.ID, view.Revision, steps[1].Attempts[1].RunID)
				if !errors.Is(err, ErrTaskUnverified) {
					t.Fatalf("failure was accepted: %v", err)
				}
			}
			cold, err := (Service{}).ContinueTaskWorkflow(ctx, scope, current.ID, nil)
			if err != nil || cold.Workflow.Sequence != instance.Sequence {
				t.Fatalf("settled workflow replayed work: %+v, %v", cold, err)
			}
		})
	}
}
