package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

const (
	fixtureConfigFingerprint       = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fixtureVerificationFingerprint = "sha256:5fd644f78a8abad0a26946b2da5a493cbefc2f81ae7533d3feff2eb3f24496b4"
)

// verificationPlanFixture 用生产摘要算法构造带内容寻址引用的测试计划。
func verificationPlanFixture(t *testing.T, verificationID string, lastMutationSeq uint64, commands []VerificationCommandSpec) (json.RawMessage, string, string) {
	t.Helper()
	fingerprint, err := FingerprintVerificationCommands(commands)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(VerificationPlanSpec{
		Version: VerificationPlanVersion, VerificationID: verificationID, ConfigFingerprint: fixtureConfigFingerprint,
		LastMutationSeq: lastMutationSeq, CommandsFingerprint: fingerprint, Commands: commands,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(plan)
	return plan, "sha256:" + hex.EncodeToString(digest[:]), fingerprint
}

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
	created.ConfigFingerprint = fixtureConfigFingerprint
	created.Build = &domain.BuildIdentity{Revision: "abc123", Dirty: "false", Flavor: "plain", Fingerprint: "sha256:3333333333333333333333333333333333333333333333333333333333333333"}
	createdPayload, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	return []domain.Event{
		{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 1, Type: domain.EventRunCreated, Payload: createdPayload, OccurredAt: time.Unix(1000, 0)},
		{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 2, Type: domain.EventRunStarted, Payload: json.RawMessage(`{"invocation":"invoke-1"}`), OccurredAt: time.Unix(1001, 0)},
		{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 3, Type: domain.EventModelRequestStarted, Payload: json.RawMessage(`{"provider":"openai_compatible","model":"test-model"}`), OccurredAt: time.Unix(1002, 0)},
		{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 4, Type: domain.EventVerificationStarted, Payload: json.RawMessage(`{"commands_fingerprint":"` + fixtureVerificationFingerprint + `"}`), OccurredAt: time.Unix(1003, 0)},
	}
}

// frozenFixtureProjection 返回当前 v2 冻结配置合同的最小合法投影。
func frozenFixtureProjection() FrozenConfigProjection {
	return FrozenConfigProjection{
		ConfigVersion: frozenAgentConfigVersion,
		Preset:        "coding",
		Profile:       "default",
		Provider: FrozenProviderProjection{
			Kind: ProviderKindOpenAICompatible, BaseURL: "https://example.invalid/v1",
			Model: "test-model", ID: ProviderIDCustom, Admission: "optional",
		},
		Policy:       PolicyIdentity{Write: "ask", Exec: "ask", ProtectedPaths: []string{".env"}},
		Verification: FrozenVerificationProjection{Commands: 1, Fingerprint: fixtureVerificationFingerprint},
	}
}

// frozenFixtureConfig 通过生产构造器生成与事件指纹绑定的合法冻结配置。
func frozenFixtureConfig(t *testing.T, mutate func(*FrozenConfigProjection)) json.RawMessage {
	t.Helper()
	projection := frozenFixtureProjection()
	if mutate != nil {
		mutate(&projection)
	}
	frozen, err := NewFrozenRunConfig(fixtureConfigFingerprint, projection)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// uncheckedFrozenFixture 只为负向测试构造摘要自洽但语义非法的冻结制品。
func uncheckedFrozenFixture(t *testing.T, projection FrozenConfigProjection) json.RawMessage {
	t.Helper()
	fingerprint, err := canonicaljson.Fingerprint(frozenRunConfigProjectionPrefix, projection)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(FrozenRunConfig{
		SchemaVersion: frozenRunConfigSchemaVersion, ConfigFingerprint: fixtureConfigFingerprint,
		ProjectionFingerprint: fingerprint, Projection: projection,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

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
	manifest, err := ManifestFromRun(events, FreeformTaskSpec("fix the failing test"), frozenFixtureConfig(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != manifestSchemaVersion || manifest.RunID != "run-1" || manifest.WorkspaceHash != "sha256:ws" {
		t.Fatalf("manifest=%#v", manifest)
	}
	if manifest.ConfigFingerprint != fixtureConfigFingerprint || manifest.Build.Revision != "abc123" || manifest.Build.Fingerprint == "" {
		t.Fatalf("frozen identity=%#v", manifest)
	}
	if manifest.Provider.Kind != ProviderKindOpenAICompatible || manifest.Provider.BaseURL != "https://example.invalid/v1" || manifest.Provider.Model != "test-model" {
		t.Fatalf("provider=%#v", manifest.Provider)
	}
	if manifest.Preset != "coding" || manifest.Profile != "default" || manifest.Policy.Write != "ask" || manifest.Policy.Exec != "ask" || len(manifest.Policy.ProtectedPaths) != 1 {
		t.Fatalf("config view=%#v", manifest)
	}
	if manifest.Verification.Commands != 1 || manifest.Verification.Fingerprint != fixtureVerificationFingerprint {
		t.Fatalf("verification=%#v", manifest.Verification)
	}
}

func TestManifestFromRunRejectsLegacyRuns(t *testing.T) {
	legacy := []domain.Event{
		{RunID: "run-legacy", WorkspaceHash: "sha256:old", Seq: 1, Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"old","workspace":"/old"}`), OccurredAt: time.Unix(1, 0)},
	}
	if _, err := ManifestFromRun(legacy, FreeformTaskSpec("old"), nil); err == nil {
		t.Fatal("legacy run without frozen config must fail closed")
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
	turnID := domain.TurnID("turn-1")
	operationID := domain.OperationID("verify-op-1")
	plan, planDigest, fingerprint := verificationPlanFixture(t, "verify-1", 3, []VerificationCommandSpec{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}})
	if fingerprint != fixtureVerificationFingerprint {
		t.Fatalf("fixture fingerprint drifted: %s", fingerprint)
	}
	planRef := `{"sha256":"` + planDigest + `"}`
	events := append(manifestFixtureEvents(t),
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 5, Type: domain.EventModelResponseFinished, Payload: json.RawMessage(`{"usage":{"prompt_tokens":11,"completion_tokens":7}}`), OccurredAt: time.Unix(1004, 0)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 6, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: json.RawMessage(`{"verification_id":"verify-1","commands_fingerprint":"` + fingerprint + `","last_mutation_seq":3,"workspace_observation":"sha256:workspace","plan_ref":` + planRef + `}`), OccurredAt: time.Unix(1004, 1)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 7, Type: domain.EventVerificationCommandProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"verification_id":"verify-1","command_index":0,"arguments":{"argv":["verify"]},"plan_ref":` + planRef + `}`), OccurredAt: time.Unix(1004, 2)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 8, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"approval-fp"}`), OccurredAt: time.Unix(1004, 3)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 9, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"exec_command","side_effect_class":"process"}`), OccurredAt: time.Unix(1004, 4)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 10, Type: domain.EventOperationSucceeded, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"exec_command","result":{"ok":true}}`), OccurredAt: time.Unix(1004, 5)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 11, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: json.RawMessage(`{"verification_id":"verify-1","ok":true}`), OccurredAt: time.Unix(1005, 0)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 12, Type: domain.EventRunCompleted, Payload: json.RawMessage(`{"final_response":"done"}`), OccurredAt: time.Unix(1006, 0)},
	)
	projection := domain.RunProjection{ID: "run-1", State: domain.RunCompleted, VerificationSucceeded: true, LastVerificationSeq: 11, LastMutationSeq: 3}
	result, err := ResultFromRun(events, projection, FreeformTaskSpec("fix the failing test"), frozenFixtureConfig(t, nil), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || result.State != string(domain.RunCompleted) || result.Outcome != "completed" || result.FailureClass != "" || result.StopReason != "" {
		t.Fatalf("result=%#v", result)
	}
	if !result.Evidence.Trusted || result.Evidence.VerificationSeq != 11 || !result.Evidence.VerificationOK || result.Evidence.LastMutationSeq != 3 {
		t.Fatalf("evidence=%#v", result.Evidence)
	}
	if result.Metrics.Turns != 0 || result.Metrics.ToolCalls != 1 || result.Metrics.InputTokens != 11 || result.Metrics.OutputTokens != 7 || result.Metrics.WallTimeMS != 6000 {
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
	frozenConfig := frozenFixtureConfig(t, nil)
	result, err := ResultFromRun(budgetFailed, projection, FreeformTaskSpec("fix the failing test"), frozenConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.FailureClass != "budget_turns" || result.StopReason != "turns" || result.Outcome != string(domain.RunFailed) {
		t.Fatalf("budget result=%#v", result)
	}
	guardFailed := append(manifestFixtureEvents(t),
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 5, Type: domain.EventRunFailed, Payload: json.RawMessage(`{"error":"loop guard","error_class":"loop_guard_repeated_operation_failure"}`), OccurredAt: time.Unix(1005, 0)},
	)
	guardResult, err := ResultFromRun(guardFailed, projection, FreeformTaskSpec("fix the failing test"), frozenConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if guardResult.FailureClass != "loop_guard_repeated_operation_failure" || guardResult.StopReason != "loop_guard_repeated_operation_failure" {
		t.Fatalf("loop guard result=%#v", guardResult)
	}
	if !IsKnownTerminalFailureClass(guardResult.FailureClass) {
		t.Fatalf("known terminal failure class omitted %q", guardResult.FailureClass)
	}
	cancelled := append(manifestFixtureEvents(t),
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 5, Type: domain.EventRunCancelled, Payload: json.RawMessage(`{"cause":"user_interrupt"}`), OccurredAt: time.Unix(1005, 0)},
	)
	cancelledResult, err := ResultFromRun(cancelled, domain.RunProjection{ID: "run-1", State: domain.RunCancelled}, FreeformTaskSpec("fix the failing test"), frozenConfig, nil)
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
		{&ports.WaitingApprovalError{}, FailureApproval},
		{&ports.PolicyDeniedError{}, FailurePolicy},
		{&ports.BudgetExceededError{Dimension: "turns"}, "budget_turns"},
		{&ports.LoopGuardExceededError{Reason: "periodic_turn_cycle"}, "loop_guard_periodic_turn_cycle"},
		{errors.New("plain"), FailureAgent},
	}
	for _, test := range tests {
		if got := ClassifyError(test.err); got != test.want {
			t.Fatalf("ClassifyError(%v)=%q want %q", test.err, got, test.want)
		}
	}
}

func TestCompletionTrustedRequiresFreshVerification(t *testing.T) {
	if CompletionTrusted(domain.RunCompleted, true, 5, 3, true) != true {
		t.Fatal("completed run with fresh verification must be trusted")
	}
	if CompletionTrusted(domain.RunCompleted, true, 3, 5, true) {
		t.Fatal("stale verification must not be trusted")
	}
	if CompletionTrusted(domain.RunCompleted, true, 0, 0, true) {
		t.Fatal("missing verification event must not be trusted")
	}
	if CompletionTrusted(domain.RunFailed, true, 5, 3, true) {
		t.Fatal("non-completed run must not be trusted")
	}
	if CompletionTrusted(domain.RunCompleted, true, 5, 0, true) != true {
		t.Fatal("zero-mutation run with executed verification command may be trusted")
	}
	if CompletionTrusted(domain.RunCompleted, true, 5, 0, false) {
		t.Fatal("verification without any command execution must not be trusted")
	}
	if CompletionTrusted(domain.RunCompleted, false, 5, 3, true) {
		t.Fatal("failed verification must not be trusted")
	}
}

// verificationEvidenceFixture 构造实际执行或成功对账的完整命令链。
func verificationEvidenceFixture(t *testing.T, commands []VerificationCommandSpec, reconcileLast bool) ([]domain.Event, VerificationEvidenceSpec) {
	t.Helper()
	plan, digest, fingerprint := verificationPlanFixture(t, "v1", 0, commands)
	turnID := domain.TurnID("turn-1")
	events := make([]domain.Event, 0, len(commands)*4+2)
	appendEvent := func(eventType domain.EventType, operationID *domain.OperationID, payload json.RawMessage) {
		events = append(events, domain.Event{Seq: uint64(len(events) + 1), Type: eventType, TurnID: &turnID, OperationID: operationID, Payload: payload})
	}
	appendEvent(domain.EventVerificationStarted, nil, json.RawMessage(fmt.Sprintf(`{"verification_id":"v1","commands_fingerprint":%q,"workspace_observation":"sha256:workspace","plan_ref":{"sha256":%q}}`, fingerprint, digest)))
	for index, command := range commands {
		operationID := domain.OperationID(fmt.Sprintf("operation-%d", index))
		appendEvent(domain.EventVerificationCommandProposed, &operationID, json.RawMessage(fmt.Sprintf(`{"verification_id":"v1","command_index":%d,"arguments":%s,"plan_ref":{"sha256":%q}}`, index, command.Arguments, digest)))
		appendEvent(domain.EventPolicyDecided, &operationID, json.RawMessage(`{"effect":"allow","fingerprint":"policy-fp"}`))
		appendEvent(domain.EventOperationStarted, &operationID, json.RawMessage(`{"tool_name":"exec_command","side_effect_class":"process"}`))
		if reconcileLast && index == len(commands)-1 {
			appendEvent(domain.EventOperationReconciled, &operationID, json.RawMessage(`{"verdict":"applied_success","workspace_observation":"sha256:workspace"}`))
		} else {
			appendEvent(domain.EventOperationSucceeded, &operationID, json.RawMessage(`{"tool_name":"exec_command","result":{"ok":true}}`))
		}
	}
	appendEvent(domain.EventVerificationFinished, nil, json.RawMessage(`{"verification_id":"v1","ok":true}`))
	return events, VerificationEvidenceSpec{Plan: plan, Commands: len(commands), Fingerprint: fingerprint, ConfigFingerprint: fixtureConfigFingerprint, FinishedSeq: events[len(events)-1].Seq}
}

// TestVerificationCommandsSucceededRequiresCompletePlan 锁定冻结参数、命令
// 类型、授权和完整成功终态；夹入工作区变更或旧提议即完成均不得可信。
func TestVerificationCommandsSucceededRequiresCompletePlan(t *testing.T) {
	events, spec := verificationEvidenceFixture(t, []VerificationCommandSpec{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}, false)
	planHash := sha256.Sum256(spec.Plan)
	planDigest := "sha256:" + hex.EncodeToString(planHash[:])
	if !VerificationCommandsSucceeded(events, spec) {
		t.Fatal("complete frozen command must be trusted")
	}
	cases := []struct {
		name   string
		change func([]domain.Event, *VerificationEvidenceSpec) []domain.Event
	}{
		{name: "proposal only", change: func(events []domain.Event, _ *VerificationEvidenceSpec) []domain.Event {
			return []domain.Event{events[0], events[1], events[5]}
		}},
		{name: "missing terminal", change: func(events []domain.Event, _ *VerificationEvidenceSpec) []domain.Event {
			return append(events[:4], events[5])
		}},
		{name: "wrong arguments", change: func(events []domain.Event, _ *VerificationEvidenceSpec) []domain.Event {
			events[1].Payload = json.RawMessage(`{"verification_id":"v1","command_index":0,"arguments":{"argv":["other"]},"plan_ref":{"sha256":"` + planDigest + `"}}`)
			return events
		}},
		{name: "wrong tool", change: func(events []domain.Event, _ *VerificationEvidenceSpec) []domain.Event {
			events[3].Payload = json.RawMessage(`{"tool_name":"read_file","side_effect_class":"process"}`)
			return events
		}},
		{name: "wrong terminal tool", change: func(events []domain.Event, _ *VerificationEvidenceSpec) []domain.Event {
			events[4].Payload = json.RawMessage(`{"tool_name":"read_file","result":{"ok":true}}`)
			return events
		}},
		{name: "empty authorization fingerprint", change: func(events []domain.Event, _ *VerificationEvidenceSpec) []domain.Event {
			events[2].Payload = json.RawMessage(`{"effect":"allow"}`)
			return events
		}},
		{name: "missing plan", change: func(events []domain.Event, spec *VerificationEvidenceSpec) []domain.Event {
			spec.Plan = nil
			return events
		}},
		{name: "missing second command", change: func(events []domain.Event, spec *VerificationEvidenceSpec) []domain.Event {
			spec.Commands = 2
			return events
		}},
		{name: "stale mutation baseline", change: func(events []domain.Event, spec *VerificationEvidenceSpec) []domain.Event {
			spec.LastMutationSeq = 5
			return events
		}},
		{name: "interleaved mutation", change: func(events []domain.Event, spec *VerificationEvidenceSpec) []domain.Event {
			other := domain.OperationID("other")
			mutation := domain.Event{Seq: 6, Type: domain.EventOperationStarted, OperationID: &other, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)}
			finished := events[5]
			finished.Seq = 7
			spec.FinishedSeq = 7
			return append(append(events[:5], mutation), finished)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			copyEvents := append([]domain.Event(nil), events...)
			copySpec := spec
			copyEvents = test.change(copyEvents, &copySpec)
			if VerificationCommandsSucceeded(copyEvents, copySpec) {
				t.Fatal("incomplete or mismatched verification must not be trusted")
			}
		})
	}
}

// TestVerificationCommandsSucceededAcceptsSuccessfulReconciliation 锁定多命令
// 计划中已启动命令经成功人工对账后才可作为成功终态。
func TestVerificationCommandsSucceededAcceptsSuccessfulReconciliation(t *testing.T) {
	events, spec := verificationEvidenceFixture(t, []VerificationCommandSpec{
		{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify-a"]}`)},
		{ID: "verification-2", Arguments: json.RawMessage(`{"argv":["verify-b"]}`)},
	}, true)
	if !VerificationCommandsSucceeded(events, spec) {
		t.Fatal("both frozen commands have successful terminal evidence")
	}
	failed := append([]domain.Event(nil), events...)
	failed[len(failed)-2].Payload = json.RawMessage(`{"verdict":"applied_failure"}`)
	if VerificationCommandsSucceeded(failed, spec) {
		t.Fatal("failed reconciliation cannot satisfy a frozen command")
	}
	drifted := append([]domain.Event(nil), events...)
	drifted[len(drifted)-2].Payload = json.RawMessage(`{"verdict":"applied_success","workspace_observation":"sha256:drifted"}`)
	if VerificationCommandsSucceeded(drifted, spec) {
		t.Fatal("successful reconciliation after workspace drift cannot be trusted")
	}
}

