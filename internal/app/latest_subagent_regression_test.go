package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

func prepareLatestDelegation(t *testing.T) (string, string, config.Config, domain.RunID, domain.RunID) {
	t.Helper()
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	parent, err := service.Run(context.Background(), RunOptions{Task: "parent", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	child := domain.NewRunID()
	events, err := journal.Load(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	delegatedAt := events[len(events)-1].OccurredAt.Add(time.Nanosecond)
	_, err = RecordSubagentDelegated(context.Background(), journal, parent.ID, hash, DelegateOptions{ParentRunID: parent.ID, ChildRunID: child, Task: "patch"}, delegatedAt)
	closeErr := journal.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("委派失败: %v %v", err, closeErr)
	}
	return workspace, stateDir, configuration, parent.ID, child
}

func assertLatestChildState(t *testing.T, workspace, stateDir string, configuration config.Config, parent, child domain.RunID, state, outcome string) {
	t.Helper()
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, parent, true)
	if err != nil {
		t.Fatal(err)
	}
	record := inspected.Projection.Children[string(child)]
	if record.State != state || record.Outcome != outcome || inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("父子投影错误: %+v parent=%s", record, inspected.Projection.State)
	}
	count := 0
	for _, event := range inspected.Events {
		if event.Type == domain.EventSubagentCompleted {
			count++
		}
	}
	expected := 0
	if state == "completed" {
		expected = 1
	}
	if count != expected {
		t.Fatalf("终态回执重复或缺失: %d", count)
	}
}

