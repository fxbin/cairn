package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

type scriptedProvider struct {
	scripts  [][]ports.ModelEvent
	errors   []error
	requests []ports.ModelRequest
}

type cancellingProvider struct{ cancel context.CancelFunc }

type cancellingExecTool struct{ cancel context.CancelFunc }

type uncertainExecTool struct{}

type flakyVerificationTool struct{ calls int }

type countingExecTool struct{ calls int }

type mutatingVerificationTool struct{ path string }

func (uncertainExecTool) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "exec_command"}
}

func (uncertainExecTool) SideEffect() tools.SideEffectClass { return tools.SideEffectProcess }

func (uncertainExecTool) Execute(_ context.Context, _ json.RawMessage) tools.Result {
	return tools.Failure(tools.CodeTerminationUnconfirmed, "process termination is unconfirmed")
}

func (tool mutatingVerificationTool) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "exec_command"}
}

func (tool mutatingVerificationTool) SideEffect() tools.SideEffectClass {
	return tools.SideEffectProcess
}

func (tool mutatingVerificationTool) Execute(_ context.Context, _ json.RawMessage) tools.Result {
	if err := os.WriteFile(tool.path, []byte("mutated by verification"), 0o600); err != nil {
		return tools.Failure("write_failed", err.Error())
	}
	return tools.Success(struct{}{}, "verification passed", 0, "", false)
}

func (tool *countingExecTool) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "exec_command"}
}

func (tool *countingExecTool) SideEffect() tools.SideEffectClass { return tools.SideEffectProcess }

func (tool *countingExecTool) Execute(_ context.Context, _ json.RawMessage) tools.Result {
	tool.calls++
	return tools.Success(struct{}{}, "executed", 0, "", false)
}

func (tool *flakyVerificationTool) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "exec_command"}
}

func (tool *flakyVerificationTool) SideEffect() tools.SideEffectClass { return tools.SideEffectProcess }

func (tool *flakyVerificationTool) Execute(_ context.Context, _ json.RawMessage) tools.Result {
	tool.calls++
	if tool.calls == 1 {
		return tools.Failure("nonzero_exit", "test failure")
	}
	return tools.Success(struct{}{}, "test passed", 0, "", false)
}

func (tool cancellingExecTool) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "exec_command"}
}

func (tool cancellingExecTool) SideEffect() tools.SideEffectClass { return tools.SideEffectProcess }

func (tool cancellingExecTool) Execute(ctx context.Context, _ json.RawMessage) tools.Result {
	tool.cancel()
	return tools.Failure("cancelled", ctx.Err().Error())
}

func (provider cancellingProvider) Stream(ctx context.Context, _ ports.ModelRequest, _ func(ports.ModelEvent) error) error {
	provider.cancel()
	return ctx.Err()
}

func (provider *scriptedProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.requests = append(provider.requests, request)
	index := len(provider.requests) - 1
	if index < len(provider.errors) && provider.errors[index] != nil {
		return provider.errors[index]
	}
	for _, event := range provider.scripts[index] {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func TestLoopRecoversOnceFromMalformedToolArgumentsWithoutOperation(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{
		scripts: [][]ports.ModelEvent{
			nil,
			{{Kind: ports.TextDelta, Text: "corrected"}, {Kind: ports.Done}},
		},
		errors: []error{ports.ErrMalformedToolCallArguments},
	}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "recover malformed arguments")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	result, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "recover malformed arguments", WorkspaceHash: "workspace"})
	if err != nil || result.Response != "corrected" {
		t.Fatalf("run=%#v err=%v", result, err)
	}
	if len(provider.requests) != 2 || !hasUserMessage(provider.requests[1].Messages, malformedToolArgumentsFeedback) {
		t.Fatalf("repair feedback was not sent exactly after malformed call: %#v", provider.requests)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, domain.EventToolCallProposed) != 0 || countEvent(events, domain.EventPolicyDecided) != 0 || countEvent(events, domain.EventOperationStarted) != 0 {
		t.Fatalf("malformed arguments reached the tool path: %#v", events)
	}
	if malformedToolArgumentsFailureCount(events) != 1 || !containsEventPayload(events, domain.EventModelRequestFailed, `"retryable":true`) {
		t.Fatalf("first malformed failure was not persisted as recoverable: %#v", events)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
}

func TestLoopRejectsMalformedToolCallEventWithoutRecordingArguments(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "bad", Name: "list_files", Arguments: json.RawMessage(`{"token":"sk-not-for-journal"`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "corrected"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "reject raw malformed arguments")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "reject raw malformed arguments", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if containsEventPayload(events, domain.EventModelRequestFailed, "sk-not-for-journal") || countEvent(events, domain.EventToolCallProposed) != 0 || countEvent(events, domain.EventOperationStarted) != 0 {
		t.Fatalf("raw malformed arguments leaked into durable or executable paths: %#v", events)
	}
}

func TestLoopRejectsExpandedCredentialBeforeToolProposal(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ExecCommand{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "credential-sentinel-value"
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "secret-env", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["/usr/bin/true"],"env":{"GITHUB_PAT":"` + sentinel + `"}}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "reject expanded credential")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "reject expanded credential", WorkspaceHash: "workspace"}); err == nil {
		t.Fatal("credential-bearing tool call unexpectedly succeeded")
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if containsEventPayload(events, domain.EventToolCallProposed, sentinel) || countEvent(events, domain.EventToolCallProposed) != 0 || countEvent(events, domain.EventOperationStarted) != 0 {
		t.Fatalf("credential reached the durable tool path: %#v", events)
	}
}

