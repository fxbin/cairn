package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

type workflowReviewFixture struct {
	output  string
	path    string
	calls   int
	request ports.ModelRequest
}

func (provider *workflowReviewFixture) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if provider.calls == 1 {
		provider.request = request
		path := provider.path
		if path == "" {
			path = "created.txt"
		}
		arguments, err := json.Marshal(struct {
			Path string `json:"path"`
		}{path})
		if err != nil {
			return err
		}
		call := &ports.ToolCall{ID: "review-read", Name: "read_file", Arguments: arguments}
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: call}); err != nil {
			return err
		}
	} else if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: provider.output}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

func TestWorkflowVerificationPersistsProgramAndIndependentReview(t *testing.T) {
	for _, tc := range []struct {
		name, command, findings string
		verdict                 contract.EvaluationVerdict
		state                   string
	}{
		{"passed", "/usr/bin/true", `{"findings":[]}`, contract.VerdictPassed, "completed"},
		{"uncorroborated", "/usr/bin/true", `{"findings":[{"id":"f-1","category":"correctness","severity":"medium","criterion_id":"build","description":"Unproven","location":"created.txt:1"}]}`, contract.VerdictUnknown, "failed"},
		{"unrelated_file", "/usr/bin/true", `{"findings":[]}`, contract.VerdictUnknown, "failed"},
		{"stale_gate", "/usr/bin/true", `{"findings":[]}`, contract.VerdictPassed, ""},
		{"program_failed", "/usr/bin/false", "", contract.VerdictFailed, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			configuration := workflowExecConfig(nil)
			configuration.Verification.Commands = []config.VerificationCommand{{ID: "check", Argv: []string{tc.command}}}
			scope := WorkflowScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: configuration}
			acceptance, err := contract.NewAcceptanceContract([]contract.AcceptanceCriterion{
				{ID: "build", Statement: "验证工作区内创建的文件", Required: true, Checks: []string{"check"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			reviewer := &workflowReviewFixture{output: tc.findings}
			if tc.name == "unrelated_file" {
				reviewer.path = "unrelated.txt"
				if err := os.WriteFile(filepath.Join(scope.Workspace, reviewer.path), []byte("unrelated"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			worker := &patchThenFinalProvider{}
			service := Service{NewProvider: func(configuration config.Config) (ports.Provider, error) {
				if configuration.Tools.VerificationFilesOnly {
					return reviewer, nil
				}
				return worker, nil
			}}
			nodes := []WorkflowNode{
				{ID: "worker", Kind: workflow.StepKindExecute, Prompt: "worker self-summary marker"},
				{ID: "program", Kind: workflow.StepKindVerify},
				{ID: "review", Kind: workflow.StepKindReview},
			}
			if tc.name == "stale_gate" {
				nodes = append(nodes, WorkflowNode{ID: "accept", Kind: workflow.StepKindAccept})
			}
			definition, err := service.SaveWorkflowDefinition(ctx, scope, WorkflowDefinitionDraft{
				Name: "verification integration", Verification: &acceptance, Nodes: nodes,
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := service.StartWorkflow(ctx, StartWorkflowOptions{Scope: scope, DefinitionID: definition.ID, Revision: definition.Revision})
			if err != nil && !errors.Is(err, ErrWorkflowFinished) {
				t.Fatalf("start: %v", err)
			}
			view, err := (Service{}).WorkflowStatus(ctx, scope.Workspace, scope.StateDir, scope.Config, workflow.ID(outcome.View.WorkflowID))
			if err != nil {
				t.Fatal(err)
			}
			report := view.Verification
			if view.FinishState != tc.state || report == nil || report.Evaluation.Verdict != tc.verdict {
				t.Fatalf("work did not produce expected terminal report: %+v, report %+v", view, report)
			}
			if tc.name == "stale_gate" {
				if view.Phase != "waiting_human" {
					t.Fatalf("expected human gate, got %s", view.Phase)
				}
				if err := os.WriteFile(filepath.Join(scope.Workspace, "created.txt"), []byte("changed after review"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := service.ApproveStep(ctx, WorkflowDecisionOptions{
					Scope: scope, WorkflowID: workflow.ID(view.WorkflowID), Sequence: view.Sequence, StepID: "accept", Reason: "inspect",
				}); !errors.Is(err, ErrVerifierTarget) {
					t.Fatalf("drifted verification was accepted: %v", err)
				}
			}
			if tc.findings != "" {
				if report.ReviewRunID == "" || report.ProgramRunID == "" || report.WorkerRunID == "" || len(reviewer.request.Tools) != 3 {
					t.Fatalf("missing isolated review: %+v, tools %+v", report, reviewer.request.Tools)
				}
				var messages strings.Builder
				for _, message := range reviewer.request.Messages {
					messages.WriteString(message.Content)
				}
				if !strings.Contains(messages.String(), "验证工作区内创建的文件") ||
					!strings.Contains(messages.String(), "created.txt") ||
					strings.Contains(messages.String(), "worker self-summary marker") {
					t.Fatalf("unexpected independent prompt: %s", messages.String())
				}
			} else if reviewer.calls != 0 || report.ReviewRunID != "" {
				t.Fatal("review ran without program evidence")
			}
		})
	}
}

func TestWorkflowReviewCannotCoverUnknownMutationAlongsidePatch(t *testing.T) {
	patch, process := domain.NewOperationID(), domain.NewOperationID()
	digest := "sha256:" + strings.Repeat("a", 64)
	worker := []domain.Event{
		{Type: domain.EventOperationStarted, OperationID: &patch, Payload: mustJSON(t, struct {
			Class string `json:"side_effect_class"`
			Path  string `json:"planned_path"`
			Hash  string `json:"planned_after"`
		}{"workspace", "changed.txt", digest})},
		{Type: domain.EventOperationSucceeded, OperationID: &patch},
	}
	review := []domain.Event{{Type: domain.EventOperationSucceeded, Payload: mustJSON(t, struct {
		Name   string              `json:"tool_name"`
		Result contract.ToolResult `json:"result"`
	}{"read_file", contract.ToolResult{OK: true, SHA256: digest, Data: json.RawMessage(`{"path":"changed.txt","offset":0}`)}})}}
	if !workflowReviewReadFiles(review, worker) {
		t.Fatal("exactly matching complete read was not credited")
	}
	worker = append(worker, domain.Event{
		Type: domain.EventOperationStarted, OperationID: &process,
		Payload: json.RawMessage(`{"tool_name":"exec_command","side_effect_class":"process","mutation_barrier":true}`),
	}, domain.Event{Type: domain.EventOperationSucceeded, OperationID: &process})
	if workflowReviewReadFiles(review, worker) {
		t.Fatal("review of one patch incorrectly covered an untracked command mutation")
	}
}
