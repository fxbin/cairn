package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// TestResumeZeroMutationVerificationStages 核对零变更验证各持久化中断点：
// 未启动命令可按授权链继续，已启动命令须对账，已成功命令不得重跑。
func TestResumeZeroMutationVerificationStages(t *testing.T) {
	cases := []struct {
		name               string
		stage              int
		ask                bool
		drift              bool
		directRepair       bool
		mutated            bool
		missingObservation bool
		volatile           bool
		wantCalls          int
		wantError          string
		wantOK             bool
	}{
		{name: "proposed", stage: 1, wantCalls: 1, wantOK: true},
		{name: "authorized", stage: 2, wantCalls: 1, wantOK: true},
		{name: "manually approved", stage: 2, ask: true, wantCalls: 1, wantOK: true},
		{name: "started unresolved", stage: 3, wantError: "dangling side-effect", wantOK: false},
		{name: "successful terminal", stage: 4, wantCalls: 0, wantOK: true},
		{name: "drift after success", stage: 4, drift: true, wantError: "workspace changed since verification started", wantOK: false},
		{name: "failed terminal closes after drift", stage: 5, drift: true, directRepair: true, wantCalls: 0, wantOK: false},
		{name: "finished stable", stage: 6, wantOK: true},
		{name: "finished drift", stage: 6, drift: true, wantError: "workspace changed since verification started", wantOK: true},
		{name: "closed final stable", stage: 7, wantOK: true},
		{name: "closed final drift", stage: 7, drift: true, wantError: "workspace changed since verification started", wantOK: true},
		{name: "finished missing observation", stage: 6, missingObservation: true, wantError: "no persisted workspace observation", wantOK: true},
		{name: "closed final missing observation", stage: 7, missingObservation: true, wantError: "no persisted workspace observation", wantOK: true},
		{name: "finished volatile drift", stage: 6, drift: true, volatile: true, wantOK: true},
		{name: "mutated finished drift", stage: 6, mutated: true, drift: true, wantError: "workspace changed since verification started", wantOK: true},
		{name: "mutated closed final drift", stage: 7, mutated: true, drift: true, wantError: "workspace changed since verification started", wantOK: true},
		{name: "mutated finished stable", stage: 6, mutated: true, wantOK: true},
		{name: "mutated closed final stable", stage: 7, mutated: true, wantOK: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
			if err != nil {
				t.Fatal(err)
			}
			execMode := "allow"
			if test.ask {
				execMode = "ask"
			}
			gate, err := policy.New("allow", execMode, nil, tools.ApplyPatch{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			verificationTool := &countingExecTool{}
			registry, err := tools.NewRegistry(verificationTool)
			if err != nil {
				t.Fatal(err)
			}
			commands := []VerificationCommand{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}
			fingerprint, err := FingerprintVerificationCommands(commands)
			if err != nil {
				t.Fatal(err)
			}
			verificationWorkspace := workspace
			if test.volatile {
				verificationWorkspace = workspace.ObservationView([]string{"drift.txt"})
			}
			observation, err := verificationWorkspace.Observation()
			if err != nil {
				t.Fatal(err)
			}
			journal := memory.New()
			runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
			var seq uint64
			appendEvent := func(draft domain.EventDraft) {
				t.Helper()
				if _, err := journal.Append(context.Background(), seq, draft); err != nil {
					t.Fatal(err)
				}
				seq++
			}
			var mutationSeq uint64
			if test.mutated {
				priorTurn, priorOperation := domain.NewTurnID(), domain.NewOperationID()
				for _, draft := range []domain.EventDraft{
					{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"resume verify","workspace":"/workspace"}`)},
					{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted},
					{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &priorTurn, Payload: json.RawMessage(`{"turn":1}`)},
					{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolCallProposed, TurnID: &priorTurn, OperationID: &priorOperation, Payload: mustJSON(toolCallPayload{CallID: "patch", ToolName: "apply_patch", Arguments: json.RawMessage(`{"path":"initial.txt","mode":"create","content":"initial"}`)})},
					policyDecisionDraft(RunInput{RunID: runID, WorkspaceHash: "workspace"}, priorTurn, priorOperation, policy.Decision{Effect: policy.Allow, RuleID: "write_allowed", Fingerprint: fingerprint}),
					{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &priorTurn, OperationID: &priorOperation, Payload: mustJSON(operationPayload{ToolName: "apply_patch", SideEffectClass: "workspace"})},
					{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationSucceeded, TurnID: &priorTurn, OperationID: &priorOperation, Payload: mustJSON(operationPayload{ToolName: "apply_patch", Result: tools.Success(struct{}{}, "patched", 0, "", false)})},
					{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolResultAppended, TurnID: &priorTurn, OperationID: &priorOperation, Payload: mustJSON(tools.Success(struct{}{}, "patched", 0, "", false))},
					{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &priorTurn, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})},
				} {
					appendEvent(draft)
				}
				priorEvents, err := journal.Load(context.Background(), runID)
				if err != nil {
					t.Fatal(err)
				}
				priorProjection, err := domain.Replay(priorEvents)
				if err != nil {
					t.Fatal(err)
				}
				mutationSeq = priorProjection.LastMutationSeq
			}
			persistedObservation := observation
			if test.missingObservation {
				persistedObservation = ""
			}
			base := []domain.EventDraft{
				{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"resume verify","workspace":"/workspace"}`)},
				{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted},
				{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID, Payload: json.RawMessage(`{"turn":1}`)},
				{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &turnID, Payload: json.RawMessage(`{"provider":"openai_compatible","model":"fake"}`)},
				{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelResponseFinished, TurnID: &turnID, Payload: json.RawMessage(`{"text":"done","tool_calls":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"stop_reason":"stop"}`)},
				{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventAssistantMessageAppended, TurnID: &turnID, Payload: json.RawMessage(`{"text":"done"}`)},
				{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: mustJSON(struct {
					VerificationID       string `json:"verification_id"`
					LastMutationSeq      uint64 `json:"last_mutation_seq"`
					CommandsFingerprint  string `json:"commands_fingerprint"`
					WorkspaceObservation string `json:"workspace_observation,omitempty"`
				}{VerificationID: "verify-1", LastMutationSeq: mutationSeq, CommandsFingerprint: fingerprint, WorkspaceObservation: persistedObservation})},
			}
			if test.mutated {
				base = base[2:]
			}
			for _, draft := range base {
				appendEvent(draft)
			}
			if test.stage >= 1 {
				appendEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventVerificationCommandProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(verificationProposalPayload{VerificationID: "verify-1", CommandIndex: 0, Arguments: commands[0].Arguments})})
			}
			if test.stage >= 2 {
				decision, err := gate.DecideVerificationWithObservation(commands[0].Arguments, "", int(mutationSeq), "", observation)
				if err != nil {
					t.Fatal(err)
				}
				appendEvent(policyDecisionDraft(RunInput{RunID: runID, WorkspaceHash: "workspace"}, turnID, operationID, decision))
				if test.ask {
					appendEvent(approvalRequestedDraft(RunInput{RunID: runID, WorkspaceHash: "workspace"}, turnID, operationID, decision))
					appendEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventApprovalGranted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
						Fingerprint string `json:"fingerprint"`
					}{Fingerprint: decision.Fingerprint})})
					approved := policy.Decision{Effect: policy.Allow, RuleID: "approved_once", Fingerprint: decision.Fingerprint}
					appendEvent(policyDecisionDraft(RunInput{RunID: runID, WorkspaceHash: "workspace"}, turnID, operationID, approved))
				}
			}
			if test.stage >= 3 {
				appendEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: "exec_command", SideEffectClass: string(tools.SideEffectProcess)})})
			}
			if test.stage == 4 || test.stage >= 6 {
				appendEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationSucceeded, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: "exec_command", Result: tools.Success(struct{}{}, "verified", 0, "", false)})})
			}
			if test.stage == 5 {
				appendEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: "exec_command", Result: tools.Failure("verification_failed", "failed")})})
			}
			if test.stage >= 6 {
				appendEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(tools.Success(struct{}{}, "verified", 0, "", false))})
				appendEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{VerificationID: "verify-1", OK: true})})
			}
			if test.stage == 7 {
				appendEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})})
			}
			if test.drift {
				if err := os.WriteFile(filepath.Join(root, "drift.txt"), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			loop := Loop{Journal: journal, Provider: &scriptedProvider{}, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 6, Verification: commands, VerificationWorkspace: verificationWorkspace}
			if test.directRepair {
				events, loadErr := journal.Load(context.Background(), runID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				projection, replayErr := domain.Replay(events)
				if replayErr != nil {
					t.Fatal(replayErr)
				}
				var repaired bool
				repaired, err = loop.repairIncompleteVerification(context.Background(), &projection, RunInput{RunID: runID, WorkspaceHash: "workspace"}, events)
				if !repaired {
					t.Fatal("failed verification terminal must be closed without rerunning")
				}
			} else {
				_, err = loop.Resume(context.Background(), RunInput{RunID: runID, Task: "resume verify", WorkspaceHash: "workspace"}, ports.Approval{})
			}
			if test.wantError == "" && err != nil {
				t.Fatalf("resume: %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("resume error = %v, want %q", err, test.wantError)
			}
			if verificationTool.calls != test.wantCalls {
				t.Fatalf("verification calls = %d, want %d", verificationTool.calls, test.wantCalls)
			}
			events, err := journal.Load(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			projection, replayErr := domain.Replay(events)
			if replayErr != nil {
				t.Fatal(replayErr)
			}
			if test.wantError != "" && projection.State == domain.RunCompleted {
				t.Fatal("unsafe recovery reached completed")
			}
			verificationOK := false
			for _, event := range events {
				if event.Type != domain.EventVerificationFinished {
					continue
				}
				var payload domain.VerificationFinishedPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				verificationOK = payload.OK
			}
			if verificationOK != test.wantOK {
				t.Fatalf("verification finished OK = %v, want %v", verificationOK, test.wantOK)
			}
		})
	}
}
