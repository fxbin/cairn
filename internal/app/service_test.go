package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/lock"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
	_ "modernc.org/sqlite"
)

// withTestProvider 为应用层测试配置补齐能力卡所需的模型与显式声明；
// 测试替身模型名不在内建目录，必须显式声明才能通过装配期失败关闭。
func withTestProvider(configuration config.Config) config.Config {
	if configuration.Provider.Model == "" {
		configuration.Provider.Model = "test-model"
	}
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	return configuration
}

// mustTestCard 解析测试配置的能力卡，供 manifest 投影断言复用。
func mustTestCard(t *testing.T, configuration config.Config) contract.ProviderCapabilityCard {
	t.Helper()
	card, err := resolveProviderCard(withTestProvider(configuration))
	if err != nil {
		t.Fatal(err)
	}
	return card
}

type finalProvider struct{}

func TestConfiguredLoopPassesContextLimits(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Context.AutoCompact = false
	configuration.Context.MaxPromptTokens = 12_345
	configuration.Context.MaxSummaryBytes = 6_789
	loop := configuredLoop(nil, nil, nil, &policy.Gate{}, nil, nil, configuration, nil)
	if loop.ContextAutoCompact || loop.ContextMaxPromptTokens != 12_345 || loop.ContextMaxSummaryBytes != 6_789 {
		t.Fatalf("loop context = %#v", loop)
	}
}

func TestConfiguredLoopPassesAutomaticVerificationAfterPatch(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Verification.AutoAfterPatch = true
	loop := configuredLoop(nil, nil, nil, &policy.Gate{}, nil, nil, configuration, nil)
	if !loop.VerificationAutoAfterPatch {
		t.Fatalf("loop verification configuration = %#v", loop)
	}
}

func TestExecToolForAppliesSandboxConfiguration(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	configuration := withTestProvider(config.Defaults())
	configuration.Sandbox.Backend = "docker"
	configuration.Sandbox.DockerImage = "alpine:3.21"
	configuration.Sandbox.MaxPIDs = 48
	tool, err := execToolFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Backend != tools.ExecBackendDocker || tool.DockerImage != "alpine:3.21" || tool.DockerMaxPIDs != 48 {
		t.Fatalf("exec tool = %#v", tool)
	}
}

func TestServiceRejectsInvalidDockerSandboxBeforeProviderOrDoctorStateAccess(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Sandbox.Backend = "docker"
	providerCalled := false
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		providerCalled = true
		return finalProvider{}, nil
	}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	if _, err := service.Run(context.Background(), RunOptions{Task: "answer", Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil || !strings.Contains(err.Error(), "sandbox.docker_image") {
		t.Fatalf("run error = %v", err)
	}
	if providerCalled {
		t.Fatal("provider was constructed before sandbox validation")
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: domain.NewRunID(), Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil || !strings.Contains(err.Error(), "sandbox.docker_image") {
		t.Fatalf("resume error = %v", err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); err == nil || !strings.Contains(err.Error(), "sandbox.docker_image") {
		t.Fatalf("doctor error = %v", err)
	}
}

func (finalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done})
}

type patchThenFinalProvider struct{ calls int }

func (provider *patchThenFinalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if provider.calls == 1 {
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"created.txt","mode":"create","content":"created"}`)}}); err != nil {
			return err
		}
	} else if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "patched and verified"}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done})
}

type blockingResumeProvider struct {
	calls   int
	entered chan struct{}
	release <-chan struct{}
}

type waitForDeadlineProvider struct{ calls int }

func (provider *waitForDeadlineProvider) Stream(ctx context.Context, _ ports.ModelRequest, _ func(ports.ModelEvent) error) error {
	provider.calls++
	<-ctx.Done()
	return ctx.Err()
}

func (provider *blockingResumeProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if provider.calls == 1 {
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-lock", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"lock.txt","mode":"create","content":"locked"}`)}}); err != nil {
			return err
		}
		return emit(ports.ModelEvent{Kind: ports.Done})
	}
	close(provider.entered)
	<-provider.release
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "resumed once"}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done})
}

