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
	"time"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/buildinfo"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/lock"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/provider"
	"github.com/fxbin/cairn/internal/tools"
	_ "modernc.org/sqlite"
)

// injectInvalidBuildIdentity 注入必然非法的 plain 构建事实，并在测试结束时恢复全局值。
func injectInvalidBuildIdentity(t *testing.T) {
	t.Helper()
	originalFlavor, originalVersion, originalDigest := buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest
	originalRevision, originalDirty := buildinfo.Revision, buildinfo.Dirty
	t.Cleanup(func() {
		buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest = originalFlavor, originalVersion, originalDigest
		buildinfo.Revision, buildinfo.Dirty = originalRevision, originalDirty
	})
	buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest = "plain", "unexpected", "sha256:unexpected"
}

// assertStateDirEmpty 断言入口失败发生在 Boot 创建日志、锁或制品目录之前。
func assertStateDirEmpty(t *testing.T, stateDir string) {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("state directory must remain empty, entries=%v", entries)
	}
}

// TestServiceRunRejectsInvalidBuildIdentityBeforePersistence 锁定非法编译期身份在
// Boot 前失败关闭，不得构造 Provider 或留下日志、锁和制品目录。
func TestServiceRunRejectsInvalidBuildIdentityBeforePersistence(t *testing.T) {
	injectInvalidBuildIdentity(t)
	providerCalled := false
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		providerCalled = true
		return finalProvider{}, nil
	}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, err := service.Run(context.Background(), RunOptions{Task: "answer", Workspace: workspace, StateDir: stateDir, Config: withTestProvider(config.Defaults())})
	if err == nil || !strings.Contains(err.Error(), "invalid current build identity") {
		t.Fatalf("run error = %v", err)
	}
	if providerCalled {
		t.Fatal("provider was constructed before build identity validation")
	}
	assertStateDirEmpty(t, stateDir)
}

// withTestProvider 为应用层测试配置补齐能力卡所需的模型与显式声明；
// 测试替身模型名不在内建目录，必须显式声明才能通过装配期失败关闭。
func withTestProvider(configuration config.Config) config.Config {
	if configuration.Provider.BaseURL == "" {
		configuration.Provider.BaseURL = "https://provider.test/v1"
	}
	if configuration.Provider.Model == "" {
		configuration.Provider.Model = "test-model"
	}
	configuration.Provider.Admission = config.ProviderAdmissionOptional
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	return configuration
}

