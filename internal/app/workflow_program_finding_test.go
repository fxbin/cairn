// 本文件覆盖程序真实退出、证据缺口与制品错配。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/artifact"
)

func TestWorkflowProgramFailureFindingsRequiresBoundExit(t *testing.T) {
	execution, input, run, events := workflowFailureFindingFixture(t)
	findings, err := execution.workflowProgramFailureFindings(run, events, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want one required-criterion finding", len(findings))
	}
	finding := findings[0]
	if finding.Status != contract.FindingConfirmed || !finding.IsBlocking() ||
		finding.CriterionID != "build" || finding.TargetID != string(input.WorkerRunID) ||
		!strings.Contains(finding.Description, "check") || !strings.Contains(finding.Description, "1") ||
		len(finding.EvidenceRefs) != 1 || len(finding.Reviews) != 1 || len(finding.Reviews[0].EvidenceRefs) != 1 ||
		finding.EvidenceRefs[0] != finding.Reviews[0].EvidenceRefs[0] {
		t.Fatalf("wrong bound finding: %+v", finding)
	}
	replayed, err := execution.workflowProgramFailureFindings(run, events, input)
	if err != nil || len(replayed) != 1 || replayed[0].ID != finding.ID {
		t.Fatalf("finding identity is not stable: %+v, %v", replayed, err)
	}

	t.Run("worker_version_drift", func(t *testing.T) {
		other := input
		other.WorkspaceVersion = "wrong version"
		expectNoProgramFailureFinding(t, execution, other, run, events)
	})
	t.Run("no_start", func(t *testing.T) {
		missing := slices.Clone(events)
		start := programFindingEventIndex(t, missing, domain.EventOperationStarted)
		missing[start].Type = domain.EventApprovalRequested
		expectNoProgramFailureFinding(t, execution, input, run, missing)
	})
	t.Run("reconciliation_is_not_a_direct_result", func(t *testing.T) {
		ambiguous := slices.Clone(events)
		result := programFindingEventIndex(t, ambiguous, domain.EventOperationFailed)
		ambiguous[result].Type = domain.EventOperationReconciled
		expectNoProgramFailureFinding(t, execution, input, run, ambiguous)
	})
	t.Run("signal_and_runtime_failures", func(t *testing.T) {
		for _, status := range []struct {
			name, code, signal, stderr string
			exit                       int
		}{
			{name: "approval_denied", code: "approval_denied"},
			{name: "timed_out", code: "cancelled", exit: 1},
			{name: "signalled", code: "nonzero_exit", signal: "terminated", exit: 1},
			{name: "container_start", code: "nonzero_exit", exit: 125},
			{name: "sandbox_start", code: "nonzero_exit", exit: 1, stderr: "bwrap: cannot set up namespace"},
		} {
			t.Run(status.name, func(t *testing.T) {
				ineligible := slices.Clone(events)
				result := programFindingEventIndex(t, ineligible, domain.EventOperationFailed)
				var payload struct {
					ToolName  string              `json:"tool_name"`
					Result    contract.ToolResult `json:"result"`
					ResultRef *contract.Ref       `json:"result_ref"`
				}
				if err := json.Unmarshal(ineligible[result].Payload, &payload); err != nil {
					t.Fatal(err)
				}
				payload.Result.Code = status.code
				payload.Result.Data = mustJSON(t, struct {
					Argv     []string `json:"argv"`
					Backend  string   `json:"backend"`
					CWD      string   `json:"cwd"`
					ExitCode int      `json:"exit_code"`
					Signal   string   `json:"signal"`
					Stderr   string   `json:"stderr"`
				}{[]string{"/usr/bin/false"}, "host+seatbelt", ".", status.exit, status.signal, status.stderr})
				ineligible[result].Payload = mustJSON(t, payload)
				expectNoProgramFailureFinding(t, execution, input, run, ineligible)
			})
		}
	})
	t.Run("valid_digest_for_another_result", func(t *testing.T) {
		mismatch := slices.Clone(events)
		result := programFindingEventIndex(t, mismatch, domain.EventOperationFailed)
		var payload struct {
			ToolName  string          `json:"tool_name"`
			Result    json.RawMessage `json:"result"`
			ResultRef *contract.Ref   `json:"result_ref"`
		}
		if err := json.Unmarshal(mismatch[result].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		store := artifact.Store{Root: manifestArtifactRoot(execution.stateDir, execution.workspaceHash)}
		ref, err := store.Put([]byte(`{"OK":false,"code":"another_receipt"}`), "application/json")
		if err != nil {
			t.Fatal(err)
		}
		payload.ResultRef = &ref
		mismatch[result].Payload = mustJSON(t, payload)
		resultFindings, err := execution.workflowProgramFailureFindings(run, mismatch, input)
		if len(resultFindings) != 0 || !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("mismatched result produced %+v, %v", resultFindings, err)
		}
	})
}