func TestLoopFailsOnSecondMalformedToolArgumentsWithoutOperation(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{nil, nil}, errors: []error{ports.ErrMalformedToolCallArguments, ports.ErrMalformedToolCallArguments}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "stop malformed arguments")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "stop malformed arguments", WorkspaceHash: "workspace"})
	if !errors.Is(err, ports.ErrMalformedToolCallArguments) || len(provider.requests) != 2 {
		t.Fatalf("error=%v requests=%d", err, len(provider.requests))
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if malformedToolArgumentsFailureCount(events) != 2 || countEvent(events, domain.EventRunFailed) != 1 || countEvent(events, domain.EventToolCallProposed) != 0 || countEvent(events, domain.EventOperationStarted) != 0 {
		t.Fatalf("second malformed failure did not terminate safely: %#v", events)
	}
	if !containsEventPayload(events, domain.EventRunFailed, malformedToolArgumentsErrorClass) || !containsEventPayload(events, domain.EventModelRequestFailed, `"retryable":false`) {
		t.Fatalf("second malformed failure was not classified: %#v", events)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunFailed {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
}

func TestResumeRestoresMalformedToolArgumentsFeedback(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID, turnID, attemptID := domain.NewRunID(), domain.NewTurnID(), domain.NewAttemptID()
	appendInitialRunningEvents(t, journal, runID, "resume malformed arguments")
	for offset, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &turnID, AttemptID: &attemptID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelRequestFailedPayload{Error: ports.ErrMalformedToolCallArguments.Error(), ErrorClass: malformedToolArgumentsErrorClass, Retryable: true})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})},
	} {
		if _, err := journal.Append(context.Background(), uint64(offset+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "recovered"}, {Kind: ports.Done}}}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	result, err := loop.Resume(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, Approval{})
	if err != nil || result.Response != "recovered" || len(provider.requests) != 1 || !hasUserMessage(provider.requests[0].Messages, malformedToolArgumentsFeedback) {
		t.Fatalf("resume=%#v err=%v requests=%#v", result, err, provider.requests)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
}

func TestResumeFailsOnMalformedToolArgumentsAfterCorrectionWasUsed(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID, turnID, attemptID := domain.NewRunID(), domain.NewTurnID(), domain.NewAttemptID()
	appendInitialRunningEvents(t, journal, runID, "resume second malformed arguments")
	for offset, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &turnID, AttemptID: &attemptID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelRequestFailedPayload{Error: ports.ErrMalformedToolCallArguments.Error(), ErrorClass: malformedToolArgumentsErrorClass, Retryable: true})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})},
	} {
		if _, err := journal.Append(context.Background(), uint64(offset+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{nil}, errors: []error{ports.ErrMalformedToolCallArguments}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	_, err = loop.Resume(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, Approval{})
	if !errors.Is(err, ports.ErrMalformedToolCallArguments) || len(provider.requests) != 1 {
		t.Fatalf("resume error=%v requests=%d", err, len(provider.requests))
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if malformedToolArgumentsFailureCount(events) != 2 || countEvent(events, domain.EventRunFailed) != 1 {
		t.Fatalf("resume gave another correction chance: %#v", events)
	}
}

func TestMessagesFromEventsPlacesMalformedToolArgumentsFeedbackInHistoricalOrder(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID, firstTurnID, firstAttemptID := domain.NewRunID(), domain.NewTurnID(), domain.NewAttemptID()
	secondTurnID, secondAttemptID, operationID := domain.NewTurnID(), domain.NewAttemptID(), domain.NewOperationID()
	call := ports.ToolCall{ID: "read", Name: "list_files", Arguments: json.RawMessage(`{"path":"."}`)}
	appendInitialRunningEvents(t, journal, runID, "ordered feedback")
	for offset, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &firstTurnID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &firstTurnID, AttemptID: &firstAttemptID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestFailed, TurnID: &firstTurnID, AttemptID: &firstAttemptID, Payload: mustJSON(modelRequestFailedPayload{Error: ports.ErrMalformedToolCallArguments.Error(), ErrorClass: malformedToolArgumentsErrorClass, Retryable: true})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &firstTurnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &secondTurnID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &secondTurnID, AttemptID: &secondAttemptID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelResponseFinished, TurnID: &secondTurnID, AttemptID: &secondAttemptID, Payload: mustJSON(modelResponsePayload{Text: "after repair", ToolCalls: []ports.ToolCall{call}})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolCallProposed, TurnID: &secondTurnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: call.ID, ToolName: call.Name, Arguments: call.Arguments})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventPolicyDecided, TurnID: &secondTurnID, OperationID: &operationID, Payload: mustJSON(struct {
			Effect string `json:"effect"`
		}{Effect: "allow"})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &secondTurnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: call.Name})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationSucceeded, TurnID: &secondTurnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: tools.Success(map[string]any{}, "listed", 0, "", false)})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolResultAppended, TurnID: &secondTurnID, OperationID: &operationID, Payload: mustJSON(tools.Success(map[string]any{}, "listed", 0, "", false))},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &secondTurnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})},
	} {
		if _, err := journal.Append(context.Background(), uint64(offset+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	loop := Loop{Tools: registry}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	messages := loop.messagesFromEvents(events, "fallback")
	if len(messages) != 5 || messages[2].Role != "user" || messages[2].Content != malformedToolArgumentsFeedback || messages[3].Role != "assistant" || messages[3].Content != "after repair" || messages[4].Role != "tool" {
		t.Fatalf("messages=%#v", messages)
	}
}

func hasUserMessage(messages []ports.Message, expected string) bool {
	for _, message := range messages {
		if message.Role == "user" && message.Content == expected {
			return true
		}
	}
	return false
}

func TestLoopPersistsReadOnlyToolExecutionAndFinalResponse(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte("Cairn is event sourced.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "call_read", Name: "read_file", Arguments: json.RawMessage(`{"path":"answer.txt"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "The repository uses an event-sourced journal."}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	ctx := context.Background()
	_, err = journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"describe","workspace":"/workspace"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted})
	if err != nil {
		t.Fatal(err)
	}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	result, err := loop.Run(ctx, RunInput{RunID: runID, Task: "describe", WorkspaceHash: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "The repository uses an event-sourced journal." {
		t.Fatalf("response = %q", result.Response)
	}
	if len(provider.requests) != 2 || len(provider.requests[1].Messages) != 4 {
		t.Fatalf("provider requests = %#v", provider.requests)
	}
	events, err := journal.Load(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted {
		t.Fatalf("projection = %#v, %v", projection, err)
	}
}

func TestLoopKeepsReplayableJournalWhenResultArtifactStorageFails(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte(strings.Repeat("Cairn is event sourced. ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(t.TempDir(), "artifacts"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	store.MaxBytes = 1024
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "call_read", Name: "read_file", Arguments: json.RawMessage(`{"path":"answer.txt"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "artifact fallback is durable"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "persist fallback")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Artifacts: store, Model: "fake", MaxTurns: 3}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "persist fallback", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted {
		t.Fatalf("projection = %#v, %v", projection, err)
	}
	for _, event := range events {
		if event.Type != domain.EventOperationSucceeded {
			continue
		}
		var payload struct {
			ResultRef          *artifact.Ref `json:"result_ref"`
			ResultStorageError string        `json:"result_storage_error"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ResultRef != nil || payload.ResultStorageError == "" {
			t.Fatalf("artifact fallback was not recorded: %s", event.Payload)
		}
		return
	}
	t.Fatal("missing successful read operation")
}

func TestLoopRejectsInvalidTurnCloseBeforeJournalWrite(t *testing.T) {
	journal := memory.New()
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	drafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: mustJSON(domain.RunCreatedPayload{Task: "reject invalid close", Workspace: "/workspace"})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: "read", ToolName: "read_file", Arguments: json.RawMessage(`{"path":"answer.txt"}`)})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
			Effect string `json:"effect"`
		}{Effect: "allow"})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: "read_file", SideEffectClass: string(tools.SideEffectNone)})},
	}
	for expected, draft := range drafts {
		if _, err := journal.Append(context.Background(), uint64(expected), draft); err != nil {
			t.Fatal(err)
		}
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	loop := Loop{Journal: journal}
	err = loop.append(context.Background(), &projection, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})})
	if err == nil || !strings.Contains(err.Error(), "turn_closed") {
		t.Fatalf("invalid turn close error = %v", err)
	}
	events, err = journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(drafts) {
		t.Fatalf("invalid turn close was persisted: %d events", len(events))
	}
}