// putTestFrozenRunConfig 写入符合当前 v2 合同的冻结配置，供手工事件夹具引用。
func putTestFrozenRunConfig(t *testing.T, stateDir, workspaceHash string, configuration config.Config) artifact.Ref {
	t.Helper()
	frozen, err := marshalFrozenRunConfig(configuration, configuration.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := openArtifactStore(stateDir, workspaceHash, configuration.Output)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := artifacts.Put(frozen, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	return reference
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

// admissionGuardProvider 记录准入失败前是否错误进入了模型流。
type admissionGuardProvider struct {
	calls int
}

func (provider *admissionGuardProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "unexpected model call"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

func TestConfiguredLoopPassesContextLimits(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Context.AutoCompact = false
	configuration.Context.MaxPromptTokens = 12_345
	configuration.Context.MaxSummaryBytes = 6_789
	configuration.Context.InventoryBudgetPercent = 35
	configuration.Context.ThresholdReservePercent = 15
	loop := configuredLoop(nil, nil, nil, &policy.Gate{}, nil, nil, configuration, nil)
	if loop.ContextAutoCompact || loop.ContextMaxPromptTokens != 12_345 || loop.ContextMaxSummaryBytes != 6_789 ||
		loop.ContextInventoryBudgetPercent != 35 || loop.ContextThresholdReservePercent != 15 {
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
	doctorReport, err := service.Doctor(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if doctorReport.ConfigValid || doctorReport.Ready {
		t.Fatalf("invalid docker sandbox must be reported as not ready: %+v", doctorReport)
	}
}

func (finalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// emitTestCompletion 生成满足显式能力卡的最小流终态：每次调用都回报 usage 与
// 协议 finish_reason，确保应用层测试覆盖真实的运行时能力约束。
func emitTestCompletion(emit func(ports.ModelEvent) error) error {
	if err := emit(ports.ModelEvent{Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done, StopReason: "stop"})
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
	return emitTestCompletion(emit)
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
		return emitTestCompletion(emit)
	}
	close(provider.entered)
	<-provider.release
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "resumed once"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
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
	// 零命令计划：允许 completed，但不得标记可信（ADR-0017）。
	if result.Result.State != string(domain.RunCompleted) || result.Result.Evidence.Trusted {
		t.Fatalf("zero-command completion must be untrusted: result=%#v", result.Result)
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
	var waiting *ports.WaitingApprovalError
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
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	aborted, err := service.Abort(context.Background(), AbortOptions{RunID: initial.ID, Reason: "operator stopped the task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || aborted.ID != initial.ID || aborted.Result == nil || aborted.Result.State != string(domain.RunFailed) {
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
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial = %#v, err = %v", initial, err)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: "sha256:not-the-pending-approval", Approve: true}}); err == nil {
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
	resumed, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
	if err != nil || resumed.Response != "patched and verified" || resumed.Result == nil || resumed.Result.State != string(domain.RunCompleted) {
		t.Fatalf("resumed = %#v, err = %v", resumed, err)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true}}); err == nil {
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
	var exceeded *ports.BudgetExceededError
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
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial = %#v, err = %v", initial, err)
	}
	resumed, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
	if err != nil || resumed.Response != "patched and verified" || resumed.Result == nil || resumed.Result.State != string(domain.RunCompleted) {
		t.Fatalf("resumed = %#v, err = %v", resumed, err)
	}
	if !resumed.Result.Evidence.Trusted {
		t.Fatalf("approved frozen verifier must have complete trusted evidence: %#v", resumed.Result.Evidence)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, initial.ID, true)
	if err != nil || inspected.Projection.Phase() != "completed" {
		t.Fatalf("inspect = %#v, %v", inspected, err)
	}
	if !inspected.Summary.CompletionTrusted {
		t.Fatalf("inspect trust differs from result evidence: %#v", inspected.Summary)
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
	configRef := putTestFrozenRunConfig(t, stateDir, hash, configuration)
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
		Task              string       `json:"task"`
		Workspace         string       `json:"workspace"`
		ConfigRef         artifact.Ref `json:"config_ref"`
		ConfigFingerprint string       `json:"config_fingerprint"`
	}{Task: "recover", Workspace: workspace, ConfigRef: configRef, ConfigFingerprint: configuration.Fingerprint()})})
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
	if err != nil || result.Response != "done" || result.Result == nil || result.Result.State != string(domain.RunCompleted) {
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

// TestProjectedRunOutcomeFailsClosedOnProjectionError 锁定执行成功后若事件无法
// 投影为统一合同，调用方必须收到状态损坏错误而不是伪成功的 nil Result。
func TestProjectedRunOutcomeFailsClosedOnProjectionError(t *testing.T) {
	stateDir := t.TempDir()
	const workspaceHash = "projection-error"
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	runID := domain.NewRunID()
	outcome, err := projectedRunOutcome(journal, stateDir, workspaceHash, runID, "done", nil)
	if err == nil || !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("projection error = %v", err)
	}
	if outcome.ID != runID || outcome.Response != "done" || outcome.Result != nil {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestNewRunIsRejectedWhileWorkspaceHasPendingApproval(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	first, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: withTestProvider(config.Defaults())})
	var waiting *ports.WaitingApprovalError
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
	configRef := putTestFrozenRunConfig(t, stateDir, hash, configuration)
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
		Task              string       `json:"task"`
		Workspace         string       `json:"workspace"`
		ConfigRef         artifact.Ref `json:"config_ref"`
		ConfigFingerprint string       `json:"config_fingerprint"`
	}{Task: "recover command", Workspace: workspace, ConfigRef: configRef, ConfigFingerprint: configuration.Fingerprint()})})
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
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	firstResult := make(chan error, 1)
	go func() {
		_, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
		firstResult <- err
	}()
	<-provider.entered
	_, secondErr := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
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
	configRef := putTestFrozenRunConfig(t, stateDir, hash, configuration)
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
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
	if err != nil || aborted.ID != runID || aborted.Result == nil || aborted.Result.State != string(domain.RunFailed) {
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
	var waiting *ports.WaitingApprovalError
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
	if _, err := journal.Append(context.Background(), events[len(events)-1].Seq, domain.EventDraft{RunID: initial.ID, WorkspaceHash: hash, Type: domain.EventRunCancelRequested, Payload: artifact.MustEncodeRedactedJSON(struct {
		Cause string `json:"cause"`
	}{Cause: "user_interrupt"})}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	aborted, err := service.Abort(context.Background(), AbortOptions{RunID: initial.ID, Reason: "finish interrupted cancellation", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || aborted.ID != initial.ID || aborted.Result == nil || aborted.Result.State != string(domain.RunCancelled) {
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

// TestLegacyFingerprintRunsInspectButRefuseResume 锁定旧指纹运行不再被解释为
// 当前合同：只读投影与 Doctor 报状态损坏，恢复则先由指纹漂移失败关闭。
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
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
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
	if _, err := service.Inspect(context.Background(), workspace, stateDir, configuration, runID, false); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("legacy inspect must fail closed with ErrStateCorrupt: %v", err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("legacy doctor must fail closed with ErrStateCorrupt: %v", err)
	}
	_, err = service.Resume(context.Background(), ResumeOptions{RunID: runID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if !errors.Is(err, ErrConfigDrift) {
		t.Fatalf("legacy resume must fail closed with ErrConfigDrift, got %v", err)
	}
}

// TestLegacyProviderCapabilityRunRefusesRecovery 锁定旧版直接序列化 Config 的
// 冻结制品在 Resume、Abort 和两类 Reconcile 前失败关闭，且不得追加事件或
// 进入模型流。
func TestLegacyProviderCapabilityRunRefusesRecovery(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "legacy-model"
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
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
		Task              string       `json:"task"`
		Workspace         string       `json:"workspace"`
		ConfigRef         artifact.Ref `json:"config_ref"`
		ConfigFingerprint string       `json:"config_fingerprint"`
	}{Task: "legacy capability", Workspace: workspace, ConfigRef: configRef, ConfigFingerprint: configuration.Fingerprint()})}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	provider := &admissionGuardProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: runID, Workspace: workspace, StateDir: stateDir, Config: configuration}); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("legacy frozen config must fail closed with ErrStateCorrupt: %v", err)
	}
	if _, err := service.Abort(context.Background(), AbortOptions{
		RunID: runID, Reason: "reject legacy frozen config",
		Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("legacy frozen config abort must fail closed with ErrStateCorrupt: %v", err)
	}
	turnID, operationID := domain.NewTurnID(), domain.NewOperationID()
	journal, err = openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	drafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"legacy-reconcile","tool_name":"apply_patch","arguments":{"path":"x.txt","mode":"create","content":"x"}}`)},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)},
		{RunID: runID, WorkspaceHash: hash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)},
	}
	for index, draft := range drafts {
		if _, err := journal.Append(context.Background(), uint64(index+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []string{"not_applied", "abort_run"} {
		if _, err := service.Reconcile(context.Background(), ReconcileOptions{
			RunID: runID, OperationID: operationID, Verdict: verdict, Note: "legacy config rejected",
			Workspace: workspace, StateDir: stateDir, Config: configuration,
		}); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("legacy frozen config reconcile %s must fail closed with ErrStateCorrupt: %v", verdict, err)
		}
	}
	if provider.calls != 0 {
		t.Fatalf("legacy recovery called provider %d times", provider.calls)
	}
	readOnly, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	events, err := readOnly.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 {
		t.Fatalf("failed recovery appended events: %d", len(events))
	}
}

// TestVerificationEnvironmentFlowsThroughConfiguredExecTool 锁定 2026-08-16
// AB Gate 回归的根因不再复发：Preset 工具子集必须携带已配置装配的 exec 工具，
// 验证命令声明的环境变量（经 env_allowlist 授权）能真实进入执行环境。
func TestVerificationEnvironmentFlowsThroughConfiguredExecTool(t *testing.T) {
	provider := &finalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
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

// TestServiceResumeEnforcesRequiredAdmission 锁定普通运行恢复入口仍经过
// required 准入门；拒绝发生在模型调用和任何新事件之前。
func TestServiceResumeEnforcesRequiredAdmission(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.Admission = config.ProviderAdmissionRequired
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	configRef := putTestFrozenRunConfig(t, stateDir, workspaceHash, configuration)
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{
		RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated,
		Payload: artifact.MustEncodeRedactedJSON(struct {
			Task              string       `json:"task"`
			Workspace         string       `json:"workspace"`
			ConfigRef         artifact.Ref `json:"config_ref"`
			ConfigFingerprint string       `json:"config_fingerprint"`
		}{Task: "resume requires admission", Workspace: workspace, ConfigRef: configRef, ConfigFingerprint: configuration.Fingerprint()}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	provider := &admissionGuardProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: runID, Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil || !strings.Contains(err.Error(), "admission gate") {
		t.Fatalf("resume without admission decision error = %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider stream calls=%d, want 0", provider.calls)
	}
	readOnly, err := openReadOnlyJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	events, err := readOnly.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("failed resume appended events: %d", len(events))
	}
}

// TestServiceReconcileEnforcesRequiredAdmission 锁定普通运行对账入口在写入
// 人工判定前重新准入，失败时既不调用模型，也不追加对账事件。
func TestServiceReconcileEnforcesRequiredAdmission(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.Admission = config.ProviderAdmissionRequired
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	configRef := putTestFrozenRunConfig(t, stateDir, workspaceHash, configuration)
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	drafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
			Task              string       `json:"task"`
			Workspace         string       `json:"workspace"`
			ConfigRef         artifact.Ref `json:"config_ref"`
			ConfigFingerprint string       `json:"config_fingerprint"`
		}{Task: "reconcile requires admission", Workspace: workspace, ConfigRef: configRef, ConfigFingerprint: configuration.Fingerprint()})},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventTurnOpened, TurnID: &turnID},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"call-admission","tool_name":"apply_patch","arguments":{"path":"x.txt","mode":"create","content":"x"}}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)},
	}
	for index, draft := range drafts {
		if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	provider := &admissionGuardProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	if _, err := service.Reconcile(context.Background(), ReconcileOptions{
		RunID: runID, OperationID: operationID, Verdict: "not_applied",
		Note: "operation was not applied", Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); err == nil || !strings.Contains(err.Error(), "admission gate") {
		t.Fatalf("reconcile without admission decision error = %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider stream calls=%d, want 0", provider.calls)
	}
	readOnly, err := openReadOnlyJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	events, err := readOnly.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(drafts) {
		t.Fatalf("failed reconcile appended events: %d", len(events))
	}
}

// TestEnforceAdmissionGateModes 锁定 v2 准入语义：optional 不拦截；
// required 在无决定、无证据时失败关闭，写入匹配决定与证据后放行。
func TestEnforceAdmissionGateModes(t *testing.T) {
	stateDir := t.TempDir()
	workspaceHash := "sha256:" + strings.Repeat("a", 64)
	base := config.Defaults()
	base.Provider.BaseURL = "https://api.example.com/v1"
	base.Provider.Model = "gate-model"
	base.Provider.APIKeyEnv = "AGENT_API_KEY"
	base.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true, StopReasons: []string{"stop"}}

	optional := base
	optional.Provider.Admission = config.ProviderAdmissionOptional
	if err := enforceAdmissionGate(stateDir, workspaceHash, optional); err != nil {
		t.Fatalf("optional must not block: %v", err)
	}
	required := base
	required.Provider.Admission = config.ProviderAdmissionRequired
	if err := enforceAdmissionGate(stateDir, workspaceHash, required); err == nil {
		t.Fatal("required with no decisions must fail closed")
	}

	profile, err := provider.ResolveResolvedProfile(provider.ResolvedFacts{
		Selection:     provider.SelectionFacts{ProviderID: contract.ProviderIDCustom, Model: "gate-model", BaseURL: "https://api.example.com/v1"},
		Capability:    &contract.ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true, StopReasons: []string{"stop"}},
		CredentialRef: "AGENT_API_KEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	requestFingerprint, err := profile.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	evidenceStore, err := provider.OpenEvidenceStore(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	evidence := provider.ProviderProbeEvidence{
		SchemaVersion: 1, Selection: profile.Selection, ProbeContractVersion: provider.CurrentProbeContractVersion,
		Observations: provider.EvidenceObservations{
			UsageStreamed: provider.ObservationObservedTrue, StopReasonsState: provider.StateObserved, StopReasons: []string{"stop"},
			ToolRoundtrip: provider.RoundtripSkipped, ModelRevisionState: provider.StateNotObserved,
		},
		Outcome: provider.EvidenceOutcomeCompleted, ProbedAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
	evidenceFingerprint, err := evidenceStore.Append(&evidence)
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := provider.OpenDecisionStore(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	decision := provider.ProviderAdmissionDecision{
		SchemaVersion: 1, EvidenceFingerprint: evidenceFingerprint,
		Binding:  provider.AdmissionBinding{RequestProfileFingerprint: requestFingerprint, ConfigFingerprint: required.Fingerprint()},
		Decision: provider.DecisionAdmitted, DecidedAt: time.Date(2026, 9, 3, 12, 5, 0, 0, time.UTC),
		Authority: provider.AuthorityMaintainer, Reason: "probe accepted",
	}
	if _, err := decisions.Append(&decision); err != nil {
		t.Fatal(err)
	}
	if err := enforceAdmissionGate(stateDir, workspaceHash, required); err != nil {
		t.Fatalf("admitted head with matching evidence must pass: %v", err)
	}
}

func TestServiceListRunsWithoutJournalReturnsEmptyWithoutCreatingState(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	items, err := (Service{}).ListRuns(context.Background(), workspace, stateDir, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v", items)
	}
	assertStateDirEmpty(t, stateDir)
}

func TestServiceListRunsProjectsSessionsAndSortsCurrentWorkspace(t *testing.T) {
	configuration := config.Defaults()
	workspace, otherWorkspace, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	tieTime := time.Date(2026, time.September, 13, 8, 0, 0, 0, time.UTC)
	newerTime := tieTime.Add(time.Hour)
	appendCreated := func(runID domain.RunID, occurredAt time.Time, task, sessionID string, prevRunID domain.RunID) {
		t.Helper()
		_, appendErr := journal.Append(context.Background(), 0, domain.EventDraft{
			RunID:         runID,
			WorkspaceHash: workspaceHash,
			OccurredAt:    occurredAt,
			Type:          domain.EventRunCreated,
			Payload: artifact.MustEncodeRedactedJSON(domain.RunCreatedPayload{
				Task:      task,
				Workspace: workspace,
				SessionID: sessionID,
				PrevRunID: prevRunID,
			}),
		})
		if appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	runA, runB, runNew := domain.RunID("run-a"), domain.RunID("run-b"), domain.RunID("run-new")
	appendCreated(runB, tieTime, "second tie", "session-tie", runA)
	appendCreated(runA, tieTime, "first tie", "session-tie", "")
	appendCreated(runNew, newerTime, "newest", "session-new", "")
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runA, WorkspaceHash: workspaceHash, OccurredAt: tieTime.Add(time.Minute), Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	_, otherHash, err := workspaceFor(otherWorkspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	otherJournal, err := openJournal(stateDir, otherHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherJournal.Append(context.Background(), 0, domain.EventDraft{RunID: domain.RunID("run-other"), WorkspaceHash: otherHash, OccurredAt: newerTime.Add(time.Hour), Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(domain.RunCreatedPayload{Task: "other", Workspace: otherWorkspace})}); err != nil {
		t.Fatal(err)
	}
	if err := otherJournal.Close(); err != nil {
		t.Fatal(err)
	}

	items, err := (Service{}).ListRuns(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %#v", items)
	}
	if got, want := []domain.RunID{items[0].ID, items[1].ID, items[2].ID}, []domain.RunID{runNew, runA, runB}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run order = %v, want %v", got, want)
	}
	if items[1].SessionID != "session-tie" || items[1].PrevRunID != "" || items[1].Task != "first tie" || items[1].State != domain.RunRunning || items[1].Phase != "running" {
		t.Fatalf("run-a summary = %#v", items[1])
	}
	if !items[1].CreatedAt.Equal(tieTime) || !items[1].UpdatedAt.Equal(tieTime.Add(time.Minute)) {
		t.Fatalf("run-a times = created %s updated %s", items[1].CreatedAt, items[1].UpdatedAt)
	}
	if items[2].PrevRunID != runA {
		t.Fatalf("run-b prev = %q", items[2].PrevRunID)
	}
}

func TestServiceListRunsFailsClosedForForeignWorkspaceEvent(t *testing.T) {
	configuration := config.Defaults()
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{
		RunID:         domain.RunID("run-foreign"),
		WorkspaceHash: "sha256:foreign",
		Type:          domain.EventRunCreated,
		Payload:       artifact.MustEncodeRedactedJSON(domain.RunCreatedPayload{Task: "foreign", Workspace: workspace}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{}).ListRuns(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("ListRuns error = %v", err)
	}
}

func TestServiceListRunsHonorsCancelledContextWithoutCreatingState(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Service{}).ListRuns(ctx, workspace, stateDir, config.Defaults()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListRuns error = %v", err)
	}
	assertStateDirEmpty(t, stateDir)
}

func TestServiceListRunsFailsClosedForInvalidTransition(t *testing.T) {
	configuration := config.Defaults()
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run-invalid-transition")
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(domain.RunCreatedPayload{Task: "invalid", Workspace: workspace})}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCompleted, Payload: artifact.MustEncodeRedactedJSON(domain.RunCompletedPayload{FinalResponse: "impossible"})}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{}).ListRuns(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("ListRuns error = %v", err)
	}
}

func TestServiceListRunsClassifiesInvalidPersistedEventAsCorrupt(t *testing.T) {
	configuration := config.Defaults()
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run-invalid-time")
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(domain.RunCreatedPayload{Task: "invalid time", Workspace: workspace})}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(stateDir, "workspaces", workspaceHash, "runs.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE events SET occurred_at = ? WHERE run_id = ?", "not-a-time", runID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{}).ListRuns(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("ListRuns error = %v", err)
	}
}

func TestServiceListRunsClassifiesInvalidPersistedEventTypeAsCorrupt(t *testing.T) {
	configuration := config.Defaults()
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run-invalid-sequence")
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(domain.RunCreatedPayload{Task: "invalid sequence", Workspace: workspace})}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(stateDir, "workspaces", workspaceHash, "runs.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE events SET seq = ? WHERE run_id = ?", "not-a-sequence", runID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{}).ListRuns(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("ListRuns error = %v", err)
	}
}

func TestServiceListRunsClassifiesInvalidDatabaseAsCorrupt(t *testing.T) {
	configuration := config.Defaults()
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "runs.db"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{}).ListRuns(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("ListRuns error = %v", err)
	}
}

// TestRunRejectsPriorRunWithoutFrozenConfig 锁定新运行续接前必须校验前驱的
// v2 冻结配置；缺少 config_ref 时不得调用模型或创建新的运行。
func TestRunRejectsPriorRunWithoutFrozenConfig(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	priorRunID := domain.NewRunID()
	drafts := []domain.EventDraft{
		{
			RunID: priorRunID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated,
			Payload: artifact.MustEncodeRedactedJSON(domain.RunCreatedPayload{
				Task: "legacy prior", Workspace: workspace, ConfigFingerprint: configuration.Fingerprint(), SessionID: "session-v2",
			}),
		},
		{RunID: priorRunID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted},
		{RunID: priorRunID, WorkspaceHash: workspaceHash, Type: domain.EventRunFailed, Payload: json.RawMessage(`{"error":"legacy contract"}`)},
	}
	for index, draft := range drafts {
		if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	provider := &admissionGuardProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	if _, err := service.Run(context.Background(), RunOptions{
		Task: "continue", Workspace: workspace, StateDir: stateDir, Config: configuration,
		SessionID: "session-v2", PrevRunID: priorRunID,
	}); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("run with legacy prior error = %v, want state corrupt", err)
	}
	if provider.calls != 0 {
		t.Fatalf("run with legacy prior called provider %d times", provider.calls)
	}
	items, err := service.ListRuns(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != priorRunID {
		t.Fatalf("failed continuation created a new run: %#v", items)
	}
}

// TestSessionContinuationRejectsLegacyAncestorConfig 锁定会话链选择与普通续链
// 都会校验全部祖先；即使直接前驱使用 v2，旧格式祖先也必须按状态损坏拒绝。
func TestSessionContinuationRejectsLegacyAncestorConfig(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := openArtifactStore(stateDir, workspaceHash, configuration.Output)
	if err != nil {
		t.Fatal(err)
	}
	legacyFrozen, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	legacyRef, err := artifacts.Put(artifact.RedactJSON(legacyFrozen), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	currentRef := putTestFrozenRunConfig(t, stateDir, workspaceHash, configuration)
	rootRunID, tailRunID := domain.NewRunID(), domain.NewRunID()
	sessionID := "session-with-legacy-root"
	appendTerminalRun := func(runID domain.RunID, previous domain.RunID, configRef artifact.Ref) {
		t.Helper()
		drafts := []domain.EventDraft{
			{
				RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated,
				Payload: artifact.MustEncodeRedactedJSON(struct {
					domain.RunCreatedPayload
					ConfigRef artifact.Ref `json:"config_ref"`
				}{
					RunCreatedPayload: domain.RunCreatedPayload{
						Task: "session member", Workspace: workspace, ConfigFingerprint: configuration.Fingerprint(),
						SessionID: sessionID, PrevRunID: previous,
					},
					ConfigRef: configRef,
				}),
			},
			{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted},
			{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunFailed, Payload: json.RawMessage(`{"error":"terminal fixture"}`)},
		}
		for index, draft := range drafts {
			if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendTerminalRun(rootRunID, "", legacyRef)
	appendTerminalRun(tailRunID, rootRunID, currentRef)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := (Service{}).ResolveSessionContinuation(context.Background(), workspace, stateDir, configuration, tailRunID); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("session with legacy ancestor error = %v, want state corrupt", err)
	}

	model := &admissionGuardProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return model, nil }}
	if _, err := service.Run(context.Background(), RunOptions{
		Task: "continue", Workspace: workspace, StateDir: stateDir, Config: configuration,
		SessionID: sessionID, PrevRunID: tailRunID, RequireSessionTail: false,
	}); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("run with legacy ancestor error = %v, want state corrupt", err)
	}
	if model.calls != 0 {
		t.Fatalf("run with legacy ancestor called provider %d times", model.calls)
	}
	items, err := service.ListRuns(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("failed continuation created a new run: %#v", items)
	}
}

// TestResumeAndDoctorRejectMissingPriorContextArtifact 锁定前情制品属于运行完整
// 性合同：引用目标缺失时 Resume 与 Doctor 都返回状态损坏类别。
func TestResumeAndDoctorRejectMissingPriorContextArtifact(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	initial, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration, PriorContext: "必须持久恢复的前情"})
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, initial.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		PriorContextRef *artifact.Ref `json:"prior_context_ref"`
	}
	if err := json.Unmarshal(inspected.Events[0].Payload, &payload); err != nil || payload.PriorContextRef == nil {
		t.Fatalf("run_created 缺少前情引用: payload=%s err=%v", inspected.Events[0].Payload, err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(payload.PriorContextRef.SHA256, "sha256:")
	path := filepath.Join(stateDir, "workspaces", hash, "artifacts", "sha256", digest[:2], digest)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration}); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("resume error=%v, want state corrupt", err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("doctor error=%v, want state corrupt", err)
	}
}