func TestServiceRunThenInspect(t *testing.T) {
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	configuration := withTestProvider(config.Defaults())
	workspace := t.TempDir()
	stateDir := t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "answer", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "done" {
		t.Fatalf("result = %#v", result)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil || inspected.Projection.ID != result.ID || len(inspected.Events) == 0 {
		t.Fatalf("inspect = %#v, %v", inspected, err)
	}
	var payload struct {
		ConfigRef         *artifact.Ref         `json:"config_ref"`
		ConfigFingerprint string                `json:"config_fingerprint"`
		Build             *domain.BuildIdentity `json:"build"`
	}
	if err := json.Unmarshal(inspected.Events[0].Payload, &payload); err != nil || payload.ConfigRef == nil || payload.ConfigRef.SHA256 == "" {
		t.Fatalf("run_created did not freeze a config artifact: payload=%s err=%v", inspected.Events[0].Payload, err)
	}
	if payload.ConfigFingerprint != configuration.Fingerprint() || payload.Build == nil || payload.Build.Flavor != "plain" || payload.Build.Fingerprint == "" {
		t.Fatalf("run_created did not freeze identity: payload=%s", inspected.Events[0].Payload)
	}
	if inspected.Projection.ConfigFingerprint != payload.ConfigFingerprint || inspected.Projection.Build == nil || *inspected.Projection.Build != *payload.Build {
		t.Fatalf("run identity did not replay: projection=%+v payload=%+v", inspected.Projection, payload)
	}
}

// TestServiceRunProjectsUnifiedContractResult 锁定普通 Run 的 V1.1 产品合同
// 投影：Run 返回的 contract.Result 与事后 Inspect 的 manifest 来自同一
// 构造器，字段一致且不泄漏任务正文或本地路径。
func TestServiceRunProjectsUnifiedContractResult(t *testing.T) {
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "test-model"
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "answer the question", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result == nil {
		t.Fatal("run must project a unified contract result")
	}
	manifest := result.Result.Manifest
	if manifest.RunID != result.ID || manifest.Task.Kind != contract.TaskKindFreeform || manifest.Task.PromptDigest == "" {
		t.Fatalf("manifest=%#v", manifest)
	}
	if manifest.ConfigFingerprint != configuration.Fingerprint() || manifest.Provider.Kind != contract.ProviderKindOpenAICompatible || manifest.Provider.BaseURL != "https://example.invalid/v1" || manifest.Provider.Model != "test-model" {
		t.Fatalf("manifest identity=%#v", manifest)
	}
	if manifest.Build.Fingerprint == "" || manifest.Preset != config.PresetCoding {
		t.Fatalf("manifest build/preset=%#v", manifest)
	}
	if result.Result.State != string(domain.RunCompleted) || !result.Result.Evidence.Trusted {
		t.Fatalf("result=%#v", result.Result)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil || inspected.Manifest == nil {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	if !reflect.DeepEqual(*inspected.Manifest, manifest) {
		t.Fatalf("inspect manifest drifted from run projection:\ninspect=%#v\nrun=%#v", *inspected.Manifest, manifest)
	}
	encoded, err := json.Marshal(result.Result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "answer the question") || strings.Contains(string(encoded), workspace) || strings.Contains(string(encoded), stateDir) {
		t.Fatalf("contract result leaked prompt or local paths: %s", encoded)
	}
}

func TestProviderSetupFailureDoesNotLeaveAnActiveRun(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	failing := Service{NewProvider: func(config.Config) (ports.Provider, error) { return nil, errors.New("provider unavailable") }}
	if _, err := failing.Run(context.Background(), RunOptions{Task: "answer", Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil {
		t.Fatal("provider setup unexpectedly succeeded")
	}
	working := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	result, err := working.Run(context.Background(), RunOptions{Task: "answer", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || result.Response != "done" {
		t.Fatalf("second run = %#v, %v", result, err)
	}
}

func TestServiceRequiresSuccessfulVerificationAfterPatch(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write = "allow"
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	workspace := t.TempDir()
	stateDir := t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "created.txt"))
	if err != nil || string(content) != "created" {
		t.Fatalf("content = %q, %v", content, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil || inspected.Projection.State != "completed" || !inspected.Projection.VerificationSucceeded || inspected.Projection.LastVerificationSeq <= inspected.Projection.LastMutationSeq {
		t.Fatalf("inspect = %#v, %v", inspected, err)
	}
	var reference *artifact.Ref
	for _, event := range inspected.Events {
		if event.Type != domain.EventOperationSucceeded {
			continue
		}
		var payload struct {
			ResultRef *artifact.Ref `json:"result_ref"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ResultRef != nil {
			reference = payload.ResultRef
			break
		}
	}
	if reference == nil {
		t.Fatalf("operation result event has no artifact reference: %#v", inspected.Events)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	artifactPath := filepath.Join(stateDir, "workspaces", hash, "artifacts", "sha256", digest[:2], digest)
	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("referenced artifact is missing: %v", err)
	}
}

func TestServiceStopsBeforeUnapprovedPatch(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	workspace := t.TempDir()
	stateDir := t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: withTestProvider(config.Defaults())})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) || result.ID == "" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "created.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("unapproved patch changed workspace: %v", statErr)
	}
}

func TestServiceAbortClosesPendingApprovalWithoutStartingOperation(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	initial, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	aborted, err := service.Abort(context.Background(), AbortOptions{RunID: initial.ID, Reason: "operator stopped the task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || aborted.ID != initial.ID {
		t.Fatalf("abort=%#v err=%v", aborted, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, initial.ID, true)
	if err != nil || inspected.Projection.State != domain.RunFailed || inspected.Projection.PendingApproval != nil {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	started, systemDenied := false, false
	for _, event := range inspected.Events {
		started = started || event.Type == domain.EventOperationStarted
		systemDenied = systemDenied || event.Type == domain.EventApprovalDenied && bytes.Contains(event.Payload, []byte(`"actor":"system"`))
	}
	if started || !systemDenied {
		t.Fatalf("abort did not close approval safely: %#v", inspected.Events)
	}
}

func TestServiceResumeConsumesOneMatchingApproval(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	workspace := t.TempDir()
	stateDir := t.TempDir()
	initial, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial = %#v, err = %v", initial, err)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: agent.Approval{Fingerprint: "sha256:not-the-pending-approval", Approve: true}}); err == nil {
		t.Fatal("mismatched approval unexpectedly succeeded")
	}
	beforeApproval, err := service.Inspect(context.Background(), workspace, stateDir, configuration, initial.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range beforeApproval.Events {
		if event.Type == domain.EventRunResumed {
			t.Fatalf("mismatched approval appended run_resumed: %#v", beforeApproval.Events)
		}
	}
	resumed, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: agent.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
	if err != nil || resumed.Response != "patched and verified" {
		t.Fatalf("resumed = %#v, err = %v", resumed, err)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: agent.Approval{Fingerprint: waiting.Fingerprint, Approve: true}}); err == nil {
		t.Fatal("duplicate approval unexpectedly succeeded")
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, initial.ID, true)
	if err != nil || inspected.Projection.WorkspaceObservation == "" {
		t.Fatalf("resume observation missing: %#v, %v", inspected, err)
	}
	resumeEvents, grantedAt, resumedAt, policyAllowAt := 0, -1, -1, -1
	for index, event := range inspected.Events {
		if event.Type == domain.EventRunResumed {
			resumeEvents++
			resumedAt = index
		}
		if event.Type == domain.EventApprovalGranted {
			grantedAt = index
		}
		if event.Type == domain.EventPolicyDecided && bytes.Contains(event.Payload, []byte(`"effect":"allow"`)) {
			policyAllowAt = index
		}
	}
	if resumeEvents != 1 {
		t.Fatalf("run_resumed events = %d", resumeEvents)
	}
	if grantedAt < 0 || resumedAt <= grantedAt || policyAllowAt <= resumedAt {
		t.Fatalf("resume ordering must be approval_granted -> run_resumed -> policy_decided(allow): %#v", inspected.Events)
	}
}

func TestServicePersistsBudgetFailureWhenWallTimeExpires(t *testing.T) {
	provider := &waitForDeadlineProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	// 预算需覆盖 race/慢盘下的 Boot 装配耗时，保证断言的"过期发生在模型调用
	// 期间"稳定成立；provider 阻塞到截止再返回，测试时长受此值控制。
	configuration.Budget.MaxWallTime = "2s"
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "wait", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var exceeded *agent.BudgetExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != "wall_time" || result.ID == "" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls=%d", provider.calls)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil || inspected.Projection.State != domain.RunFailed {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	seenBudgetStop := false
	for _, event := range inspected.Events {
		if event.Type == domain.EventRunCancelRequested && bytes.Contains(event.Payload, []byte(`"budget_exhausted"`)) {
			seenBudgetStop = true
		}
	}
	if !seenBudgetStop {
		t.Fatalf("missing budget stop event: %#v", inspected.Events)
	}
}

func TestServiceResumeContinuesTheSameVerificationCommandAfterApproval(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	initial, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial = %#v, err = %v", initial, err)
	}
	resumed, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: agent.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
	if err != nil || resumed.Response != "patched and verified" {
		t.Fatalf("resumed = %#v, err = %v", resumed, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, initial.ID, true)
	if err != nil || inspected.Projection.Phase() != "completed" {
		t.Fatalf("inspect = %#v, %v", inspected, err)
	}
	proposed, started := 0, 0
	var startedPlan, proposedPlan *artifact.Ref
	for _, event := range inspected.Events {
		if event.Type == domain.EventVerificationStarted {
			var payload struct {
				PlanRef *artifact.Ref `json:"plan_ref"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				startedPlan = payload.PlanRef
			}
		}
		if event.Type == domain.EventVerificationCommandProposed {
			proposed++
			var payload struct {
				PlanRef *artifact.Ref `json:"plan_ref"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				proposedPlan = payload.PlanRef
			}
		}
		if event.Type == domain.EventOperationStarted && bytes.Contains(event.Payload, []byte(`"exec_command"`)) {
			started++
		}
	}
	if proposed != 1 || started != 1 {
		t.Fatalf("verification proposal was regenerated or not executed exactly once: proposed=%d started=%d", proposed, started)
	}
	if startedPlan == nil || proposedPlan == nil || startedPlan.SHA256 != proposedPlan.SHA256 {
		t.Fatalf("verification command was not bound to its frozen plan: started=%#v proposed=%#v", startedPlan, proposedPlan)
	}
}

func TestServiceReconcilesDanglingWorkspaceOperationWithoutRetry(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	workspace := t.TempDir()
	stateDir := t.TempDir()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: encode(struct {
		Task              string `json:"task"`
		Workspace         string `json:"workspace"`
		ConfigFingerprint string `json:"config_fingerprint"`
	}{Task: "recover", Workspace: workspace, ConfigFingerprint: configuration.Fingerprint()})})
	appendEvent(1, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunStarted})
	appendEvent(2, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventTurnOpened, TurnID: &turnID})
	appendEvent(3, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"call-recover","tool_name":"apply_patch","arguments":{"path":"x.txt","mode":"create","content":"x"}}`)})
	appendEvent(4, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)})
	appendEvent(5, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)})
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	if _, err := service.Reconcile(context.Background(), ReconcileOptions{RunID: runID, OperationID: operationID, Verdict: "not_applied", Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil {
		t.Fatal("reconciliation without an audit note unexpectedly succeeded")
	}
	result, err := service.Reconcile(context.Background(), ReconcileOptions{RunID: runID, OperationID: operationID, Verdict: "not_applied", Note: "crash occurred before rename", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || result.Response != "done" {
		t.Fatalf("reconcile = %#v, %v", result, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, runID, true)
	if err != nil || inspected.Projection.Phase() != "completed" {
		t.Fatalf("inspect = %#v, %v", inspected, err)
	}
	for _, event := range inspected.Events {
		if event.Type == domain.EventOperationStarted && event.Seq > 6 && bytes.Contains(event.Payload, []byte(`"apply_patch"`)) {
			t.Fatalf("reconciliation retried operation: %#v", event)
		}
	}
}

func TestNewRunIsRejectedWhileWorkspaceHasPendingApproval(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	first, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: withTestProvider(config.Defaults())})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) || first.ID == "" {
		t.Fatalf("first run = %#v, %v", first, err)
	}
	_, err = service.Run(context.Background(), RunOptions{Task: "another", Workspace: workspace, StateDir: stateDir, Config: withTestProvider(config.Defaults())})
	if !errors.Is(err, ErrActiveRun) {
		t.Fatalf("expected active run rejection, got %v", err)
	}
}

func TestDoctorDetectsHashChainTamperingWithoutRepairingDatabase(t *testing.T) {
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "answer", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(stateDir, "workspaces", hash, "runs.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE events SET payload_json = ? WHERE run_id = ? AND seq = 1", []byte(`{"tampered":true}`), result.ID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); err == nil {
		t.Fatal("doctor accepted a tampered journal")
	}
	if _, err := os.Stat(databasePath); err != nil {
		t.Fatalf("doctor replaced or removed original database: %v", err)
	}
}

// TestDoctorDoesNotCreateMissingState 锁定 doctor 的只读合同与首次运行前
// preflight 语义：journal 不存在时报告 absent 而不是失败，且诊断全程绝不
// 创建任何状态文件。
func TestDoctorDoesNotCreateMissingState(t *testing.T) {
	service := Service{}
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(stateDir, "workspaces", hash, "runs.db")
	report, err := service.Doctor(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatalf("doctor must support fresh-workspace preflight: %v", err)
	}
	if report.JournalState != "absent" {
		t.Fatalf("journal state = %q, want absent", report.JournalState)
	}
	if _, err := os.Stat(databasePath); !os.IsNotExist(err) {
		t.Fatalf("doctor created state while diagnosing missing database: %v", err)
	}
}

func TestDoctorRejectsMissingArtifactReferencedByAnEvent(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write, configuration.Policy.Exec = "allow", "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var reference *artifact.Ref
	for _, event := range inspected.Events {
		var payload struct {
			ResultRef *artifact.Ref `json:"result_ref"`
		}
		if event.Type == domain.EventOperationSucceeded && json.Unmarshal(event.Payload, &payload) == nil && payload.ResultRef != nil {
			reference = payload.ResultRef
			break
		}
	}
	if reference == nil {
		t.Fatal("test run did not produce an artifact reference")
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	artifactPath := filepath.Join(stateDir, "workspaces", hash, "artifacts", "sha256", digest[:2], digest)
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("doctor accepted a missing referenced artifact: %v", err)
	}
}

// TestEvidenceRejectsMissingOrTamperedFrozenConfigArtifact 锁定 Evidence 的
// 信任根：run_created 一旦声明 config_ref，该制品缺失或摘要不匹配必须以
// ErrStateCorrupt 失败关闭，不能退化为一份看似合法的空配置合同。
func TestEvidenceRejectsMissingOrTamperedFrozenConfigArtifact(t *testing.T) {
	for _, mutation := range []struct {
		name  string
		apply func(t *testing.T, path string)
	}{
		{name: "missing", apply: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tampered", apply: func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(`{"provider":{"model":"tampered"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
			configuration := withTestProvider(config.Defaults())
			workspace, stateDir := t.TempDir(), t.TempDir()
			result, err := service.Run(context.Background(), RunOptions{Task: "frozen config evidence", Workspace: workspace, StateDir: stateDir, Config: configuration})
			if err != nil {
				t.Fatal(err)
			}
			inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			var reference *artifact.Ref
			for _, event := range inspected.Events {
				if event.Type != domain.EventRunCreated {
					continue
				}
				var payload struct {
					ConfigRef *artifact.Ref `json:"config_ref"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err == nil {
					reference = payload.ConfigRef
				}
			}
			if reference == nil {
				t.Fatal("run_created did not retain frozen config reference")
			}
			digest := strings.TrimPrefix(reference.SHA256, "sha256:")
			path := filepath.Join(stateDir, "workspaces", inspected.Projection.WorkspaceHash, "artifacts", "sha256", digest[:2], digest)
			mutation.apply(t, path)
			if _, err := service.Evidence(context.Background(), workspace, stateDir, configuration, result.ID); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("evidence accepted %s frozen config artifact: %v", mutation.name, err)
			}
		})
	}
}

func TestProcessReconciliationRequiresAuditableEvidence(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: encode(struct {
		Task              string `json:"task"`
		Workspace         string `json:"workspace"`
		ConfigFingerprint string `json:"config_fingerprint"`
	}{Task: "recover command", Workspace: workspace, ConfigFingerprint: configuration.Fingerprint()})})
	appendEvent(1, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunStarted})
	appendEvent(2, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventTurnOpened, TurnID: &turnID})
	appendEvent(3, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"command-recover","tool_name":"exec_command","arguments":{"argv":["/usr/bin/true"]}}`)})
	appendEvent(4, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)})
	appendEvent(5, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"exec_command","side_effect_class":"process","mutation_barrier":true}`)})
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	// 组合根在参数与状态校验前完成纯构造装配；注入惰性 Provider 使本用例聚焦
	// 证据缺失的拒绝语义（Provider 本身不应被调用）。
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	_, err = service.Reconcile(context.Background(), ReconcileOptions{RunID: runID, OperationID: operationID, Verdict: "applied_failure", Note: "process outcome was checked", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err == nil || !strings.Contains(err.Error(), "requires evidence") {
		t.Fatalf("process reconciliation accepted no evidence: %v", err)
	}
}

func TestConcurrentResumeAllowsExactlyOneWorkspaceLockHolder(t *testing.T) {
	release := make(chan struct{})
	provider := &blockingResumeProvider{entered: make(chan struct{}), release: release}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	initial, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	firstResult := make(chan error, 1)
	go func() {
		_, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: agent.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
		firstResult <- err
	}()
	<-provider.entered
	_, secondErr := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: agent.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
	if !errors.Is(secondErr, lock.ErrHeld) {
		close(release)
		t.Fatalf("second resume did not lose workspace lock: %v", secondErr)
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first resume failed: %v", err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls=%d; duplicate resume reached the model", provider.calls)
	}
}

// TestServiceAbortReleasesCreatedRun 锁定 created 状态（进程在 run_created 与
// run_started 之间崩溃）的逃生门：新 run 被阻塞并提示 abort，Abort 补写 run_failed
// 后工作区恢复可用。
func TestServiceAbortReleasesCreatedRun(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := openArtifactStore(stateDir, hash, configuration.Output)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	configRef, err := artifacts.Put(artifact.RedactJSON(frozen), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: encode(struct {
		Task              string       `json:"task"`
		Workspace         string       `json:"workspace"`
		ConfigRef         artifact.Ref `json:"config_ref"`
		ConfigFingerprint string       `json:"config_fingerprint"`
	}{Task: "interrupted", Workspace: workspace, ConfigRef: configRef, ConfigFingerprint: configuration.Fingerprint()})}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), RunOptions{Task: "blocked", Workspace: workspace, StateDir: stateDir, Config: configuration}); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("run against created-state run err = %v", err)
	}
	aborted, err := service.Abort(context.Background(), AbortOptions{RunID: runID, Reason: "release stuck workspace", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || aborted.ID != runID {
		t.Fatalf("abort=%#v err=%v", aborted, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, runID, true)
	if err != nil || inspected.Projection.State != domain.RunFailed {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	configuration.Policy.Write = "allow"
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	if _, err := service.Run(context.Background(), RunOptions{Task: "fresh", Workspace: workspace, StateDir: stateDir, Config: configuration}); err != nil {
		t.Fatalf("workspace still blocked after abort: %v", err)
	}
}

// TestServiceAbortCompletesCancellingRun 锁定取消序列中途崩溃（cancelling 且挂起
// 审批与打开 turn）的逃生门：Abort 补写 approval_denied、turn_closed 与
// run_cancelled 完成既有取消意图，工作区恢复可用。
func TestServiceAbortCompletesCancellingRun(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	initial, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(context.Background(), initial.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), events[len(events)-1].Seq, domain.EventDraft{RunID: initial.ID, WorkspaceHash: hash, Type: domain.EventRunCancelRequested, Payload: encode(struct {
		Cause string `json:"cause"`
	}{Cause: "user_interrupt"})}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	aborted, err := service.Abort(context.Background(), AbortOptions{RunID: initial.ID, Reason: "finish interrupted cancellation", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || aborted.ID != initial.ID {
		t.Fatalf("abort=%#v err=%v", aborted, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, initial.ID, true)
	if err != nil || inspected.Projection.State != domain.RunCancelled || inspected.Projection.PendingApproval != nil {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	denied, closed, cancelled := false, false, false
	for _, event := range inspected.Events {
		denied = denied || event.Type == domain.EventApprovalDenied
		closed = closed || event.Type == domain.EventTurnClosed
		cancelled = cancelled || event.Type == domain.EventRunCancelled
	}
	if !denied || !closed || !cancelled {
		t.Fatalf("abort sequence incomplete: denied=%v closed=%v cancelled=%v", denied, closed, cancelled)
	}
	configuration.Policy.Write = "allow"
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	if _, err := service.Run(context.Background(), RunOptions{Task: "fresh", Workspace: workspace, StateDir: stateDir, Config: configuration}); err != nil {
		t.Fatalf("workspace still blocked after abort: %v", err)
	}
}

// TestInspectDetectsMissingVerificationPlanArtifact 锁定验证计划制品参与完整性
// 校验：审批绑定的信任根被删除后，inspect 必须报告状态损坏而不是静默通过。
func TestInspectDetectsMissingVerificationPlanArtifact(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write = "allow"
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	workspace := t.TempDir()
	stateDir := t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var planPath string
	for _, event := range inspected.Events {
		var payload struct {
			PlanRef *artifact.Ref `json:"plan_ref"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.PlanRef == nil {
			continue
		}
		digest := strings.TrimPrefix(payload.PlanRef.SHA256, "sha256:")
		planPath = filepath.Join(stateDir, "workspaces", inspected.Projection.WorkspaceHash, "artifacts", "sha256", digest[:2], digest)
		break
	}
	if planPath == "" {
		t.Fatalf("no verification plan artifact referenced: %#v", inspected.Events)
	}
	if err := os.Remove(planPath); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, false); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("inspect after plan artifact removal err = %v", err)
	}
}

