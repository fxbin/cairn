package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// TestResumeEnforcesPersistedResponseBudget 覆盖响应落盘后中断的恢复窗口，
// 超限不得继续模型、工具、验证或完成；未确认的副作用仍须先对账。
func TestResumeEnforcesPersistedResponseBudget(t *testing.T) {
	for _, dimension := range []string{"input_tokens", "output_tokens", "cost_usd"} {
		for _, stage := range []string{"final", "tool", "verification proposed", "final closed", "read pending", "process pending", "approval pending", "continue closed"} {
			t.Run(dimension+"/"+stage, func(t *testing.T) {
				loop, input, journal, provider, command := persistedBudgetFixture(t, stage)
				switch dimension {
				case "input_tokens":
					loop.MaxInputTokens = 1
				case "output_tokens":
					loop.MaxOutputTokens = 1
				case "cost_usd":
					loop.MaxCostUSD, loop.InputTokenUSD, loop.OutputTokenUSD = 0.003, 0.001, 0.001
				}
				var err error
				if stage == "continue closed" {
					_, err = loop.Run(context.Background(), input)
				} else {
					_, err = loop.Resume(context.Background(), input, ports.Approval{})
				}
				events, loadErr := journal.Load(context.Background(), input.RunID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				projection, replayErr := domain.Replay(events)
				if replayErr != nil {
					t.Fatal(replayErr)
				}
				if stage == "process pending" {
					if !errors.Is(err, ports.ErrWaitingReconcile) || projection.PendingOperation == nil || projection.State != domain.RunRunning {
						t.Fatalf("pending side effect lost: projection=%#v err=%v", projection, err)
					}
				} else {
					var exceeded *ports.BudgetExceededError
					if !errors.As(err, &exceeded) || exceeded.Dimension != dimension || projection.State != domain.RunFailed {
						t.Fatalf("state=%s err=%v, want failed %s", projection.State, err, dimension)
					}
					if projection.PendingApproval != nil || projection.PendingOperation != nil || projection.CurrentTurn != nil {
						t.Fatalf("budget stop left unresolved state: %#v", projection)
					}
				}
				if len(provider.requests) != 0 || command.calls != 0 || countEvent(events, domain.EventRunCompleted) != 0 {
					t.Fatalf("over-budget recovery continued: model=%d exec=%d", len(provider.requests), command.calls)
				}
			})
		}
	}
}

// TestResumeAllowsPersistedResponseAtBudgetBoundary 保留既有严格大于上限的语义。
func TestResumeAllowsPersistedResponseAtBudgetBoundary(t *testing.T) {
	loop, input, journal, provider, command := persistedBudgetFixture(t, "final")
	loop.MaxInputTokens, loop.MaxOutputTokens = 2, 2
	loop.MaxCostUSD, loop.InputTokenUSD, loop.OutputTokenUSD = 0.004, 0.001, 0.001
	if _, err := loop.Resume(context.Background(), input, ports.Approval{}); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(context.Background(), input.RunID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted || len(provider.requests) != 0 || command.calls != 1 {
		t.Fatalf("projection=%#v err=%v model=%d exec=%d", projection, err, len(provider.requests), command.calls)
	}
}

// persistedBudgetFixture 构造含已落盘用量的运行及恢复覆盖层；Provider 仅作离线兜底。
func persistedBudgetFixture(t *testing.T, stage string) (*Loop, RunInput, *memory.Journal, *scriptedProvider, *countingExecTool) {
	t.Helper()
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	patch := tools.ApplyPatch{Workspace: workspace}
	gate, err := policy.New("allow", "allow", nil, patch)
	if err != nil {
		t.Fatal(err)
	}
	command := &countingExecTool{}
	registry, err := tools.NewRegistry(command, tools.ListFiles{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1}}, {Kind: ports.Done}}}}
	journal := memory.New()
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	input := RunInput{RunID: runID, Task: "persisted budget", WorkspaceHash: "workspace"}
	appendInitialRunningEvents(t, journal, runID, input.Task)
	seq := uint64(2)
	appendEvent := func(kind domain.EventType, operation *domain.OperationID, payload json.RawMessage) {
		t.Helper()
		if _, err := journal.Append(context.Background(), seq, domain.EventDraft{RunID: runID, WorkspaceHash: input.WorkspaceHash, Type: kind, TurnID: &turnID, OperationID: operation, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		seq++
	}
	appendEvent(domain.EventTurnOpened, nil, json.RawMessage(`{"turn":1}`))
	appendEvent(domain.EventModelRequestStarted, nil, json.RawMessage(`{"provider":"fake","model":"fake"}`))
	call := ports.ToolCall{ID: "command", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["verify"]}`)}
	response := modelResponsePayload{Text: "done", Usage: &ports.Usage{PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4}, StopReason: "stop"}
	if stage == "tool" || stage == "process pending" || stage == "approval pending" || stage == "read pending" {
		if stage == "read pending" {
			call.Name, call.Arguments = "list_files", json.RawMessage(`{}`)
		}
		response.ToolCalls = []ports.ToolCall{call}
	}
	appendEvent(domain.EventModelResponseFinished, nil, mustJSON(response))
	commands := []VerificationCommand{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}
	observation, err := workspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if stage == "verification proposed" || stage == "final closed" {
		appendEvent(domain.EventAssistantMessageAppended, nil, mustJSON(domain.AssistantMessagePayload{Text: "done"}))
		fingerprint, err := FingerprintVerificationCommands(commands)
		if err != nil {
			t.Fatal(err)
		}
		appendEvent(domain.EventVerificationStarted, nil, mustJSON(struct {
			VerificationID       string `json:"verification_id"`
			CommandsFingerprint  string `json:"commands_fingerprint"`
			WorkspaceObservation string `json:"workspace_observation"`
		}{VerificationID: "verify-1", CommandsFingerprint: fingerprint, WorkspaceObservation: observation}))
		if stage == "verification proposed" {
			appendEvent(domain.EventVerificationCommandProposed, &operationID, mustJSON(verificationProposalPayload{VerificationID: "verify-1", CommandIndex: 0, Arguments: commands[0].Arguments}))
		} else {
			appendEvent(domain.EventVerificationFinished, nil, mustJSON(domain.VerificationFinishedPayload{VerificationID: "verify-1", OK: false}))
			appendEvent(domain.EventTurnClosed, nil, mustJSON(domain.TurnClosedPayload{Outcome: "final"}))
		}
	}
	if stage == "process pending" || stage == "read pending" || stage == "approval pending" {
		appendEvent(domain.EventToolCallProposed, &operationID, mustJSON(toolCallPayload{CallID: call.ID, ToolName: call.Name, Arguments: call.Arguments}))
		if stage == "approval pending" {
			appendEvent(domain.EventPolicyDecided, &operationID, json.RawMessage(`{"effect":"ask","fingerprint":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
			appendEvent(domain.EventApprovalRequested, &operationID, json.RawMessage(`{"summary":"command approval","fingerprint":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
		} else {
			appendEvent(domain.EventPolicyDecided, &operationID, json.RawMessage(`{"effect":"allow","fingerprint":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`))
			class := "process"
			if stage == "read pending" {
				class = "none"
			}
			appendEvent(domain.EventOperationStarted, &operationID, mustJSON(operationPayload{ToolName: call.Name, SideEffectClass: class}))
		}
	}
	if stage == "continue closed" {
		appendEvent(domain.EventTurnClosed, nil, mustJSON(domain.TurnClosedPayload{Outcome: "continue"}))
	}
	return &Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 4, Verification: commands}, input, journal, provider, command
}
