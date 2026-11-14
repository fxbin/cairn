package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// TestMinimalBootExposesOnlyBasicPorts 校验真实 Boot 的工具声明和注入端口，
// 具体注册表的高级方法不能透过最小视图被 Core 识别。
func TestMinimalBootExposesOnlyBasicPorts(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Preset = config.PresetMinimal
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	boot, err := service.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	want := []string{"list_files", "read_file", "search_text", "apply_patch", "exec_command"}
	if got := boot.Registry.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools=%v want=%v", got, want)
	}
	if boot.MCP != nil || boot.Loop.Delegator != nil || boot.Loop.AsyncChildren != nil || len(boot.Loop.Verification) != 0 || len(boot.Loop.Hooks) != 0 {
		t.Fatal("minimal boot created an optional capability")
	}
	if _, exposed := boot.Loop.Tools.(interface {
		ActiveSkills() []contract.SkillContent
	}); exposed {
		t.Fatal("minimal registry exposed skill methods")
	}
	if _, exposed := boot.Loop.Tools.(interface{ MCPNeedsQuiescence() bool }); exposed {
		t.Fatal("minimal registry exposed MCP methods")
	}
	if boot.Manifest().ConfigFingerprint != configuration.Fingerprint() {
		t.Fatal("minimal boot lost its frozen configuration identity")
	}
	doctor, err := service.Doctor(context.Background(), workspace, stateDir, configuration)
	if err != nil || !reflect.DeepEqual(boot.Manifest(), doctor.Capabilities) {
		t.Fatalf("minimal doctor and execution manifests diverged: %v", err)
	}
	frozen, err := FrozenToolRegistryFingerprint(config.PresetMinimal)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := boot.Registry.Fingerprint()
	if err != nil || frozen != actual {
		t.Fatalf("minimal frozen registry diverged: %v", err)
	}

}

// TestMinimalEntryPointsRejectConflictsBeforeResources 覆盖正常运行与三种恢复
// 入口的提前校验；冲突配置不能启动 Provider 或抢占工作区资源。
func TestMinimalEntryPointsRejectConflictsBeforeResources(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Preset = config.PresetMinimal
	configuration.Telemetry.Enabled = true
	providerCalls := 0
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		providerCalls++
		return finalProvider{}, nil
	}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	ctx := context.Background()
	checks := []func() error{
		func() error {
			_, err := service.Run(ctx, RunOptions{Task: "x", Workspace: workspace, StateDir: stateDir, Config: configuration})
			return err
		},
		func() error {
			_, err := service.Resume(ctx, ResumeOptions{Workspace: workspace, StateDir: stateDir, Config: configuration})
			return err
		},
		func() error {
			_, err := service.Reconcile(ctx, ReconcileOptions{Workspace: workspace, StateDir: stateDir, Config: configuration})
			return err
		},
		func() error {
			_, err := service.Abort(ctx, AbortOptions{Workspace: workspace, StateDir: stateDir, Config: configuration})
			return err
		},
	}
	for _, check := range checks {
		if err := check(); err == nil || !strings.Contains(err.Error(), "minimal preset forbids telemetry") {
			t.Fatalf("minimal conflict did not fail first: %v", err)
		}
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 0 || providerCalls != 0 {
		t.Fatalf("invalid minimal configuration created resources: providers=%d entries=%d err=%v", providerCalls, len(entries), err)
	}
}

// TestMinimalFrozenConfigRejectsVerificationPlan 锁定冻结合同的独立校验：
// 即使调用方绕过配置解析，也不能生成带验证命令的最小组合投影。
func TestMinimalFrozenConfigRejectsVerificationPlan(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Preset = config.PresetMinimal
	raw, err := marshalFrozenRunConfig(configuration, configuration.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	var frozen contract.FrozenRunConfig
	if err := json.Unmarshal(raw, &frozen); err != nil {
		t.Fatal(err)
	}
	frozen.Projection.Verification.Commands = 1
	if _, err := contract.NewFrozenRunConfig(configuration.Fingerprint(), frozen.Projection); err == nil || !strings.Contains(err.Error(), "minimal frozen config forbids") {
		t.Fatalf("unsafe minimal frozen projection accepted: %v", err)
	}
}

// minimalWorkflowProvider 每次构造都从显式脚本位置继续，不保存跨实例对话，
// 用于验证正常执行与两次审批冷恢复共享同一最小装配路径。
type minimalWorkflowProvider struct {
	calls  []ports.ToolCall
	next   int
	before func(ports.ModelRequest) error
}

func (provider *minimalWorkflowProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if err := provider.before(request); err != nil {
		return err
	}
	for _, message := range request.Messages {
		if message.Role == "tool" {
			var result contract.ToolResult
			if err := json.Unmarshal([]byte(message.Content), &result); err != nil || !result.OK {
				return fmt.Errorf("basic tool did not succeed: %s", message.Content)
			}
		}
	}
	if provider.next < len(provider.calls) {
		call := provider.calls[provider.next]
		provider.next++
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &call}); err != nil {
			return err
		}
	} else if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "basic workflow complete"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// TestMinimalKernelBasicWorkflowAndColdResume 覆盖基础读搜改执行、两次审批、