func TestLoopCompactsLargeToolOutputBeforeFollowingModelRequest(t *testing.T) {
	root := t.TempDir()
	marker := "FULL_TOOL_BODY_MUST_NOT_REACH_THE_NEXT_REQUEST"
	content := strings.Repeat("padding-", 8_000) + marker
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 128*1024, 100, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "call_large", Name: "read_file", Arguments: json.RawMessage(`{"path":"large.txt"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "large file inspected"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	ctx := context.Background()
	if _, err := journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"inspect large file","workspace":"/workspace"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	loop := Loop{
		Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2,
		ContextAutoCompact: true, ContextMaxPromptTokens: 1_500, ContextMaxSummaryBytes: 1_024,
	}
	result, err := loop.Run(ctx, RunInput{RunID: runID, Task: "inspect large file", WorkspaceHash: "workspace"})
	if err != nil || result.Response != "large file inspected" {
		t.Fatalf("run = %#v, %v", result, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("model requests = %d", len(provider.requests))
	}
	secondRequest := provider.requests[1].Messages
	if len(secondRequest) != 3 || !strings.HasPrefix(secondRequest[2].Content, contextCheckpointPrefix) {
		t.Fatalf("second request did not use a checkpoint: %#v", secondRequest)
	}
	if strings.Contains(secondRequest[2].Content, marker) {
		t.Fatalf("checkpoint copied raw tool body: %q", secondRequest[2].Content)
	}
	events, err := journal.Load(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint domain.ContextCompactedPayload
	seenCheckpoint := false
	for _, event := range events {
		if event.Type != domain.EventContextCompacted {
			continue
		}
		if err := json.Unmarshal(event.Payload, &checkpoint); err != nil {
			t.Fatal(err)
		}
		seenCheckpoint = true
	}
	if !seenCheckpoint || checkpoint.ThroughSeq == 0 || checkpoint.EstimatedTokensAfter >= checkpoint.EstimatedTokensBefore || strings.Contains(checkpoint.Summary, marker) {
		t.Fatalf("invalid checkpoint = %#v", checkpoint)
	}
	rebuilt := loop.messagesFromEvents(events, "fallback")
	if len(rebuilt) != 3 || rebuilt[2].Content != secondRequest[2].Content {
		t.Fatalf("replayed context = %#v, want %#v", rebuilt, secondRequest)
	}
}

func TestLoopCompactsModerateSourceOutputBeforeBudgetDrift(t *testing.T) {
	root := t.TempDir()
	marker := "SOURCE_OUTPUT_MUST_BE_CHECKPOINTED"
	content := strings.Repeat("const item = value;\n", 90) + marker
	if err := os.WriteFile(filepath.Join(root, "source.js"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 16*1024, 100, 32*1024)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "call_source", Name: "read_file", Arguments: json.RawMessage(`{"path":"source.js"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "source inspected"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "inspect source output")
	loop := Loop{
		Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2,
		ContextAutoCompact: true, ContextMaxPromptTokens: 1_500, ContextMaxSummaryBytes: 1_024,
	}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "inspect source output", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 || len(provider.requests[1].Messages) != 3 || !strings.HasPrefix(provider.requests[1].Messages[2].Content, contextCheckpointPrefix) {
		t.Fatalf("second request did not use cautious checkpointing: %#v", provider.requests)
	}
	if strings.Contains(provider.requests[1].Messages[2].Content, marker) {
		t.Fatalf("checkpoint copied raw source body: %q", provider.requests[1].Messages[2].Content)
	}
}

func TestContextCompactionThresholdReservesPromptHeadroom(t *testing.T) {
	loop := Loop{ContextMaxPromptTokens: 50_000}
	if got := loop.contextCompactionThreshold(); got != 40_000 {
		t.Fatalf("threshold = %d, want 40000", got)
	}
	loop.ContextMaxPromptTokens = 1
	if got := loop.contextCompactionThreshold(); got != 1 {
		t.Fatalf("minimum threshold = %d, want 1", got)
	}
}

func TestDefaultPromptExplainsDeterministicPatchBinding(t *testing.T) {
	loop := Loop{}
	prompt := loop.prompt()
	for _, required := range []string{"read_file", "expected_sha256", "shell hash", "byte offset", "unchanged failed command", "unrequested tests"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("default prompt omitted %q: %s", required, prompt)
		}
	}
}

