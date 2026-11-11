package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/ports"
)

type reviewProvider func(context.Context, ports.ModelRequest, func(ports.ModelEvent) error) error

func (provider reviewProvider) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	return provider(ctx, request, emit)
}

func reviewComplete(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	for _, event := range []ports.ModelEvent{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}, {Kind: ports.Done, StopReason: "stop"}} {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func reviewHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "AGENT_") {
			t.Setenv(key, "")
		}
	}
}

func reviewConfig(t *testing.T, workspace string) config.Config {
	t.Helper()
	body := "version: 2\nprovider:\n  base_url: https://review.test/v1\n  model: review-model\n  api_key_env: REVIEW_KEY\n  admission: optional\n  capability:\n    tool_calls: true\n    streaming_usage: true\n"
	path := config.DefaultPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Load(path, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

func reviewWaitEvent(t *testing.T, events <-chan StreamEvent, kind string) StreamEvent {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Kind == kind {
				return event
			}
		case <-timer.C:
			t.Fatalf("missing %s event", kind)
			return StreamEvent{}
		}
	}
}

func reviewWaitIdle(t *testing.T, controller *RunController) {
	t.Helper()
	done := make(chan struct{})
	go func() { controller.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not finish")
	}
}

func TestReviewQueueConfirmationRegistersCancellation(t *testing.T) {
	reviewHome(t)
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := reviewConfig(t, workspace)
	entered := make(chan config.Config, 1)
	service := app.Service{NewProvider: func(cfg config.Config) (ports.Provider, error) {
		entered <- cfg
		return reviewProvider(func(ctx context.Context, _ ports.ModelRequest, _ func(ports.ModelEvent) error) error {
			<-ctx.Done()
			return ctx.Err()
		}), nil
	}}
	if _, err := service.SubmitQueueInput(context.Background(), app.QueueSubmitOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, InputID: "q", Content: "task"}); err != nil {
		t.Fatal(err)
	}
	events := make(chan StreamEvent, 256)
	prefs := NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json"))
	controller := NewRunController(service, prefs, func(event StreamEvent) { events <- event })
	t.Cleanup(func() {
		if err := controller.Shutdown(2 * time.Second); err != nil {
			t.Error(err)
		}
	})
	// 使用真实 JSON 请求边界，旧桥接忽略 mode 时必须被该用例发现。
	var request QueueActionRequest
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"workspace":%q,"state_dir":%q,"input_id":"q","mode":"read-only"}`, workspace, stateDir)), &request); err != nil {
		t.Fatal(err)
	}
	ack, err := controller.ConfirmQueue(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !controller.IsRunning(ack.RunID) {
		t.Fatal("queue ACK was returned without running registration")
	}
	if err := controller.SubmitSteering(ack.RunID, "steer", "extra context"); err != nil {
		t.Fatal(err)
	}
	select {
	case cfg := <-entered:
		if cfg.Preset != config.PresetReadOnly {
			t.Fatal("queue bypassed read-only mode")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider was not reached")
	}
	if err := controller.Cancel(ack.RunID, "stop"); err != nil {
		t.Fatal(err)
	}
	finished := reviewWaitEvent(t, events, "run.finished")
	encoded, err := json.Marshal(finished)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(encoded, &identity); err != nil {
		t.Fatal(err)
	}
	if finished.RunID != ack.RunID || identity.Workspace != workspace {
		t.Fatalf("wrong event identity: %+v", finished)
	}
	reviewWaitIdle(t, controller)
}

func TestReviewQueueIdempotentConfirmationDoesNotEmitCompletion(t *testing.T) {
	reviewHome(t)
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := reviewConfig(t, workspace)
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return reviewProvider(reviewComplete), nil }}
	if _, err := service.SubmitQueueInput(context.Background(), app.QueueSubmitOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, InputID: "q", Content: "task"}); err != nil {
		t.Fatal(err)
	}
	events := make(chan StreamEvent, 256)
	controller := NewRunController(service, nil, func(event StreamEvent) { events <- event })
	t.Cleanup(func() { _ = controller.Shutdown(2 * time.Second) })
	request := QueueActionRequest{Workspace: workspace, StateDir: stateDir, InputID: "q"}
	first, err := controller.ConfirmQueue(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	reviewWaitEvent(t, events, "run.finished")
	reviewWaitIdle(t, controller)
	second, err := controller.ConfirmQueue(context.Background(), request)
	if err != nil || second.RunID != first.RunID || second.State != "completed" {
		t.Fatalf("wrong idempotent state: %+v %v", second, err)
	}
	reviewWaitIdle(t, controller)
	select {
	case event := <-events:
		t.Fatalf("idempotent lookup emitted a fresh event: %+v", event)
	default:
	}
}

func TestReviewQueueReservationReturnsActualError(t *testing.T) {
	reviewHome(t)
	workspace := t.TempDir()
	reviewConfig(t, workspace)
	controller := NewRunController(app.Service{}, nil, nil)
	defer controller.Shutdown(time.Second)
	_, err := controller.ConfirmQueue(context.Background(), QueueActionRequest{Workspace: workspace, StateDir: t.TempDir(), InputID: "missing"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("reservation error was hidden: %v", err)
	}
}

func TestReviewWorkflowSessionAndFrozenSnapshot(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(fmt.Sprint(mutate), func(t *testing.T) {
			reviewHome(t)
			workspace := t.TempDir()
			configuration := reviewConfig(t, workspace)
			entered, release := make(chan struct{}), make(chan struct{})
			requests := make(chan ports.ModelRequest, 2)
			var calls atomic.Int32
			service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) {
				return reviewProvider(func(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
					requests <- request
					if calls.Add(1) == 1 {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return reviewComplete(ctx, request, emit)
				}), nil
			}}
			events := make(chan StreamEvent, 256)
			prefs := NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json"))
			controller := NewRunController(service, prefs, func(event StreamEvent) { events <- event })
			t.Cleanup(func() { _ = controller.Shutdown(2 * time.Second) })
			definition := WorkflowDefinition{ID: "w", Workspace: workspace, Name: "two steps", Steps: []WorkflowStep{{Task: "step-one", Model: "first-model", Mode: "read-only"}, {Task: "step-two", Model: "second-model"}}}
			if err := prefs.SaveWorkflow(definition); err != nil {
				t.Fatal(err)
			}
			ack, err := controller.RunWorkflow(context.Background(), workspace, "w", false)
			if err != nil || len(ack.RunIDs) != 1 {
				t.Fatalf("missing first run ACK: %+v %v", ack, err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("workflow never started")
			}
			if !controller.IsRunning(ack.RunIDs[0]) {
				t.Fatal("workflow run is not cancellable")
			}
			if override, ok := prefs.LoadRunOverride(ack.RunIDs[0]); !ok || override.Model != "first-model" || override.Mode != "read-only" {
				t.Fatalf("workflow override lost: %+v", override)
			}
			if mutate {
				definition.Steps[1] = WorkflowStep{Task: "changed", Model: "changed"}
				if err := prefs.SaveWorkflow(definition); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config.DefaultPath(workspace), []byte("version: [invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			reviewWaitEvent(t, events, "run.finished")
			reviewWaitEvent(t, events, "run.finished")
			reviewWaitIdle(t, controller)
			first, second := <-requests, <-requests
			if first.Model != "first-model" || second.Model != "second-model" {
				t.Fatalf("running snapshot drifted: %q %q", first.Model, second.Model)
			}
			found := false
			for _, message := range second.Messages {
				if message.Role == "user" && strings.Contains(message.Content, "step-two") {
					found = true
				}
			}
			if !found {
				t.Fatal("workflow task snapshot drifted")
			}
			runs, err := service.ListRuns(context.Background(), workspace, app.DefaultStateDir(), configuration)
			if err != nil || len(runs) != 2 {
				t.Fatalf("second step did not complete: %+v %v", runs, err)
			}
			byID := map[string]app.RunHistoryItem{}
			for _, run := range runs {
				byID[string(run.ID)] = run
			}
			root := byID[ack.RunIDs[0]]
			linked := 0
			for _, run := range runs {
				if run.PrevRunID == root.ID && run.SessionID == root.SessionID {
					linked++
				}
			}
			if linked != 1 {
				t.Fatalf("workflow did not retain one session chain: %+v", runs)
			}
		})
	}
}

func TestReviewHooksStayInOriginWorkspaceAndStateDirectory(t *testing.T) {
	reviewHome(t)
	workspace, other, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	configuration := reviewConfig(t, workspace)
	otherConfig := reviewConfig(t, other)
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return reviewProvider(reviewComplete), nil }}
	prefs := NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json"))
	for i, root := range []string{workspace, other} {
		if err := prefs.UpsertHook(HookRule{ID: fmt.Sprint(i), Workspace: root, Event: "run_finished", Enabled: true, Actions: []HookAction{{Kind: "queue_followup", Content: "followup"}}}); err != nil {
			t.Fatal(err)
		}
	}
	events := make(chan StreamEvent, 256)
	controller := NewRunController(service, prefs, func(event StreamEvent) { events <- event })
	t.Cleanup(func() { _ = controller.Shutdown(2 * time.Second) })
	if _, err := controller.StartRun(context.Background(), StartRunRequest{Workspace: workspace, StateDir: stateDir, Task: "origin"}); err != nil {
		t.Fatal(err)
	}
	reviewWaitEvent(t, events, "run.finished")
	reviewWaitIdle(t, controller)
	own, err := service.ListQueue(context.Background(), workspace, stateDir, configuration)
	if err != nil || len(own) != 1 {
		t.Fatalf("origin hook was routed to wrong state directory: %+v %v", own, err)
	}
	foreign, err := service.ListQueue(context.Background(), other, app.DefaultStateDir(), otherConfig)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign hook fired: %+v %v", foreign, err)
	}
}

func reviewGit(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.test"}, {"config", "user.name", "test"}, {"config", "commit.gpgsign", "false"}} {
		if _, err := runGit(root, args...); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReviewDiffCleanFileAndLiteralPath(t *testing.T) {
	reviewHome(t)
	root := reviewGit(t)
	for name, content := range map[string]string{"clean.txt": "clean\n", ":(glob)*": "literal\n", ".env": "SYNTHETIC_PRIVATE_MARKER=old\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runGit(root, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	if diff, err := DiffFile(root, "clean.txt", DiffScopeAll); err != nil || diff != "" {
		t.Fatalf("clean file appeared added: %q %v", diff, err)
	}
	if err := os.WriteFile(filepath.Join(root, ":(glob)*"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SYNTHETIC_PRIVATE_MARKER=new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	diff, err := DiffFile(root, ":(glob)*", DiffScopeAll)
	if err != nil || !strings.Contains(diff, "changed") || strings.Contains(diff, "SYNTHETIC_PRIVATE_MARKER") {
		t.Fatalf("literal path selected other files: %q %v", diff, err)
	}
	if _, err := DiffFile(root, "clean.txt", DiffScope("unknown")); err == nil {
		t.Fatal("unknown diff scope accepted")
	}
}

func TestReviewRenameConsumesSourceRecord(t *testing.T) {
	reviewHome(t)
	root := reviewGit(t)
	if err := os.WriteFile(filepath.Join(root, " M phantom.txt"), []byte("same"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "mv", " M phantom.txt", "new.txt"); err != nil {
		t.Fatal(err)
	}
	changes, err := ListChanges(root)
	if err != nil || len(changes.Entries) != 1 || changes.Entries[0].Relative != "new.txt" {
		t.Fatalf("rename source became phantom entry: %+v %v", changes, err)
	}
}

func TestReviewInstructionsDoNotFollowTemporaryOrTargetLinks(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(root, "AGENTS.md.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := WriteProjectInstructions(root, "new rules"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "unchanged" {
		t.Fatalf("temporary symlink was followed: %q %v", raw, err)
	}
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := WriteProjectInstructions(root, "overwrite"); err == nil {
		t.Fatal("target symlink accepted")
	}
	raw, err = os.ReadFile(target)
	if err != nil || string(raw) != "keep" {
		t.Fatalf("fixed-path writer changed an alias target: %q %v", raw, err)
	}
}

func TestReviewCorruptPreferencesNeverOverwriteOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	original := []byte("{broken")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".corrupt", []byte("older backup"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewPrefsStoreAt(path)
	if err := store.TouchProject(t.TempDir()); err == nil {
		t.Fatal("corrupt preferences allowed a silent overwrite")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(original) {
		t.Fatalf("original corrupt state lost: %q %v", raw, err)
	}
	raw, err = os.ReadFile(path + ".corrupt")
	if err != nil || string(raw) != "older backup" {
		t.Fatalf("older recovery evidence replaced: %q %v", raw, err)
	}
}
