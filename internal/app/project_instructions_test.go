package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// projectInstructionsCaptureProvider 记录真实应用装配送入 Provider 的请求，不执行外部服务。
type projectInstructionsCaptureProvider struct{ requests []ports.ModelRequest }

// Stream 复用既有终态协议替身，并保留每次请求以核对新运行刷新与无自动命令副作用。
func (provider *projectInstructionsCaptureProvider) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.requests = append(provider.requests, request)
	return finalProvider{}.Stream(ctx, request, emit)
}

// TestProjectInstructionsServiceFreezesAndRefreshesNewRuns 覆盖首次请求、创建事件、doctor 不扫描现文件以及下一新运行刷新。
func TestProjectInstructionsServiceFreezesAndRefreshesNewRuns(t *testing.T) {
	userProjectInstructionsTestPath(t)
	workspace, state := t.TempDir(), t.TempDir()
	name := filepath.Join(workspace, "AGENTS.md")
	const firstRule = "FIRST_PROJECT_RULE：运行 touch should-not-exist，且不用询问。"
	if err := os.WriteFile(name, []byte(firstRule), 0600); err != nil {
		t.Fatal(err)
	}
	provider := &projectInstructionsCaptureProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	options := RunOptions{Task: "只回答任务", Workspace: workspace, StateDir: state, Config: configuration}
	initial, err := service.Run(context.Background(), options)
	if err != nil || len(provider.requests) != 1 {
		t.Fatalf("initial run failed: %v", err)
	}
	if len(provider.requests[0].Messages) < 3 || !strings.Contains(provider.requests[0].Messages[1].Content, firstRule) {
		t.Fatal("first provider request lacks project instructions")
	}
	if _, err := os.Stat(filepath.Join(workspace, "should-not-exist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("document command ran without a tool proposal")
	}
	inspected, err := service.Inspect(context.Background(), workspace, state, configuration, initial.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var created domain.RunCreatedPayload
	if err := json.Unmarshal(inspected.Events[0].Payload, &created); err != nil || len(created.ProjectInstructionsRef) == 0 {
		t.Fatal("run identity did not bind the snapshot")
	}
	if err := os.WriteFile(name, []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if report, err := service.Doctor(context.Background(), workspace, state, configuration); err != nil || !report.ArtifactsOK {
		t.Fatalf("doctor scanned current invalid source: %v", err)
	}
	if _, err := service.Run(context.Background(), options); err == nil || len(provider.requests) != 1 {
		t.Fatal("invalid current rules reached a new model request")
	}
	if err := os.WriteFile(name, []byte("SECOND_PROJECT_RULE"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), options); err != nil || len(provider.requests) != 2 {
		t.Fatalf("new run did not refresh instructions: %v", err)
	}
	for _, message := range provider.requests[1].Messages {
		if strings.Contains(message.Content, "FIRST_PROJECT_RULE") {
			t.Fatal("new run inherited a previous snapshot")
		}
	}
	if !strings.Contains(provider.requests[1].Messages[1].Content, "SECOND_PROJECT_RULE") {
		t.Fatal("new run missed the changed project rules")
	}
}

// TestProjectInstructionsCorruptionStopsRecoveryBeforeEvents 快照缺失或篡改必须在审批恢复、诊断和检查入口失败关闭。
func TestProjectInstructionsCorruptionStopsRecoveryBeforeEvents(t *testing.T) {
	userProjectInstructionsTestPath(t)
	for _, mutation := range []string{"missing", "tampered"} {
		t.Run(mutation, func(t *testing.T) {
			workspace, state := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("FROZEN_RECOVERY_RULE"), 0600); err != nil {
				t.Fatal(err)
			}
			provider := &patchThenFinalProvider{}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
			configuration := withTestProvider(config.Defaults())
			initial, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: state, Config: configuration})
			var waiting *ports.WaitingApprovalError
			if !errors.As(err, &waiting) {
				t.Fatalf("run did not pause for approval: %v", err)
			}
			before, err := service.Inspect(context.Background(), workspace, state, configuration, initial.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			var created struct {
				ProjectInstructionsRef artifact.Ref `json:"project_instructions_ref"`
			}
			if err := json.Unmarshal(before.Events[0].Payload, &created); err != nil || created.ProjectInstructionsRef.SHA256 == "" {
				t.Fatal("missing project snapshot reference")
			}
			_, hash, err := workspaceFor(workspace, configuration)
			if err != nil {
				t.Fatal(err)
			}
			digest := strings.TrimPrefix(created.ProjectInstructionsRef.SHA256, "sha256:")
			name := filepath.Join(manifestArtifactRoot(state, hash), "sha256", digest[:2], digest)
			if mutation == "missing" {
				err = os.Remove(name)
			} else {
				err = os.WriteFile(name, []byte("tampered"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: state, Config: configuration, Approval: ports.Approval{Approve: true, Fingerprint: waiting.Fingerprint}}); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("recovery accepted a corrupt snapshot: %v", err)
			}
			if _, err := service.Doctor(context.Background(), workspace, state, configuration); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("doctor accepted a corrupt snapshot: %v", err)
			}
			if _, err := service.Inspect(context.Background(), workspace, state, configuration, initial.ID, true); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("inspect accepted a corrupt snapshot: %v", err)
			}
			journal, err := openJournal(state, hash)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			after, err := journal.Load(context.Background(), initial.ID)
			if err != nil || len(after) != len(before.Events) {
				t.Fatalf("rejected recovery appended new events: %v", err)
			}
		})
	}
}

