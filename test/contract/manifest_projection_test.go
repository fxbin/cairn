// Package contract_test 验证 V1.1 统一产品合同在普通 Run 与 benchmark 两条
// 路径上的投影一致性：同一构造器、同一事实源、无第二 owner。
package contract_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// withTestProvider 为测试配置补齐能力卡所需的模型与显式声明。
func withTestProvider(configuration config.Config) config.Config {
	if configuration.Provider.Model == "" {
		configuration.Provider.Model = "test-model"
	}
	configuration.Provider.Admission = config.ProviderAdmissionOptional
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	return configuration
}

// terminalProvider 返回无工具调用的最终文本，保证本测试不发起真实网络请求。
type terminalProvider struct{}

// Stream 生成一条最小合法响应流供合同投影测试使用。
func (terminalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"}); err != nil {
		return err
	}
	if err := emit(ports.ModelEvent{Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done, StopReason: "stop"})
}

// TestNormalRunAndBenchmarkShareOneManifestConstructor 以一次真实 SQLite 运行
// 为事实源，验证四种投影互相一致：Run 结束时返回的 contract.Result、事后
// Inspect 的 manifest、独立 evidence 视图，以及 benchmark 侧用同一事件流 +
// 冻结配置直调 contract.ResultFromRun 的结果（仅任务身份不同）。
func TestNormalRunAndBenchmarkShareOneManifestConstructor(t *testing.T) {
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return terminalProvider{}, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "test-model"
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), app.RunOptions{Task: "unified contract probe", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || result.Result == nil {
		t.Fatalf("run result=%#v err=%v", result, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil || inspected.Manifest == nil {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	if !reflect.DeepEqual(*inspected.Manifest, result.Result.Manifest) {
		t.Fatalf("inspect manifest drifted from run projection:\ninspect=%#v\nrun=%#v", *inspected.Manifest, result.Result.Manifest)
	}
	// 独立 evidence 视图：同一事件流的回放投影必须与运行时投影整体一致，
	// 包括从冻结配置回放的 Provider 能力卡。
	evidence, err := service.Evidence(context.Background(), workspace, stateDir, configuration, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence, *result.Result) {
		t.Fatalf("evidence view drifted from run projection:\nevidence=%#v\nrun=%#v", evidence, *result.Result)
	}
	if evidence.Manifest.Provider.Card == nil || evidence.Manifest.Provider.Card.Model != "test-model" || !evidence.Manifest.Provider.Card.ToolCalls {
		t.Fatalf("evidence provider card = %#v", evidence.Manifest.Provider.Card)
	}

	// benchmark 视角：同一事件流 + 运行实际写入的 v2 冻结配置，仅任务身份
	// 换成 TaskPack 投影。
	var created struct {
		ConfigRef artifact.Ref `json:"config_ref"`
	}
	if err := json.Unmarshal(inspected.Events[0].Payload, &created); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.New(filepath.Join(stateDir, "workspaces", inspected.Projection.WorkspaceHash, "artifacts"), configuration.Output.MaxPreviewBytes)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := artifacts.Open(created.ConfigRef)
	if err != nil {
		t.Fatal(err)
	}
	benchmarkTask := contract.BenchmarkTaskSpec("unified contract probe", contract.BenchmarkTaskRef{
		TaskID: "CONTRACT-001", SuiteID: "suite-x", TaskPackFingerprint: "sha256:pack", VerifierFingerprint: "sha256:verifier",
	})
	var planRef artifact.Ref
	for _, event := range inspected.Events {
		if event.Type != domain.EventVerificationStarted {
			continue
		}
		var payload struct {
			PlanRef artifact.Ref `json:"plan_ref"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		planRef = payload.PlanRef
	}
	plan, err := artifacts.Open(planRef)
	if err != nil {
		t.Fatal(err)
	}
	benchmarkResult, err := contract.ResultFromRun(inspected.Events, inspected.Projection, benchmarkTask, frozen, plan)
	if err != nil {
		t.Fatal(err)
	}
	if benchmarkResult.Manifest.ConfigFingerprint != result.Result.Manifest.ConfigFingerprint ||
		!reflect.DeepEqual(benchmarkResult.Manifest.Provider, result.Result.Manifest.Provider) ||
		benchmarkResult.Manifest.Build != result.Result.Manifest.Build ||
		benchmarkResult.Manifest.WorkspaceHash != result.Result.Manifest.WorkspaceHash ||
		benchmarkResult.State != result.Result.State ||
		benchmarkResult.Evidence != result.Result.Evidence ||
		benchmarkResult.Metrics != result.Result.Metrics {
		t.Fatalf("benchmark projection drifted from normal-run projection:\nbenchmark=%#v\nnormal=%#v", benchmarkResult, *result.Result)
	}
	if benchmarkResult.Manifest.Task.Kind != contract.TaskKindBenchmark || benchmarkResult.Manifest.Task.Benchmark.TaskID != "CONTRACT-001" {
		t.Fatalf("benchmark task spec=%#v", benchmarkResult.Manifest.Task)
	}

	encoded, err := json.Marshal(benchmarkResult)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"unified contract probe", workspace, stateDir} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("benchmark contract projection leaked %q", forbidden)
		}
	}
}