// TestSummarizeMessagesRetainsExplorationInventoryUnderBudgetPressure 锁定
// 探索记忆合同：检查点必须保留去重的已检查对象清单，且在近期事实预算被
// 挤压到截断时清单依然完整——这是压缩后"不重复探索"行为的机器可核对依据
// （AgentScope 失败 run 实证：v1 检查点丢探索记忆导致 read 爆炸循环）。
func TestSummarizeMessagesRetainsExplorationInventoryUnderBudgetPressure(t *testing.T) {
	result := func(body string) string {
		encoded, err := json.Marshal(tools.Success(map[string]any{"preview": body}, body, int64(len(body)), "", false))
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	messages := []ports.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"src/agentscope/agent/_agent.py"}`)}}},
		{Role: "tool", ToolCallID: "c1", Content: result("thinking-only branch at line 214")},
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "c2", Name: "search_text", Arguments: json.RawMessage(`{"query":"thinking_only_response"}`)}}},
		{Role: "tool", ToolCallID: "c2", Content: result("three call sites")},
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "c3", Name: "read_file", Arguments: json.RawMessage(`{"path":"src/agentscope/agent/_agent.py"}`)}}},
		{Role: "tool", ToolCallID: "c3", Content: result("second read of the same file")},
		{Role: "assistant", Content: "已定位 ReAct 循环的纯思考分支处理位置，下一步在 _agent.py 修改循环继续条件。"},
	}
	summary := summarizeMessages(messages, 260)
	if !strings.Contains(summary, "已检查对象") {
		t.Fatalf("summary missing exploration inventory: %q", summary)
	}
	for _, required := range []string{"src/agentscope/agent/_agent.py", "thinking_only_response"} {
		if !strings.Contains(summary, required) {
			t.Fatalf("inventory missing explored target %q: %q", required, summary)
		}
	}
	if strings.Count(summary, "src/agentscope/agent/_agent.py") != 1 {
		t.Fatalf("inventory must dedupe repeated reads: %q", summary)
	}
	if strings.Contains(summary, "second read of the same file") {
		t.Fatalf("recent facts may be truncated but inventory must not duplicate: %q", summary)
	}
}

func TestSummarizeMessagesRetainsPriorCheckpointWhenCompactingAgain(t *testing.T) {
	messages := []ports.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "task"},
		{Role: "user", Content: contextCheckpointPrefix + "earlier decision: inspect config before changing it"},
		{Role: "assistant", Content: "new finding: the config remains unchanged"},
	}
	summary := summarizeMessages(messages, 240)
	if !strings.Contains(summary, "earlier decision") || !strings.Contains(summary, "new finding") {
		t.Fatalf("summary did not preserve both checkpoint and recent fact: %q", summary)
	}
}

func TestLoopFeedsPolicyDeniedToolCallBackToModelForRepair(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace}, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "call_bad_patch", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"answer.txt","mode":"update","replacements":[{"old":"original","new":"changed"}]}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "I will provide the required hash before retrying."}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	ctx := context.Background()
	if _, err := journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"repair arguments","workspace":"/workspace"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	result, err := loop.Run(ctx, RunInput{RunID: runID, Task: "repair arguments", WorkspaceHash: "workspace"})
	if err != nil || result.Response != "I will provide the required hash before retrying." {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if len(provider.requests) != 2 || !strings.Contains(provider.requests[1].Messages[len(provider.requests[1].Messages)-1].Content, "expected_sha256") {
		t.Fatalf("repair feedback was not sent to model: %#v", provider.requests)
	}
	events, err := journal.Load(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	seenFailure, seenResult := false, false
	for _, event := range events {
		seenFailure = seenFailure || event.Type == domain.EventOperationFailed
		seenResult = seenResult || event.Type == domain.EventToolResultAppended
	}
	if !seenFailure || !seenResult {
		t.Fatalf("policy denial was not fully recorded: %#v", events)
	}
}

func TestResumeClosesInterruptedModelAttemptBeforeNewCall(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID, turnID, attemptID := domain.NewRunID(), domain.NewTurnID(), domain.NewAttemptID()
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"recover","workspace":"/workspace"}`)})
	appendEvent(1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted})
	appendEvent(2, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID})
	appendEvent(3, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &turnID, AttemptID: &attemptID})
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "recovered"}, {Kind: ports.Done}}}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	result, err := loop.Resume(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, Approval{})
	if err != nil || result.Response != "recovered" {
		t.Fatalf("resume = %#v, %v", result, err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	failedIndex, secondStartIndex := -1, -1
	for index, event := range events {
		if event.Type == domain.EventModelRequestFailed && event.AttemptID != nil && *event.AttemptID == attemptID {
			failedIndex = index
		}
		if event.Type == domain.EventModelRequestStarted && event.AttemptID != nil && *event.AttemptID != attemptID {
			secondStartIndex = index
		}
	}
	if failedIndex < 0 || secondStartIndex <= failedIndex {
		t.Fatalf("interrupted attempt was not closed before new attempt: %#v", events)
	}
}

func TestResumeContinuesAfterClosedTurnWithoutPendingWork(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID, closedTurn := domain.NewRunID(), domain.NewTurnID()
	appendInitialRunningEvents(t, journal, runID, "continue after crash")
	for expected, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &closedTurn},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &closedTurn, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})},
	} {
		if _, err := journal.Append(context.Background(), uint64(expected+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "continued"}, {Kind: ports.Done}}}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	result, err := loop.Resume(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, Approval{})
	if err != nil || result.Response != "continued" || len(provider.requests) != 1 {
		t.Fatalf("resume=%#v err=%v requests=%d", result, err, len(provider.requests))
	}
}