// TestLegacyFingerprintRunsInspectButRefuseResume 锁定 AB Gate 兼容矩阵的关键
// 行：指纹算法 v2（eadbe9c）之前的在途 run，inspect/doctor 照常工作，resume
// 以 ErrConfigDrift 失败关闭——一次性失效是设计决策并已公开记录。
func TestLegacyFingerprintRunsInspectButRefuseResume(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: encode(struct {
		Task              string `json:"task"`
		Workspace         string `json:"workspace"`
		ConfigFingerprint string `json:"config_fingerprint"`
	}{Task: "legacy", Workspace: workspace, ConfigFingerprint: "sha256:0000000000000000000000000000000000000000000000000000000000000000"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	if _, err := service.Inspect(context.Background(), workspace, stateDir, configuration, runID, false); err != nil {
		t.Fatalf("legacy inspect must work: %v", err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); err != nil {
		t.Fatalf("legacy doctor must work: %v", err)
	}
	_, err = service.Resume(context.Background(), ResumeOptions{RunID: runID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if !errors.Is(err, ErrConfigDrift) {
		t.Fatalf("legacy resume must fail closed with ErrConfigDrift, got %v", err)
	}
}

// TestVerificationEnvironmentFlowsThroughConfiguredExecTool 锁定 2026-08-16
// AB Gate 回归的根因不再复发：Preset 工具子集必须携带已配置装配的 exec 工具，
// 验证命令声明的环境变量（经 env_allowlist 授权）能真实进入执行环境。
func TestVerificationEnvironmentFlowsThroughConfiguredExecTool(t *testing.T) {
	provider := &finalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.EnvAllow = append(configuration.Policy.EnvAllow, "CAIRN_TEST_ENV")
	configuration.Verification.Commands = []config.VerificationCommand{{
		Argv: []string{"/bin/sh", "-c", `[ "$CAIRN_TEST_ENV" = deployed ]`},
		Env:  map[string]string{"CAIRN_TEST_ENV": "deployed"},
	}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: "env gate", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatalf("verification env was not propagated through the registry: %v", err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, false)
	if err != nil || !inspected.Projection.VerificationSucceeded {
		t.Fatalf("inspect = %+v err = %v", inspected.Projection, err)
	}
}
