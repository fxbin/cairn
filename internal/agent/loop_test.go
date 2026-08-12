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
	requests []ports.ModelRequest
}

type cancellingProvider struct{ cancel context.CancelFunc }

type cancellingExecTool struct{ cancel context.CancelFunc }

type flakyVerificationTool struct{ calls int }

type countingExecTool struct{ calls int }

type mutatingVerificationTool struct{ path string }

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
	for _, event := range provider.scripts[index] {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
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
