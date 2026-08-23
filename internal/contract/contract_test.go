package contract

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/domain"
)

// manifestFixtureEvents 构造一次完整运行的最小事件集：run_created 携带身份，
// 模型请求与验证事件补齐 Provider 与验证计划事实。
func manifestFixtureEvents(t *testing.T) []domain.Event {
	t.Helper()
	created := struct {
		domain.RunCreatedPayload
		ConfigRef struct {
			SHA256 string `json:"sha256"`
		} `json:"config_ref"`
	}{}
	created.Task = "fix the failing test"
	created.Workspace = "/private/workspace/root"
	created.ConfigFingerprint = "sha256:config"
	created.Build = &domain.BuildIdentity{Revision: "abc123", Dirty: "false", Flavor: "plain", Fingerprint: "sha256:build"}
	createdPayload, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	return []domain.Event{
		{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 1, Type: domain.EventRunCreated, Payload: createdPayload, OccurredAt: time.Unix(1000, 0)},
		{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 2, Type: domain.EventRunStarted, Payload: json.RawMessage(`{"invocation":"invoke-1"}`), OccurredAt: time.Unix(1001, 0)},
		{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 3, Type: domain.EventModelRequestStarted, Payload: json.RawMessage(`{"provider":"openai_compatible","model":"test-model"}`), OccurredAt: time.Unix(1002, 0)},
		{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 4, Type: domain.EventVerificationStarted, Payload: json.RawMessage(`{"commands_fingerprint":"sha256:verify"}`), OccurredAt: time.Unix(1003, 0)},
	}
}

// frozenFixtureConfig 模拟 run_created 冻结配置制品的非敏感内容；键名与
// config.Config 的默认 JSON 编码（Go 字段名）保持一致。
const frozenFixtureConfig = `{"Version":1,"Preset":"coding","Profile":"default","Provider":{"BaseURL":"https://example.invalid/v1","Model":"test-model"},"Policy":{"Write":"ask","Exec":"ask","ProtectedPaths":[".env"]},"Verification":{"Commands":[{"Argv":["true"]}]}}`

func TestFreeformTaskSpecDigestIsStableAndHidesPrompt(t *testing.T) {
	first := FreeformTaskSpec("修复本地测试任务")
	second := FreeformTaskSpec("修复本地测试任务")
	if first != second || first.SchemaVersion != 1 || first.Kind != TaskKindFreeform || first.Source != "cli" || !strings.HasPrefix(first.PromptDigest, "sha256:") || first.Benchmark != nil {
		t.Fatalf("task spec=%#v", first)
	}
	if FreeformTaskSpec("another task").PromptDigest == first.PromptDigest {
		t.Fatal("different prompts must produce different digests")
	}
	encoded, _ := json.Marshal(first)
	if strings.Contains(string(encoded), "修复本地测试任务") {
		t.Fatal("task spec leaked prompt text")
	}
}

func TestManifestFromRunProjectsIdentityAcrossSources(t *testing.T) {
	events := manifestFixtureEvents(t)
	manifest, err := ManifestFromRun(events, FreeformTaskSpec("fix the failing test"), json.RawMessage(frozenFixtureConfig))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || manifest.RunID != "run-1" || manifest.WorkspaceHash != "sha256:ws" {
		t.Fatalf("manifest=%#v", manifest)
	}
	if manifest.ConfigFingerprint != "sha256:config" || manifest.Build.Revision != "abc123" || manifest.Build.Fingerprint != "sha256:build" {
		t.Fatalf("frozen identity=%#v", manifest)
	}
	if manifest.Provider.Kind != ProviderKindOpenAICompatible || manifest.Provider.BaseURL != "https://example.invalid/v1" || manifest.Provider.Model != "test-model" {
		t.Fatalf("provider=%#v", manifest.Provider)
	}
	if manifest.Preset != "coding" || manifest.Profile != "default" || manifest.Policy.Write != "ask" || manifest.Policy.Exec != "ask" || len(manifest.Policy.ProtectedPaths) != 1 {
		t.Fatalf("config view=%#v", manifest)
	}
	if manifest.Verification.Commands != 1 || manifest.Verification.Fingerprint != "sha256:verify" {
		t.Fatalf("verification=%#v", manifest.Verification)
	}
}