// TestManifestReplaysProviderCardFromConfigOnly 锁定卡回放三态：合法显式声明
// 产出 config 卡，非法声明失败关闭，无声明的未知模型保持 nil。
func TestManifestReplaysProviderCardFromConfigOnly(t *testing.T) {
	events := manifestFixtureEvents(t)
	declared := frozenFixtureConfig(t, func(projection *FrozenConfigProjection) {
		projection.Provider.Capability = &FrozenProviderCapabilityProjection{ToolCalls: true, StreamingUsage: true, ContextWindowTokens: 131072}
	})
	manifest, err := ManifestFromRun(events, FreeformTaskSpec("fix the failing test"), declared)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Provider.Card == nil || manifest.Provider.Card.Source != ProviderCardSourceConfig || manifest.Provider.Card.ContextWindowTokens != 131072 {
		t.Fatalf("declared card = %#v", manifest.Provider.Card)
	}
	// 声明非法（streaming_usage 为假）时拒绝整个冻结合同。
	brokenProjection := frozenFixtureProjection()
	brokenProjection.Provider.Capability = &FrozenProviderCapabilityProjection{ToolCalls: true}
	if _, err := ManifestFromRun(events, FreeformTaskSpec("fix the failing test"), uncheckedFrozenFixture(t, brokenProjection)); err == nil {
		t.Fatal("broken declaration must fail closed")
	}
	// 未知模型无声明：nil。
	unknownEvents := make([]domain.Event, len(events))
	copy(unknownEvents, events)
	unknownEvents[2].Payload = json.RawMessage(`{"provider":"openai_compatible","model":"mystery"}`)
	unknown := frozenFixtureConfig(t, func(projection *FrozenConfigProjection) {
		projection.Provider.Model = "mystery"
	})
	manifest, err = ManifestFromRun(unknownEvents, FreeformTaskSpec("fix the failing test"), unknown)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Provider.Card != nil {
		t.Fatalf("unknown model must not invent a card: %#v", manifest.Provider.Card)
	}
}