func TestResumeProposesPersistedToolCallsBeforeRequestingAnotherModelResponse(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte("recovered read"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID, turnID, attemptID := domain.NewRunID(), domain.NewTurnID(), domain.NewAttemptID()
	appendInitialRunningEvents(t, journal, runID, "recover model response")
	call := ports.ToolCall{ID: "read-after-crash", Name: "read_file", Arguments: json.RawMessage(`{"path":"answer.txt"}`)}
	for expected, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &turnID, AttemptID: &attemptID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelResponseFinished, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelResponsePayload{Text: "read it", ToolCalls: []ports.ToolCall{call}})},
	} {
		if _, err := journal.Append(context.Background(), uint64(expected+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "recovered answer"}, {Kind: ports.Done}}}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	result, err := loop.Resume(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, Approval{})
	if err != nil || result.Response != "recovered answer" || len(provider.requests) != 1 {
		t.Fatalf("resume=%#v err=%v requests=%d", result, err, len(provider.requests))
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, domain.EventToolCallProposed) != 1 || countEvent(events, domain.EventToolResultAppended) != 1 {
		t.Fatalf("persisted response did not consume exactly one tool call: %#v", events)
	}
}

func TestResumeCompletesPersistedFinalModelResponseWithoutAnotherRequest(t *testing.T) {
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
	runID, turnID, attemptID := domain.NewRunID(), domain.NewTurnID(), domain.NewAttemptID()
	appendInitialRunningEvents(t, journal, runID, "recover final response")
	for expected, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &turnID, AttemptID: &attemptID},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelResponseFinished, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelResponsePayload{Text: "durable final"})},
	} {
		if _, err := journal.Append(context.Background(), uint64(expected+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	provider := &scriptedProvider{}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	result, err := loop.Resume(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, Approval{})
	if err != nil || result.Response != "durable final" || len(provider.requests) != 0 {
		t.Fatalf("resume=%#v err=%v requests=%d", result, err, len(provider.requests))
	}
}

func TestResumeClosesInterruptedReadBeforeStartingAnotherModelRequest(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"recover read","workspace":"/workspace"}`)})
	appendEvent(1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted})
	appendEvent(2, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID})
	attemptID := domain.NewAttemptID()
	appendEvent(3, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelRequestStarted, TurnID: &turnID, AttemptID: &attemptID})
	appendEvent(4, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventModelResponseFinished, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelResponsePayload{ToolCalls: []ports.ToolCall{{ID: "read-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"missing.txt"}`)}}})})
	appendEvent(5, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: "read-1", ToolName: "read_file", Arguments: json.RawMessage(`{"path":"missing.txt"}`)})})
	appendEvent(6, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow"}`)})
	appendEvent(7, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"read_file","side_effect_class":"none"}`)})
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "recovered after tool failure"}, {Kind: ports.Done}}}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	result, err := loop.Resume(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, Approval{})
	if err != nil || result.Response != "recovered after tool failure" {
		t.Fatalf("resume = %#v, %v", result, err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	failed, nextStart := -1, -1
	for index, event := range events {
		if event.Type == domain.EventOperationFailed && event.OperationID != nil && *event.OperationID == operationID {
			failed = index
		}
		if event.Type == domain.EventModelRequestStarted && event.AttemptID != nil && *event.AttemptID != attemptID {
			nextStart = index
		}
	}
	if failed < 0 || nextStart <= failed {
		t.Fatalf("read operation was not closed before a new model request: %#v", events)
	}
}

func TestFirstCancellationPersistsCancelledRun(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"cancel","workspace":"/workspace"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop := Loop{Journal: journal, Provider: cancellingProvider{cancel: cancel}, Tools: registry, Gate: gate, Model: "fake"}
	if _, err := loop.Run(ctx, RunInput{RunID: runID, Task: "cancel", WorkspaceHash: "workspace"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCancelled {
		t.Fatalf("projection = %#v, %v", projection, err)
	}
}

func TestCancellationClosesPendingApprovalBeforeTerminalEvent(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patch := tools.ApplyPatch{Workspace: workspace}
	registry, err := tools.NewRegistry(patch)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, patch)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"note.txt","mode":"create","content":"x"}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "cancel approval")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	input := RunInput{RunID: runID, Task: "cancel approval", WorkspaceHash: "workspace"}
	if _, err := loop.Run(context.Background(), input); err == nil {
		t.Fatal("run unexpectedly completed instead of waiting for approval")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loop.Resume(ctx, input, Approval{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("resume error = %v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCancelled || projection.PendingApproval != nil {
		t.Fatalf("projection = %#v, %v", projection, err)
	}
	foundSystemDenial := false
	for _, event := range events {
		foundSystemDenial = foundSystemDenial || event.Type == domain.EventApprovalDenied && strings.Contains(string(event.Payload), `"actor":"system"`)
	}
	if !foundSystemDenial {
		t.Fatalf("cancellation did not durably close the approval: %#v", events)
	}
}

func TestCancellationDuringProcessOperationPersistsNoFurtherModelRequest(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	registry, err := tools.NewRegistry(cancellingExecTool{cancel: cancel})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "call-exec", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["/usr/bin/true"]}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"cancel command","workspace":"/workspace"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	if _, err := loop.Run(ctx, RunInput{RunID: runID, Task: "cancel command", WorkspaceHash: "workspace"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider calls = %d; cancellation must prevent another model request", len(provider.requests))
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCancelled || projection.PendingOperation != nil {
		t.Fatalf("projection = %#v, %v", projection, err)
	}
	foundCancelled := false
	for _, event := range events {
		if event.Type == domain.EventOperationCancelled {
			foundCancelled = true
		}
	}
	if !foundCancelled {
		t.Fatalf("operation cancellation event missing: %#v", events)
	}
}

func TestUnconfirmedProcessTerminationLeavesOperationForReconciliation(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(uncertainExecTool{})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "uncertain", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["/usr/bin/true"]}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "uncertain command")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	_, runErr := loop.Run(context.Background(), RunInput{RunID: runID, Task: "uncertain command", WorkspaceHash: "workspace"})
	if !errors.Is(runErr, ErrWaitingReconcile) {
		t.Fatalf("run error = %v", runErr)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if projection.State != domain.RunRunning || projection.PendingOperation == nil || projection.PendingOperation.SideEffectClass != "process" {
		t.Fatalf("projection = %#v", projection)
	}
	if countEvent(events, domain.EventOperationFailed) != 0 || countEvent(events, domain.EventOperationCancelled) != 0 || countEvent(events, domain.EventToolResultAppended) != 0 || countEvent(events, domain.EventRunFailed) != 0 {
		t.Fatalf("unconfirmed operation was incorrectly closed: %#v", events)
	}
}

func TestLoopFeedsVerificationFailureBackForRepairBeforeCompletion(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	verificationTool := &flakyVerificationTool{}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace}, patchTool, verificationTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("broken"))
	expected := "sha256:" + hex.EncodeToString(digest[:])
	updateArguments, err := json.Marshal(tools.PatchArguments{Path: "target.txt", Mode: "update", ExpectedSHA256: expected, Replacements: []tools.Replacement{{Old: "broken", New: "fixed", Occurrence: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "create", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"target.txt","mode":"create","content":"broken"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "initial fix"}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "repair", Name: "apply_patch", Arguments: updateArguments}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "repaired and verified"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"repair","workspace":"/workspace"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 6, Verification: []VerificationCommand{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}}
	result, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "repair", WorkspaceHash: "workspace"})
	if err != nil || result.Response != "repaired and verified" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if verificationTool.calls != 2 || len(provider.requests) != 4 {
		t.Fatalf("verification calls=%d provider calls=%d", verificationTool.calls, len(provider.requests))
	}
	foundFeedback := false
	for _, message := range provider.requests[2].Messages {
		if message.Role == "user" && strings.Contains(message.Content, "Verification command verification-1 failed") {
			foundFeedback = true
		}
	}
	if !foundFeedback {
		t.Fatalf("repair request did not receive verification evidence: %#v", provider.requests[2].Messages)
	}
	content, err := os.ReadFile(filepath.Join(root, "target.txt"))
	if err != nil || string(content) != "fixed" {
		t.Fatalf("repaired content = %q, %v", content, err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted || !projection.VerificationSucceeded || projection.LastVerificationSeq <= projection.LastMutationSeq {
		t.Fatalf("projection = %#v, %v", projection, err)
	}
}

func TestLoopAutomaticallyCompletesAfterSuccessfulPatchVerification(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	verificationTool := &countingExecTool{}
	registry, err := tools.NewRegistry(patchTool, verificationTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"target.txt","mode":"create","content":"fixed"}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "repair")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, VerificationAutoAfterPatch: true, Verification: []VerificationCommand{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}}
	result, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "repair", WorkspaceHash: "workspace"})
	if err != nil || result.Response != automaticVerificationResponse {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if len(provider.requests) != 1 || verificationTool.calls != 1 {
		t.Fatalf("provider requests=%d verification calls=%d", len(provider.requests), verificationTool.calls)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted || !projection.VerificationSucceeded || projection.LastVerificationSeq <= projection.LastMutationSeq {
		t.Fatalf("projection = %#v, err = %v", projection, err)
	}
}

func TestLoopAutomaticallyVerifiesApprovedPatchOnResume(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	verificationTool := &countingExecTool{}
	registry, err := tools.NewRegistry(patchTool, verificationTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"target.txt","mode":"create","content":"fixed"}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "repair")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, VerificationAutoAfterPatch: true, Verification: []VerificationCommand{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}}
	input := RunInput{RunID: runID, Task: "repair", WorkspaceHash: "workspace"}
	_, err = loop.Run(context.Background(), input)
	var waiting *WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial run error = %v", err)
	}
	result, err := loop.Resume(context.Background(), input, Approval{Fingerprint: waiting.Fingerprint, Approve: true})
	if err != nil || result.Response != automaticVerificationResponse || len(provider.requests) != 1 || verificationTool.calls != 1 {
		t.Fatalf("result=%#v err=%v requests=%d verification calls=%d", result, err, len(provider.requests), verificationTool.calls)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted || !projection.VerificationSucceeded {
		t.Fatalf("projection = %#v, err = %v", projection, err)
	}
}

func TestLoopFeedsAutomaticVerificationFailureAfterApprovedPatchBackToModel(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	verificationTool := &flakyVerificationTool{}
	registry, err := tools.NewRegistry(patchTool, verificationTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"target.txt","mode":"create","content":"fixed"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "verification repaired"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "repair")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, VerificationAutoAfterPatch: true, Verification: []VerificationCommand{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}}
	input := RunInput{RunID: runID, Task: "repair", WorkspaceHash: "workspace"}
	_, err = loop.Run(context.Background(), input)
	var waiting *WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial run error = %v", err)
	}
	result, err := loop.Resume(context.Background(), input, Approval{Fingerprint: waiting.Fingerprint, Approve: true})
	if err != nil || result.Response != "verification repaired" || len(provider.requests) != 2 || verificationTool.calls != 2 {
		t.Fatalf("result=%#v err=%v requests=%d verification calls=%d", result, err, len(provider.requests), verificationTool.calls)
	}
	feedback := false
	for _, message := range provider.requests[1].Messages {
		feedback = feedback || message.Role == "user" && strings.Contains(message.Content, "Verification command verification-1 failed")
	}
	if !feedback {
		t.Fatalf("resumed repair request did not receive verification feedback: %#v", provider.requests[1].Messages)
	}
}

func TestLoopFeedsAutomaticVerificationFailureBackForRepair(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	verificationTool := &flakyVerificationTool{}
	registry, err := tools.NewRegistry(patchTool, verificationTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("broken"))
	updateArguments, err := json.Marshal(tools.PatchArguments{Path: "target.txt", Mode: "update", ExpectedSHA256: "sha256:" + hex.EncodeToString(digest[:]), Replacements: []tools.Replacement{{Old: "broken", New: "fixed", Occurrence: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "create", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"target.txt","mode":"create","content":"broken"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "repair", Name: "apply_patch", Arguments: updateArguments}}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "repair")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 4, VerificationAutoAfterPatch: true, Verification: []VerificationCommand{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}}
	result, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "repair", WorkspaceHash: "workspace"})
	if err != nil || result.Response != automaticVerificationResponse || len(provider.requests) != 2 || verificationTool.calls != 2 {
		t.Fatalf("result=%#v err=%v requests=%d verification calls=%d", result, err, len(provider.requests), verificationTool.calls)
	}
	feedback := false
	for _, message := range provider.requests[1].Messages {
		feedback = feedback || message.Role == "user" && strings.Contains(message.Content, "Verification command verification-1 failed")
	}
	if !feedback {
		t.Fatalf("repair request did not receive verification feedback: %#v", provider.requests[1].Messages)
	}
}

func TestLoopWaitsForAllToolCallsBeforeAutomaticVerification(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	verificationTool := &countingExecTool{}
	registry, err := tools.NewRegistry(patchTool, verificationTool, tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"target.txt","mode":"create","content":"fixed"}`)}},
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "list", Name: "list_files", Arguments: json.RawMessage(`{"path":"."}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "repair")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, VerificationAutoAfterPatch: true, Verification: []VerificationCommand{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "repair", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	listResult, verificationStarted := -1, -1
	for index, event := range events {
		if event.Type == domain.EventToolResultAppended && event.OperationID != nil && strings.Contains(string(event.Payload), "target.txt") {
			listResult = index
		}
		if event.Type == domain.EventVerificationStarted {
			verificationStarted = index
		}
	}
	if listResult < 0 || verificationStarted <= listResult {
		t.Fatalf("verification started before all model tool calls completed: %#v", events)
	}
}

func TestLoopRedactsAndRejectsSecretLikeToolArguments(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "leak", Name: "read_file", Arguments: json.RawMessage(`{"path":"answer.txt","token":"top-secret-value"}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"read","workspace":"/workspace"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake"}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "read", WorkspaceHash: "workspace"}); err == nil {
		t.Fatal("secret-like tool argument was accepted")
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunFailed {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), "top-secret-value") {
			t.Fatalf("secret leaked into event: %#v", event)
		}
	}
}