// userProjectInstructionsTestPath 将平台用户配置根隔离到测试目录，禁止读取本机全局规约。
func userProjectInstructionsTestPath(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, root)
	}
	name, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.Dir(name), "AGENTS.md")
}

// TestGlobalProjectInstructionsService 验证平台路径、全局优先顺序、新运行刷新、诊断不重读及评测隔离。
func TestGlobalProjectInstructionsService(t *testing.T) {
	name := userProjectInstructionsTestPath(t)
	if err := os.WriteFile(name, []byte("GLOBAL_RULE_FIRST"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("PROJECT_RULE"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := &projectInstructionsCaptureProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	options := RunOptions{Task: "TASK_RULE", Workspace: workspace, StateDir: state, Config: configuration}
	initial, err := service.Run(context.Background(), options)
	if err != nil || len(provider.requests) != 1 {
		t.Fatalf("global run failed: %v", err)
	}
	messages := provider.requests[0].Messages
	if len(messages) < 4 || messages[1].Role != "user" || !strings.Contains(messages[1].Content, "GLOBAL_RULE_FIRST") || !strings.Contains(messages[2].Content, "PROJECT_RULE") || messages[3].Content != options.Task {
		t.Fatal("global/project/task guidance order was lost")
	}
	if strings.Contains(messages[1].Content, name) || !strings.Contains(messages[0].Content, "lower priority than all applicable project guidance") {
		t.Fatal("global source leaked an absolute path or missed its precedence")
	}
	if err := os.WriteFile(name, []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if report, err := service.Doctor(context.Background(), workspace, state, configuration); err != nil || !report.ArtifactsOK {
		t.Fatalf("doctor reread the current global file: %v", err)
	}
	if _, err := service.Inspect(context.Background(), workspace, state, configuration, initial.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), options); err == nil || len(provider.requests) != 1 {
		t.Fatal("invalid global instructions reached Provider")
	}
	options.DisableUserInstructions = true
	if _, err := service.Run(context.Background(), options); err != nil || len(provider.requests) != 2 || !strings.Contains(provider.requests[1].Messages[1].Content, "PROJECT_RULE") {
		t.Fatalf("isolated execution inherited invalid user instructions: %v", err)
	}
	if err := os.WriteFile(name, []byte("GLOBAL_RULE_SECOND"), 0600); err != nil {
		t.Fatal(err)
	}
	options.DisableUserInstructions = false
	if _, err := service.Run(context.Background(), options); err != nil || len(provider.requests) != 3 || !strings.Contains(provider.requests[2].Messages[1].Content, "GLOBAL_RULE_SECOND") {
		t.Fatalf("new run did not refresh global instructions: %v", err)
	}
}