func TestLatestSubagentPauseResumeAndAbort(t *testing.T) {
	for _, action := range []string{"resume", "abort", "reconcile_abort"} {
		t.Run(action, func(t *testing.T) {
			workspace, stateDir, configuration, parent, child := prepareLatestDelegation(t)
			provider := &patchThenFinalProvider{}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
			result, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration, PreassignedRunID: child, ParentRunID: parent})
			var waiting *ports.WaitingApprovalError
			if !errors.As(err, &waiting) || result.ID != child {
				t.Fatalf("未进入预期审批暂停: %+v %v", result, err)
			}
			assertLatestChildState(t, workspace, stateDir, configuration, parent, child, "delegated", "")
			expected := "failed"
			switch action {
			case "resume":
				_, err = service.Resume(context.Background(), ResumeOptions{RunID: child, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
				expected = "ok"
			case "abort":
				_, err = service.Abort(context.Background(), AbortOptions{RunID: child, Workspace: workspace, StateDir: stateDir, Config: configuration, Reason: "test abort"})
			case "reconcile_abort":
				_, hash, _ := workspaceFor(workspace, configuration)
				journal, openErr := openJournal(stateDir, hash)
				if openErr != nil {
					t.Fatal(openErr)
				}
				events, loadErr := journal.Load(context.Background(), child)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				projection, replayErr := domain.Replay(events)
				if replayErr != nil || projection.CurrentTurn == nil {
					t.Fatalf("invalid pending approval: %v", replayErr)
				}
				turn, operation := *projection.CurrentTurn, waiting.OperationID
				for index, draft := range []domain.EventDraft{
					{Type: domain.EventApprovalGranted, Payload: json.RawMessage(`{"fingerprint":"` + waiting.Fingerprint + `","actor":"test"}`)},
					{Type: domain.EventPolicyDecided, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"` + waiting.Fingerprint + `"}`)},
					{Type: domain.EventOperationStarted, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)},
				} {
					draft.RunID, draft.WorkspaceHash, draft.TurnID, draft.OperationID = child, hash, &turn, &operation
					draft.OccurredAt = events[len(events)-1].OccurredAt.Add(time.Duration(index+1) * time.Nanosecond)
					if _, appendErr := journal.Append(context.Background(), projection.LastSeq+uint64(index), draft); appendErr != nil {
						t.Fatal(appendErr)
					}
				}
				if closeErr := journal.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				_, err = service.Reconcile(context.Background(), ReconcileOptions{RunID: child, OperationID: operation, Workspace: workspace, StateDir: stateDir, Config: configuration, Verdict: "abort_run", Note: "test abort"})
			}
			if err != nil {
				t.Fatal(err)
			}
			assertLatestChildState(t, workspace, stateDir, configuration, parent, child, "completed", expected)
		})
	}
}

func TestLatestSubagentCancellationUsesPersistedOutcome(t *testing.T) {
	workspace, stateDir, configuration, parent, child := prepareLatestDelegation(t)
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := service.Run(ctx, RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration, PreassignedRunID: child, ParentRunID: parent, OnEvent: func(event domain.Event) {
		if event.Type == domain.EventRunStarted {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) || result.ID != child {
		t.Fatalf("取消结果错误: %+v %v", result, err)
	}
	assertLatestChildState(t, workspace, stateDir, configuration, parent, child, "completed", "cancelled")
}

func TestLatestSubagentAdmissionRejectsWrongIdentityAndBudget(t *testing.T) {
	for _, invalid := range []string{"parent", "task", "child", "budget", "historical_budget"} {
		t.Run(invalid, func(t *testing.T) {
			workspace, stateDir, configuration, parent, child := prepareLatestDelegation(t)
			provider := &admissionGuardProvider{}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
			options := RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration, PreassignedRunID: child, ParentRunID: parent}
			switch invalid {
			case "parent":
				options.ParentRunID = "missing-parent"
			case "task":
				options.Task = "different task"
			case "child":
				options.PreassignedRunID = domain.NewRunID()
			case "historical_budget":
				_, hash, _ := workspaceFor(workspace, configuration)
				journal, openErr := openJournal(stateDir, hash)
				if openErr != nil {
					t.Fatal(openErr)
				}
				events, loadErr := journal.Load(context.Background(), parent)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				options.PreassignedRunID = domain.NewRunID()
				payload, marshalErr := json.Marshal(DelegatedPayload{ChildRunID: string(options.PreassignedRunID), TaskDigest: TaskDigest("patch"), Budget: SubagentBudget{MaxTokens: -1}})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				_, appendErr := journal.Append(context.Background(), events[len(events)-1].Seq, domain.EventDraft{RunID: parent, WorkspaceHash: hash, Type: domain.EventSubagentDelegated, OccurredAt: events[len(events)-1].OccurredAt.Add(time.Nanosecond), Payload: payload})
				_ = journal.Close()
				if appendErr != nil {
					t.Fatal(appendErr)
				}
			case "budget":
				_, hash, _ := workspaceFor(workspace, configuration)
				journal, err := openJournal(stateDir, hash)
				if err != nil {
					t.Fatal(err)
				}
				options.PreassignedRunID = domain.NewRunID()
				events, loadErr := journal.Load(context.Background(), parent)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				_, err = RecordSubagentDelegated(context.Background(), journal, parent, hash, DelegateOptions{ParentRunID: parent, ChildRunID: options.PreassignedRunID, Task: "patch", Budget: SubagentBudget{MaxTurns: 1}}, events[len(events)-1].OccurredAt.Add(time.Nanosecond))
				_ = journal.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			result, err := service.Run(context.Background(), options)
			if !errors.Is(err, ErrSubagentAppend) || result.ID != "" || provider.calls != 0 {
				t.Fatalf("非法委派进入执行: %+v err=%v calls=%d", result, err, provider.calls)
			}
		})
	}
}

func TestLatestSubagentNotificationFailureIsReturned(t *testing.T) {
	workspace, stateDir, configuration, parent, child := prepareLatestDelegation(t)
	_, hash, _ := workspaceFor(workspace, configuration)
	database, err := sql.Open("sqlite", filepath.Join(stateDir, "workspaces", hash, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TRIGGER reject_subagent_completion BEFORE INSERT ON events WHEN NEW.event_type='subagent_completed' BEGIN SELECT RAISE(ABORT,'injected notification failure'); END`); err != nil {
		t.Fatal(err)
	}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	result, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration, PreassignedRunID: child, ParentRunID: parent})
	if !errors.Is(err, ErrSubagentAppend) || result.Result == nil || result.Result.State != string(domain.RunCompleted) {
		t.Fatalf("回写错误被吞或子终态被改写: %+v %v", result, err)
	}
	assertLatestChildState(t, workspace, stateDir, configuration, parent, child, "delegated", "")
}