func TestLoopRequiresFreshApprovalWhenAReconciledActionIsProposedAgain(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	registry, err := tools.NewRegistry(patchTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	arguments := json.RawMessage(`{"path":"x.txt","mode":"create","content":"x"}`)
	decision, err := gate.Decide("apply_patch", arguments)
	if err != nil || decision.Effect != policy.Allow {
		t.Fatalf("initial decision=%#v err=%v", decision, err)
	}
	journal := memory.New()
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"retry","workspace":"/workspace"}`)})
	appendEvent(1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted})
	appendEvent(2, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID})
	appendEvent(3, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: "first", ToolName: "apply_patch", Arguments: arguments})})
	appendEvent(4, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Fingerprint string `json:"fingerprint"`
	}{Effect: "allow", Fingerprint: decision.Fingerprint})})
	appendEvent(5, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace","planned_path":"x.txt","planned_before":"absent","planned_after":"sha256:unknown"}`)})
	appendEvent(6, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationReconciled, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"verdict":"not_applied"}`)})
	appendEvent(7, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"ok":false,"code":"reconciled_not_applied"}`)})
	appendEvent(8, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnClosed, TurnID: &turnID, Payload: json.RawMessage(`{"outcome":"continue"}`)})
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "again", Name: "apply_patch", Arguments: arguments}},
		{Kind: ports.Done},
	}}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "retry", WorkspaceHash: "workspace"})
	var waiting *WaitingApprovalError
	if !errors.As(err, &waiting) || waiting.Fingerprint != decision.Fingerprint {
		t.Fatalf("re-proposal was not gated for fresh approval: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "x.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("reconciled action executed without a fresh approval: %v", statErr)
	}
}