// TestManifestProjectsVersion2Selection 锁定固定 schema v2 与
// ProviderSelection 身份投影。
func TestManifestProjectsVersion2Selection(t *testing.T) {
	events := manifestFixtureEvents(t)
	manifest, err := ManifestFromRun(events, FreeformTaskSpec("v2 run"), frozenFixtureConfig(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != manifestSchemaVersion {
		t.Fatalf("schema version = %d", manifest.SchemaVersion)
	}
	selection := manifest.Provider.Selection
	if selection == nil {
		t.Fatal("version 2 manifest must project a provider selection")
	}
	if selection.ProviderID != ProviderIDCustom || selection.ProtocolID != ProtocolOpenAIChatCompletions {
		t.Fatalf("selection identity = %+v", selection)
	}
	if selection.ModelRef != ProviderIDCustom+"/test-model" {
		t.Fatalf("model ref = %q", selection.ModelRef)
	}
	if !strings.HasPrefix(selection.EndpointDigest, "sha256:") {
		t.Fatalf("endpoint digest = %q", selection.EndpointDigest)
	}
}

// TestManifestRejectsLegacyConfigAndInvalidFacts 锁定旧 Config JSON、v1 投影
// 与非法 Provider 身份都失败关闭。
func TestManifestRejectsLegacyConfigAndInvalidFacts(t *testing.T) {
	events := manifestFixtureEvents(t)
	legacy := json.RawMessage(`{"Version":2,"Provider":{"BaseURL":"https://example.invalid/v1","Model":"test-model"}}`)
	if _, err := ManifestFromRun(events, FreeformTaskSpec("legacy"), legacy); err == nil {
		t.Fatal("direct Config JSON must fail closed")
	}
	versionOne := frozenFixtureProjection()
	versionOne.ConfigVersion = 1
	if _, err := ManifestFromRun(events, FreeformTaskSpec("v1"), uncheckedFrozenFixture(t, versionOne)); err == nil {
		t.Fatal("v1 frozen projection must fail closed")
	}
	broken := frozenFixtureProjection()
	broken.Provider.BaseURL = "not-a-url"
	if _, err := ManifestFromRun(events, FreeformTaskSpec("broken"), uncheckedFrozenFixture(t, broken)); err == nil {
		t.Fatal("invalid provider identity must fail closed")
	}
}

// TestResultFromRunMarksLegacyVacuousVerificationUntrusted 锁定旧假通过
// （verification_finished OK 但无命令提议）在读侧投影为不可信，日志仍可回放。
func TestResultFromRunMarksLegacyVacuousVerificationUntrusted(t *testing.T) {
	events := append(manifestFixtureEvents(t),
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 5, Type: domain.EventVerificationStarted, Payload: json.RawMessage(`{"verification_id":"v1","last_mutation_seq":0,"commands_fingerprint":"sha256:x"}`), OccurredAt: time.Unix(1004, 0)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 6, Type: domain.EventVerificationFinished, Payload: json.RawMessage(`{"verification_id":"v1","ok":true}`), OccurredAt: time.Unix(1005, 0)},
		domain.Event{RunID: "run-1", WorkspaceHash: "sha256:ws", Seq: 7, Type: domain.EventRunCompleted, Payload: json.RawMessage(`{"final_response":"done"}`), OccurredAt: time.Unix(1006, 0)},
	)
	projection := domain.RunProjection{ID: "run-1", State: domain.RunCompleted, VerificationSucceeded: true, LastVerificationSeq: 6, LastMutationSeq: 0}
	result, err := ResultFromRun(events, projection, FreeformTaskSpec("fix"), frozenFixtureConfig(t, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Evidence.Trusted {
		t.Fatalf("legacy vacuous verification must not be trusted: %#v", result.Evidence)
	}
	if result.State != string(domain.RunCompleted) {
		t.Fatalf("state=%s", result.State)
	}
}

// TestResultFromRunRejectsProposedOnlyVerification 锁定旧日志中仅有命令提议
// 与 OK 完成事件时，运行可读取但完成证据不得标记为可信。
func TestResultFromRunRejectsProposedOnlyVerification(t *testing.T) {
	turnID := domain.TurnID("turn-1")
	operationID := domain.OperationID("operation-1")
	plan, planDigest, fingerprint := verificationPlanFixture(t, "v1", 0, []VerificationCommandSpec{{ID: "verification-1", Arguments: json.RawMessage(`{"argv":["verify"]}`)}})
	planRef := `{"sha256":"` + planDigest + `"}`
	events := append(manifestFixtureEvents(t),
		domain.Event{Seq: 5, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: json.RawMessage(`{"verification_id":"v1","commands_fingerprint":"` + fingerprint + `","workspace_observation":"sha256:workspace","plan_ref":` + planRef + `}`)},
		domain.Event{Seq: 6, Type: domain.EventVerificationCommandProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"verification_id":"v1","command_index":0,"arguments":{"argv":["verify"]},"plan_ref":` + planRef + `}`)},
		domain.Event{Seq: 7, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: json.RawMessage(`{"verification_id":"v1","ok":true}`)},
		domain.Event{Seq: 8, Type: domain.EventRunCompleted, Payload: json.RawMessage(`{"final_response":"done"}`)},
	)
	projection := domain.RunProjection{ID: "run-1", State: domain.RunCompleted, VerificationSucceeded: true, LastVerificationSeq: 7, LastMutationSeq: 0}
	result, err := ResultFromRun(events, projection, FreeformTaskSpec("fix"), frozenFixtureConfig(t, nil), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Evidence.Trusted || !result.Evidence.VerificationOK {
		t.Fatalf("proposed-only verification must remain untrusted: %#v", result.Evidence)
	}
}
