package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

func TestRepeatedOperationFailureGuardRequiresThreeConsecutiveMatchingFailures(t *testing.T) {
	events := operationOutcomeEvents("same", domain.EventOperationFailed, "same", domain.EventOperationFailed)
	if reason := repeatedOperationFailureGuard(events); reason != "" {
		t.Fatalf("guard fired after two failures: %s", reason)
	}
	events = append(events, operationOutcomeEvents("same", domain.EventOperationFailed)...)
	if reason := repeatedOperationFailureGuard(events); reason != loopGuardRepeatedOperationFailure {
		t.Fatalf("guard reason = %q", reason)
	}
	events = append(events, operationOutcomeEvents("different", domain.EventOperationSucceeded)...)
	if reason := repeatedOperationFailureGuard(events); reason != "" {
		t.Fatalf("successful operation did not reset guard: %s", reason)
	}
}

func TestTurnLoopGuardDetectsABCycleBeforeNoProgress(t *testing.T) {
	signatures := []string{"A", "B", "A", "B", "A", "B"}
	events := make([]domain.Event, 0, len(signatures))
	for index, signature := range signatures {
		events = append(events, domain.Event{Type: domain.EventTurnClosed, Payload: mustJSON(domain.TurnClosedPayload{
			Outcome:             "continue",
			OperationSignature:  signature,
			ProgressFingerprint: "unchanged",
		}), Seq: uint64(index + 1)})
	}
	if reason := turnLoopGuard(events); reason != loopGuardPeriodicTurnCycle {
		t.Fatalf("guard reason = %q", reason)
	}
}

func TestTurnLoopGuardDetectsSixRoundsWithoutProgress(t *testing.T) {
	events := make([]domain.Event, 0, noProgressTurnLimit)
	for index := 0; index < noProgressTurnLimit; index++ {
		events = append(events, domain.Event{Type: domain.EventTurnClosed, Payload: mustJSON(domain.TurnClosedPayload{
			Outcome:             "continue",
			OperationSignature:  "same-operation",
			ProgressFingerprint: "unchanged",
		}), Seq: uint64(index + 1)})
	}
	if reason := turnLoopGuard(events); reason != loopGuardNoProgress {
		t.Fatalf("guard reason = %q", reason)
	}
}

func TestStableToolResultFingerprintIgnoresCommandDuration(t *testing.T) {
	first := tools.Result{Code: "nonzero_exit", Preview: `{"duration_ms":1,"stderr":"failed"}`}
	second := tools.Result{Code: "nonzero_exit", Preview: `{"duration_ms":999,"stderr":"failed"}`}
	if stableToolResultFingerprint(first) != stableToolResultFingerprint(second) {
		t.Fatal("command duration changed verifier state fingerprint")
	}
}

func TestRunTurnBudgetCountsPreviouslyClosedTurns(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID, turnID := domain.NewRunID(), domain.NewTurnID()
	appendInitialRunningEvents(t, journal, runID, "respect resumed turn budget")
	for offset, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})},
	} {
		if _, err := journal.Append(context.Background(), uint64(offset+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "must not run"}, {Kind: ports.Done}}}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 1}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "respect resumed turn budget", WorkspaceHash: "workspace"})
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != "turns" || len(provider.requests) != 0 {
		t.Fatalf("error=%v requests=%d", err, len(provider.requests))
	}
}

func TestLoopStopsAfterThreeMatchingFailedOperations(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	scripts := make([][]ports.ModelEvent, consecutiveOperationFailureLimit)
	for index := range scripts {
		scripts[index] = []ports.ModelEvent{
			{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "unknown-" + string(rune('1'+index)), Name: "unknown", Arguments: json.RawMessage(`{"path":"."}`)}},
			{Kind: ports.Done},
		}
	}
	provider := &scriptedProvider{scripts: scripts}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "stop repeated failures")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 10}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "stop repeated failures", WorkspaceHash: "workspace"})
	var guarded *LoopGuardExceededError
	if !errors.As(err, &guarded) || guarded.Reason != loopGuardRepeatedOperationFailure || len(provider.requests) != consecutiveOperationFailureLimit {
		t.Fatalf("error=%v requests=%d", err, len(provider.requests))
	}
	events, loadErr := journal.Load(context.Background(), runID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !containsEventPayload(events, domain.EventRunFailed, `"error_class":"loop_guard"`) {
		t.Fatalf("loop guard terminal evidence missing: %#v", events)
	}
}

func operationOutcomeEvents(fingerprint string, outcome domain.EventType, rest ...any) []domain.Event {
	pairs := append([]any{fingerprint, outcome}, rest...)
	events := make([]domain.Event, 0, len(pairs))
	for index := 0; index < len(pairs); index += 2 {
		operationID := domain.OperationID("operation-" + string(rune('1'+index/2)))
		events = append(events,
			domain.Event{Type: domain.EventPolicyDecided, OperationID: &operationID, Payload: mustJSON(struct {
				Fingerprint string `json:"fingerprint"`
			}{Fingerprint: pairs[index].(string)})},
			domain.Event{Type: pairs[index+1].(domain.EventType), OperationID: &operationID},
		)
	}
	return events
}

// TestProgressFingerprintTracksDistinctToolEvidence 锁定读证据语义：首次出现的
// 读/检索调用改变指纹（长读收敛不被误杀），重复相同调用保持指纹稳定（原地
// 徘徊仍会被熔断），副作用工具调用不参与读证据。
func TestProgressFingerprintTracksDistinctToolEvidence(t *testing.T) {
	build := func(calls ...[2]string) []domain.Event {
		events := make([]domain.Event, 0, len(calls)+1)
		for index, call := range calls {
			events = append(events, domain.Event{Type: domain.EventToolCallProposed, Payload: mustJSON(toolCallPayload{
				CallID: fmt.Sprintf("call-%d", index), ToolName: call[0], Arguments: json.RawMessage(call[1]),
			}), Seq: uint64(index + 1)})
		}
		return events
	}
	first := progressFingerprint("ws", build([2]string{"read_file", `{"path":"a.rs"}`}))
	repeat := progressFingerprint("ws", build([2]string{"read_file", `{"path":"a.rs"}`}, [2]string{"read_file", `{"path":"a.rs"}`}))
	if first != repeat {
		t.Fatal("identical read calls must not change the fingerprint")
	}
	grown := progressFingerprint("ws", build([2]string{"read_file", `{"path":"a.rs"}`}, [2]string{"search_text", `{"query":"retry"}`}))
	if first == grown {
		t.Fatal("a first-time read/search call must change the fingerprint")
	}
	withExec := progressFingerprint("ws", build([2]string{"read_file", `{"path":"a.rs"}`}, [2]string{"exec_command", `{"argv":["grep","-n","retry","src/"]}`}))
	if first == withExec {
		t.Fatal("a first-time exec exploration call must change the fingerprint (exec evidence parity)")
	}
	repeatExec := progressFingerprint("ws", build([2]string{"exec_command", `{"argv":["grep","-n","retry","src/"]}`}, [2]string{"exec_command", `{"argv":["grep","-n","retry","src/"]}`}))
	if progressFingerprint("ws", build([2]string{"exec_command", `{"argv":["grep","-n","retry","src/"]}`})) != repeatExec {
		t.Fatal("identical exec calls must not change the fingerprint")
	}
}