func TestResumeExpiresCommandApprovalAfterWorkspaceDrift(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "context.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	executor := &countingExecTool{}
	registry, err := tools.NewRegistry(executor)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "command-1", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["/usr/bin/true"]}`)}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "command-2", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["/usr/bin/true"]}`)}}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	for expected, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"run command","workspace":"/workspace"}`)},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted},
	} {
		if _, err := journal.Append(context.Background(), uint64(expected), draft); err != nil {
			t.Fatal(err)
		}
	}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	input := RunInput{RunID: runID, Task: "run command", WorkspaceHash: "workspace"}
	_, err = loop.Run(context.Background(), input)
	var initial *WaitingApprovalError
	if !errors.As(err, &initial) {
		t.Fatalf("initial run err=%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "context.txt"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = loop.Resume(context.Background(), input, Approval{Fingerprint: initial.Fingerprint, Approve: true})
	var renewed *WaitingApprovalError
	if !errors.As(err, &renewed) || renewed.Fingerprint == initial.Fingerprint {
		t.Fatalf("stale approval did not require a new approval: %v", err)
	}
	if executor.calls != 0 {
		t.Fatalf("stale approval executed command %d times", executor.calls)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	foundStaleResult := false
	for _, event := range events {
		if event.Type == domain.EventOperationFailed && strings.Contains(string(event.Payload), "approval_stale") {
			foundStaleResult = true
		}
	}
	if !foundStaleResult {
		t.Fatalf("no durable stale-approval result in events: %#v", events)
	}
}

func TestLoopFailsBeforeToolExecutionWhenToolBudgetIsExhausted(t *testing.T) {
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
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "list-1", Name: "list_files", Arguments: json.RawMessage(`{"path":"."}`)}},
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "list-2", Name: "list_files", Arguments: json.RawMessage(`{"path":"."}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "tool budget")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, MaxToolCalls: 1}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "tool budget", WorkspaceHash: "workspace"})
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != "tool_calls" {
		t.Fatalf("error = %v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunFailed {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
	if countEvent(events, domain.EventOperationStarted) != 1 {
		t.Fatalf("started operations = %d, events=%#v", countEvent(events, domain.EventOperationStarted), events)
	}
}

func TestLoopFailsWhenReportedTokenBudgetIsExceeded(t *testing.T) {
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
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}},
		{Kind: ports.TextDelta, Text: "answer"}, {Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "token budget")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, MaxInputTokens: 1}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "token budget", WorkspaceHash: "workspace"})
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != "input_tokens" {
		t.Fatalf("error = %v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, domain.EventAssistantMessageAppended) != 0 || countEvent(events, domain.EventRunCompleted) != 0 {
		t.Fatalf("token-limited run reached completion events: %#v", events)
	}
}