// workflowFailureFindingFixture 经真实 Workflow Program 执行获得账本与制品。
func workflowFailureFindingFixture(t *testing.T) (workflowExecution, workflowVerificationInputs, domain.RunProjection, []domain.Event) {
	t.Helper()
	ctx := context.Background()
	configuration := workflowExecConfig(nil)
	configuration.Verification.Commands = []config.VerificationCommand{{ID: "check", Argv: []string{"/usr/bin/false"}}}
	scope := WorkflowScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: configuration}
	acceptance, err := contract.NewAcceptanceContract([]contract.AcceptanceCriterion{
		{ID: "build", Statement: "必需的程序检查", Required: true, Checks: []string{"check"}},
		{ID: "extra", Statement: "可选的程序检查", Checks: []string{"check"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &patchThenFinalProvider{}, nil
	}}
	definition, err := service.SaveWorkflowDefinition(ctx, scope, WorkflowDefinitionDraft{
		Name: "program finding fixture", Verification: &acceptance,
		Nodes: []WorkflowNode{
			{ID: "worker", Kind: workflow.StepKindExecute, Prompt: "create fixture"},
			{ID: "program", Kind: workflow.StepKindVerify},
			{ID: "review", Kind: workflow.StepKindReview},
		},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.StartWorkflow(ctx, StartWorkflowOptions{Scope: scope, DefinitionID: definition.ID, Revision: definition.Revision})
	if err != nil || outcome.View.FinishState != "failed" || outcome.View.Verification == nil {
		t.Fatalf("program fixture did not settle: %+v, %v", outcome, err)
	}
	report := outcome.View.Verification
	object, ok := report.Scope.Object(workflowProgramCoverageID)
	if !ok {
		t.Fatal("missing program subject")
	}
	execution := workflowExecution{stateDir: scope.StateDir, workspaceHash: outcome.View.WorkspaceHash}
	journal, err := openReadOnlyJournalContext(ctx, scope.StateDir, outcome.View.WorkspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	events, err := journal.Load(ctx, report.ProgramRunID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	return execution, workflowVerificationInputs{
		Acceptance: acceptance, WorkerRunID: report.WorkerRunID, ProgramRunID: report.ProgramRunID,
		WorkspaceVersion: object.Version,
	}, run, events
}

func expectNoProgramFailureFinding(t *testing.T, execution workflowExecution, input workflowVerificationInputs, run domain.RunProjection, events []domain.Event) {
	t.Helper()
	findings, err := execution.workflowProgramFailureFindings(run, events, input)
	if err != nil || len(findings) != 0 {
		t.Fatalf("ineligible record produced %+v, %v", findings, err)
	}
}

func programFindingEventIndex(t *testing.T, events []domain.Event, kind domain.EventType) int {
	t.Helper()
	for index, event := range events {
		if event.Type == kind {
			return index
		}
	}
	t.Fatalf("missing fixture event %s", kind)
	return -1
}