func TestManifestFromRunToleratesLegacyRuns(t *testing.T) {
	legacy := []domain.Event{
		{RunID: "run-legacy", WorkspaceHash: "sha256:old", Seq: 1, Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"old","workspace":"/old"}`), OccurredAt: time.Unix(1, 0)},
	}
	manifest, err := ManifestFromRun(legacy, FreeformTaskSpec("old"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ConfigFingerprint != "" || manifest.Build.Fingerprint != "" || manifest.Provider.Kind != "" || manifest.Preset != "" || manifest.Verification.Fingerprint != "" {
		t.Fatalf("legacy manifest must leave unknown fields empty: %#v", manifest)
	}
}

func TestManifestFromRunRejectsMissingRunCreated(t *testing.T) {
	if _, err := ManifestFromRun(nil, FreeformTaskSpec("x"), nil); err == nil {
		t.Fatal("empty events must be rejected")
	}
	if _, err := ManifestFromRun([]domain.Event{{Type: domain.EventRunStarted}}, FreeformTaskSpec("x"), nil); err == nil {
		t.Fatal("events without run_created must be rejected")
	}
}

func TestResultFromRunProjectsCompletedTrustedOutcome(t *testing.T) {
	events := append(manifestFixtureEvents(t),
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 5, Type: domain.EventModelResponseFinished, Payload: json.RawMessage(`{"usage":{"prompt_tokens":11,"completion_tokens":7}}`), OccurredAt: time.Unix(1004, 0)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 6, Type: domain.EventVerificationFinished, Payload: json.RawMessage(`{"ok":true}`), OccurredAt: time.Unix(1005, 0)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 7, Type: domain.EventRunCompleted, Payload: json.RawMessage(`{"final_response":"done"}`), OccurredAt: time.Unix(1006, 0)},
	)
	projection := domain.RunProjection{ID: "run-1", State: domain.RunCompleted, VerificationSucceeded: true, LastVerificationSeq: 6, LastMutationSeq: 3}
	result, err := ResultFromRun(events, projection, FreeformTaskSpec("fix the failing test"), json.RawMessage(frozenFixtureConfig))
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || result.State != string(domain.RunCompleted) || result.Outcome != "completed" || result.FailureClass != "" || result.StopReason != "" {
		t.Fatalf("result=%#v", result)
	}
	if !result.Evidence.Trusted || result.Evidence.VerificationSeq != 6 || !result.Evidence.VerificationOK || result.Evidence.LastMutationSeq != 3 {
		t.Fatalf("evidence=%#v", result.Evidence)
	}
	if result.Metrics.Turns != 0 || result.Metrics.InputTokens != 11 || result.Metrics.OutputTokens != 7 || result.Metrics.WallTimeMS != 6000 {
		t.Fatalf("metrics=%#v", result.Metrics)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "fix the failing test") || strings.Contains(string(encoded), "/private/workspace/root") || strings.Contains(string(encoded), "done") {
		t.Fatalf("result leaked prompt, workspace path, or final response: %s", encoded)
	}
}

func TestResultFromRunClassifiesBudgetAndLoopGuardFailures(t *testing.T) {
	budgetFailed := append(manifestFixtureEvents(t),
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 5, Type: domain.EventRunFailed, Payload: json.RawMessage(`{"error":"turns budget exhausted","budget_dimension":"turns"}`), OccurredAt: time.Unix(1005, 0)},
	)
	projection := domain.RunProjection{ID: "run-1", State: domain.RunFailed}
	result, err := ResultFromRun(budgetFailed, projection, FreeformTaskSpec("fix the failing test"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.FailureClass != "budget_turns" || result.StopReason != "turns" || result.Outcome != string(domain.RunFailed) {
		t.Fatalf("budget result=%#v", result)
	}
	guardFailed := append(manifestFixtureEvents(t),
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 5, Type: domain.EventRunFailed, Payload: json.RawMessage(`{"error":"loop guard","error_class":"loop_guard_repeated_operation_failure"}`), OccurredAt: time.Unix(1005, 0)},
	)
	guardResult, err := ResultFromRun(guardFailed, projection, FreeformTaskSpec("fix the failing test"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if guardResult.FailureClass != "loop_guard_repeated_operation_failure" || guardResult.StopReason != "loop_guard_repeated_operation_failure" {
		t.Fatalf("loop guard result=%#v", guardResult)
	}
	cancelled := append(manifestFixtureEvents(t),
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 5, Type: domain.EventRunCancelled, Payload: json.RawMessage(`{"cause":"user_interrupt"}`), OccurredAt: time.Unix(1005, 0)},
	)
	cancelledResult, err := ResultFromRun(cancelled, domain.RunProjection{ID: "run-1", State: domain.RunCancelled}, FreeformTaskSpec("fix the failing test"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cancelledResult.FailureClass != FailureCancelled || cancelledResult.StopReason != "cancelled: user_interrupt" {
		t.Fatalf("cancelled result=%#v", cancelledResult)
	}
}

func TestClassifyErrorPreservesUnifiedTaxonomy(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&agent.WaitingApprovalError{}, FailureApproval},
		{&agent.PolicyDeniedError{}, FailurePolicy},
		{&agent.BudgetExceededError{Dimension: "turns"}, "budget_turns"},
		{&agent.LoopGuardExceededError{Reason: "periodic_turn_cycle"}, "loop_guard_periodic_turn_cycle"},
		{errors.New("plain"), FailureAgent},
	}
	for _, test := range tests {
		if got := ClassifyError(test.err); got != test.want {
			t.Fatalf("ClassifyError(%v)=%q want %q", test.err, got, test.want)
		}
	}
}

func TestCompletionTrustedRequiresFreshVerification(t *testing.T) {
	if CompletionTrusted(domain.RunCompleted, true, 5, 3) != true {
		t.Fatal("completed run with fresh verification must be trusted")
	}
	if CompletionTrusted(domain.RunCompleted, true, 3, 5) {
		t.Fatal("stale verification must not be trusted")
	}
	if CompletionTrusted(domain.RunCompleted, true, 0, 0) {
		t.Fatal("missing verification event must not be trusted")
	}
	if CompletionTrusted(domain.RunFailed, true, 5, 3) {
		t.Fatal("non-completed run must not be trusted")
	}
}

// TestManifestReplaysProviderCardAcrossSources 锁定卡回放三态：显式声明产出
// config 卡（与 Boot 同一解析函数）、无声明命中内建目录产出 builtin 卡、
// 声明存在但非法时不回退内建（防御路径保持 nil）。
func TestManifestReplaysProviderCardAcrossSources(t *testing.T) {
	events := manifestFixtureEvents(t)
	declared := strings.Replace(frozenFixtureConfig, `"Model":"test-model"`, `"Model":"test-model","Capability":{"ToolCalls":true,"StreamingUsage":true,"ContextWindowTokens":131072}`, 1)
	manifest, err := ManifestFromRun(events, FreeformTaskSpec("fix the failing test"), json.RawMessage(declared))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Provider.Card == nil || manifest.Provider.Card.Source != ProviderCardSourceConfig || manifest.Provider.Card.ContextWindowTokens != 131072 {
		t.Fatalf("declared card = %#v", manifest.Provider.Card)
	}
	// 无声明 + 内建模型：事件与冻结配置同为 glm-5.2 时 builtin 回放。
	builtin := strings.Replace(frozenFixtureConfig, `"Model":"test-model"`, `"Model":"glm-5.2"`, 1)
	builtinEvents := make([]domain.Event, len(events))
	copy(builtinEvents, events)
	builtinEvents[2].Payload = json.RawMessage(`{"provider":"openai_compatible","model":"glm-5.2"}`)
	manifest, err = ManifestFromRun(builtinEvents, FreeformTaskSpec("fix the failing test"), json.RawMessage(builtin))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Provider.Card == nil || manifest.Provider.Card.Source != ProviderCardSourceBuiltin || manifest.Provider.Card.ContextWindowTokens != 0 {
		t.Fatalf("builtin card = %#v", manifest.Provider.Card)
	}
	// 声明非法（streaming_usage 缺省为假）时不落入内建回退。
	broken := strings.Replace(builtin, `"Model":"glm-5.2"`, `"Model":"glm-5.2","Capability":{"ToolCalls":true}`, 1)
	manifest, err = ManifestFromRun(builtinEvents, FreeformTaskSpec("fix the failing test"), json.RawMessage(broken))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Provider.Card != nil {
		t.Fatalf("broken declaration must not fall back to builtin: %#v", manifest.Provider.Card)
	}
	// 未知模型无声明：nil。
	unknown := strings.Replace(frozenFixtureConfig, `"Model":"test-model"`, `"Model":"mystery"`, 1)
	unknownEvents := make([]domain.Event, len(events))
	copy(unknownEvents, events)
	unknownEvents[2].Payload = json.RawMessage(`{"provider":"openai_compatible","model":"mystery"}`)
	_ = unknownEvents
	manifest, err = ManifestFromRun(unknownEvents, FreeformTaskSpec("fix the failing test"), json.RawMessage(unknown))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Provider.Card != nil {
		t.Fatalf("unknown model must not invent a card: %#v", manifest.Provider.Card)
	}
}