// 全新模型实例恢复、Journal 完整性与不重复副作用，并验证记忆/消息未被装配。
func TestMinimalKernelBasicWorkflowAndColdResume(t *testing.T) {
	grep, err := exec.LookPath("grep")
	if err != nil {
		t.Skip("当前环境没有用于基础命令验收的 grep")
	}
	workspace, stateDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "input.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousExplain, previousDeliver := tools.CurrentMemoryExplain(), tools.CurrentMessageDeliver()
	defer tools.BindMemoryExplain(previousExplain)
	defer tools.BindMessageDeliver(previousDeliver)
	marker := errors.New("outer message callback")
	tools.BindMemoryExplain(func(context.Context, tools.MemoryRecallRequest) (json.RawMessage, error) {
		return json.RawMessage(`{"outer":true}`), nil
	})
	tools.BindMessageDeliver(func(context.Context, string, string, string) error { return marker })
	digest := sha256.Sum256([]byte("seed\n"))
	patch, err := json.Marshal(tools.PatchArguments{Path: "input.txt", Mode: "update", ExpectedSHA256: "sha256:" + hex.EncodeToString(digest[:]), Replacements: []tools.Replacement{{Old: "seed", New: "changed"}}})
	if err != nil {
		t.Fatal(err)
	}
	command, err := json.Marshal(tools.ExecArguments{Argv: []string{grep, "-q", "changed", "input.txt"}, TimeoutMS: 1000, MaxOutputBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	calls := []ports.ToolCall{
		{ID: "list", Name: "list_files", Arguments: json.RawMessage(`{"path":"."}`)},
		{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"input.txt"}`)},
		{ID: "search", Name: "search_text", Arguments: json.RawMessage(`{"query":"seed"}`)},
		{ID: "patch", Name: "apply_patch", Arguments: patch},
		{ID: "test", Name: "exec_command", Arguments: command},
	}
	providerInstances := 0
	starts := []int{0, 4, 5}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		if providerInstances >= len(starts) {
			return nil, errors.New("unexpected provider construction")
		}
		provider := &minimalWorkflowProvider{calls: calls, next: starts[providerInstances], before: func(request ports.ModelRequest) error {
			var names []string
			for _, spec := range request.Tools {
				names = append(names, spec.Name)
			}
			if strings.Join(names, ",") != "list_files,read_file,search_text,apply_patch,exec_command" {
				return fmt.Errorf("advanced tools exposed: %v", names)
			}
			result, err := tools.CurrentMemoryExplain()(context.Background(), tools.MemoryRecallRequest{})
			if err != nil || string(result) != `{"outer":true}` || !errors.Is(tools.CurrentMessageDeliver()(context.Background(), "", "", ""), marker) {
				return errors.New("minimal run replaced optional tool callbacks")
			}
			return nil
		}}
		providerInstances++
		return provider, nil
	}}
	configuration := withTestProvider(config.Defaults())
	configuration.Preset = config.PresetMinimal
	ctx := context.Background()
	first, runErr := service.Run(ctx, RunOptions{Task: "read, search, update and test", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var waiting *ports.WaitingApprovalError
	if !errors.As(runErr, &waiting) {
		t.Fatalf("patch did not require approval: %v", runErr)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "input.txt"))
	if err != nil || string(content) != "seed\n" {
		t.Fatalf("unapproved patch changed the workspace: %q %v", content, err)
	}
	second, resumeErr := service.Resume(ctx, ResumeOptions{RunID: first.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Approve: true, Fingerprint: waiting.Fingerprint}})
	waiting = nil
	if !errors.As(resumeErr, &waiting) {
		t.Fatalf("test command did not require independent approval: %v", resumeErr)
	}
	final, err := service.Resume(ctx, ResumeOptions{RunID: second.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Approve: true, Fingerprint: waiting.Fingerprint}})
	if err != nil || final.Result == nil || final.Result.State != string(domain.RunCompleted) || final.Response != "basic workflow complete" || providerInstances != 3 {
		t.Fatalf("final=%+v providers=%d err=%v", final, providerInstances, err)
	}
	content, err = os.ReadFile(filepath.Join(workspace, "input.txt"))
	if err != nil || string(content) != "changed\n" {
		t.Fatalf("approved patch not applied: %q %v", content, err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	events, err := journal.Load(ctx, final.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.VerifyChain(events); err != nil {
		t.Fatal(err)
	}
	counts := make(map[domain.EventType]int)
	for _, event := range events {
		counts[event.Type]++
	}
	if counts[domain.EventOperationStarted] != 5 || counts[domain.EventOperationSucceeded] != 5 || counts[domain.EventRunCompleted] != 1 || counts[domain.EventVerificationFinished] != 1 || counts[domain.EventVerificationCommandProposed] != 0 {
		t.Fatalf("unexpected operation or verification facts: %v", counts)
	}
	if final.Result.Evidence.Trusted || final.Result.Evidence.VerificationOK || final.Result.Manifest.Preset != config.PresetMinimal || final.Result.Manifest.Verification.Commands != 0 {
		t.Fatalf("zero-command completion was reported as verified: %+v", final.Result.Evidence)
	}
	if _, err := os.Stat(memoryDBPath(stateDir, hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("minimal run created memory storage: %v", err)
	}
}