func TestLoopFailsWhenReportedCostBudgetIsExceeded(t *testing.T) {
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
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}},
		{Kind: ports.TextDelta, Text: "answer"}, {Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "cost budget")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, MaxCostUSD: 0.002, InputTokenUSD: 0.001, OutputTokenUSD: 0.001}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "cost budget", WorkspaceHash: "workspace"})
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != "cost_usd" {
		t.Fatalf("error = %v", err)
	}
}

func TestVerificationRejectsCommandsThatMutateVisibleWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	registry, err := tools.NewRegistry(patchTool, mutatingVerificationTool{path: filepath.Join(root, "node_modules", "after-check.txt")})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"changed.txt","mode":"create","content":"changed"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "verify safely")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2, VerificationWorkspace: workspace.ObservationView(nil), Verification: []VerificationCommand{{ID: "check", Arguments: json.RawMessage(`{"argv":["/usr/bin/true"]}`)}}}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "verify safely", WorkspaceHash: "workspace"})
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != "turns" {
		t.Fatalf("error=%v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, domain.EventRunCompleted) != 0 || !containsEventPayload(events, domain.EventOperationFailed, "verification_mutated_workspace") {
		t.Fatalf("verification mutation was not rejected: %#v", events)
	}
}

func appendInitialRunningEvents(t *testing.T, journal *memory.Journal, runID domain.RunID, task string) {
	t.Helper()
	for expected, draft := range []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: mustJSON(domain.RunCreatedPayload{Task: task, Workspace: "/workspace"})},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted},
	} {
		if _, err := journal.Append(context.Background(), uint64(expected), draft); err != nil {
			t.Fatal(err)
		}
	}
}

func countEvent(events []domain.Event, eventType domain.EventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func containsEventPayload(events []domain.Event, eventType domain.EventType, fragment string) bool {
	for _, event := range events {
		if event.Type == eventType && strings.Contains(string(event.Payload), fragment) {
			return true
		}
	}
	return false
}

// retryableProviderError 模拟实现了 ports.ClassifiedError 的瞬态提供方失败。
type retryableProviderError struct{}

func (retryableProviderError) Error() string      { return "provider rate_limited: upstream saturated" }
func (retryableProviderError) ErrorClass() string { return "rate_limited" }
func (retryableProviderError) IsRetryable() bool  { return true }

// TestLoopKeepsRunResumableAfterRetryableProviderFailure 锁定瞬态提供方失败的
// 语义：失败与分类被持久化、当前轮次关闭，但 run 保持 running，resume 可带着
// 既有上下文续跑至完成。
func TestLoopKeepsRunResumableAfterRetryableProviderFailure(t *testing.T) {
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
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{nil, {{Kind: ports.TextDelta, Text: "recovered"}, {Kind: ports.Done}}}, errors: []error{retryableProviderError{}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "transient")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake"}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "transient", WorkspaceHash: "workspace"}); err == nil {
		t.Fatal("retryable provider failure must surface an error")
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunRunning {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
	if !containsEventPayload(events, domain.EventModelRequestFailed, `"error_class":"rate_limited"`) || !containsEventPayload(events, domain.EventModelRequestFailed, `"retryable":true`) {
		t.Fatalf("failure classification missing: %#v", events)
	}
	if countEvent(events, domain.EventRunFailed) != 0 {
		t.Fatalf("retryable failure must not terminate the run: %#v", events)
	}
	resumed, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "transient", WorkspaceHash: "workspace"})
	if err != nil || resumed.Response != "recovered" {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	finalEvents, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	final, err := domain.Replay(finalEvents)
	if err != nil || final.State != domain.RunCompleted {
		t.Fatalf("final projection=%#v err=%v", final, err)
	}
}

// cancelingJournal 在第 N 次 Append 时取消运行上下文并以 canceled 失败，模拟
// 取消信号落在事件追加间隙的竞态窗口。
type cancelingJournal struct {
	ports.Journal
	cancel    context.CancelFunc
	failAfter uint64
	appends   uint64
}

// Append 在命中注入点时先取消上下文再返回取消错误，其余调用原样透传。
func (journal *cancelingJournal) Append(ctx context.Context, expectedSeq uint64, draft domain.EventDraft) (domain.Event, error) {
	journal.appends++
	if journal.appends == journal.failAfter {
		journal.cancel()
		return domain.Event{}, context.Canceled
	}
	return journal.Journal.Append(ctx, expectedSeq, draft)
}

// TestLoopPersistsCancellationWhenInterruptHitsAppendGap 锁定顶层兜底：取消落在
// 追加间隙、主循环以原始 append 错误退出时，run 仍必须补写完整取消序列。
func TestLoopPersistsCancellationWhenInterruptHitsAppendGap(t *testing.T) {
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
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "partial"}, {Kind: ports.Done}}}}
	inner := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, inner, runID, "gap")
	runContext, cancel := context.WithCancel(context.Background())
	journal := &cancelingJournal{Journal: inner, cancel: cancel, failAfter: 3}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake"}
	_, err = loop.Run(runContext, RunInput{RunID: runID, Task: "gap", WorkspaceHash: "workspace"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	cancel()
	events, err := inner.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCancelled {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
	if countEvent(events, domain.EventRunCancelRequested) != 1 || countEvent(events, domain.EventRunCancelled) != 1 {
		t.Fatalf("cancellation was not persisted once: %#v", events)
	}
}

// TestLoopEnforcesOutputBudgetWithEstimatedUsageWhenProviderOmitsUsage 锁定
// usage 缺失兜底：不回报 usage 的端点上，预算用字节估算近似生效并显式标记。
func TestLoopEnforcesOutputBudgetWithEstimatedUsageWhenProviderOmitsUsage(t *testing.T) {
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
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "this response is long enough to exceed a tiny output budget"}, {Kind: ports.Done}}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "estimate")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, MaxOutputTokens: 1}
	_, err = loop.Run(context.Background(), RunInput{RunID: runID, Task: "estimate", WorkspaceHash: "workspace"})
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != "output_tokens" {
		t.Fatalf("err = %v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsEventPayload(events, domain.EventModelResponseFinished, `"usage_estimated":true`) {
		t.Fatalf("estimated usage was not flagged: %#v", events)
	}
}
