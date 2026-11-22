// verifier_test.go 回归只读 Verifier 配置派生与结构化 Finding 解析。
//
// Author: fxbin
package app

import (
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

func verifierTestRequest(t *testing.T) VerifierRequest {
	t.Helper()
	accepted, err := contract.NewAcceptanceContract([]contract.AcceptanceCriterion{
		{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}},
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	parent := withTestProvider(config.Defaults())
	parent.Policy.Exec = "allow"
	parent.Verification.Commands = []config.VerificationCommand{{ID: "build", Argv: []string{"/usr/bin/true"}}}
	return VerifierRequest{
		TargetRunID: "run-worker", Workspace: "/workspace", StateDir: "/state",
		Config: parent, ParentRunID: "run-parent", CriterionIDs: []string{"compile"},
		Acceptance: accepted,
	}
}

func TestVerifierConfigIsReadOnly(t *testing.T) {
	request := verifierTestRequest(t)
	child, err := verifierConfig(request.Config)
	if err != nil {
		t.Fatalf("verifier config: %v", err)
	}
	if child.Preset != config.PresetReadOnly || child.Policy.Write != "deny" || child.Policy.Exec != "deny" {
		t.Fatalf("verifier config is not read-only: %+v", child.Policy)
	}
	if len(child.Verification.Commands) != 0 || child.Tools.DelegateAsync {
		t.Fatal("verifier config retained verification commands or delegation")
	}
	// 父配置不能被污染。
	if request.Config.Preset == config.PresetReadOnly || len(request.Config.Verification.Commands) != 1 {
		t.Fatal("parent config was mutated")
	}
}

func TestVerifierConfigForcesReadOnlyFromWritableBase(t *testing.T) {
	// 即使父配置显式允许写与执行，派生出的 Verifier 配置也必须强制 deny。
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write = "allow"
	configuration.Policy.Exec = "allow"
	configuration.Tools.LspDiagnostics = true
	child, err := verifierConfig(configuration)
	if err != nil {
		t.Fatalf("verifier config: %v", err)
	}
	if child.Policy.Write != "deny" || child.Policy.Exec != "deny" {
		t.Fatalf("writable base leaked into verifier config: %+v", child.Policy)
	}
	if child.Tools.LspDiagnostics {
		t.Fatal("independent verifier inherited host diagnostics")
	}
}

func TestParseVerifierOutput(t *testing.T) {
	request := verifierTestRequest(t)
	raw := "```json\n" + `{"findings":[{"id":"f-1","target_id":"obj-1","category":"correctness","severity":"high","description":"发现问题","status":"confirmed","criterion_id":"compile","evidence_refs":[{"sha256":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bytes":1,"media_type":"application/json","truncated_preview":""}]}]}` + "\n```"
	findings, err := parseVerifierOutput(raw, request)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings) != 1 || findings[0].ID != "f-1" || findings[0].Status != contract.FindingProposed {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	// Verifier 自报的 confirmed 不得被信任，必须降级为待核验 Proposed 且清空复核。
	if findings[0].Version != contract.FindingVersion || len(findings[0].Reviews) != 0 {
		t.Fatalf("finding defaults not applied: %+v", findings[0])
	}
}

func TestParseVerifierOutputForcesTargetAndFallsBackCriteria(t *testing.T) {
	request := verifierTestRequest(t)
	// CriterionIDs 留空，应回退到 Acceptance.Criteria（compile）。
	request.CriterionIDs = nil
	// 模型自报 target_id 与 status 都被强制改写。
	raw := `{"findings":[{"id":"f-1","target_id":"attacker-target","category":"c","severity":"low","description":"d","status":"confirmed","criterion_id":"compile"}]}`
	findings, err := parseVerifierOutput(raw, request)
	if err != nil {
		t.Fatalf("parse findings with criterion fallback: %v", err)
	}
	if findings[0].TargetID != "run-worker" {
		t.Fatalf("target_id was not forced to target run: %q", findings[0].TargetID)
	}
	if findings[0].Status != contract.FindingProposed {
		t.Fatalf("status was not downgraded: %s", findings[0].Status)
	}
	// 回退后未绑定的标准仍被拒绝。
	unbound := `{"findings":[{"id":"f-2","target_id":"o","category":"c","severity":"low","description":"d","criterion_id":"unknown"}]}`
	if _, err := parseVerifierOutput(unbound, request); err == nil {
		t.Fatal("unbound criterion passed with fallback criteria")
	}
}

func TestParseVerifierOutputRejectsEmptyCriteria(t *testing.T) {
	request := verifierTestRequest(t)
	request.CriterionIDs = nil
	request.Acceptance, _ = contract.NewAcceptanceContract(nil) // 构造失败返回零值合同
	raw := `{"findings":[{"id":"f-1","target_id":"o","category":"c","severity":"low","description":"d"}]}`
	if _, err := parseVerifierOutput(raw, request); err == nil {
		t.Fatal("verifier output with no criteria available was accepted")
	}
}

func TestParseVerifierOutputEmptyFindings(t *testing.T) {
	request := verifierTestRequest(t)
	findings, err := parseVerifierOutput(`{"findings":[]}`, request)
	if err != nil {
		t.Fatalf("parse empty findings: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("empty findings expected, got %d", len(findings))
	}
}

func TestParseVerifierOutputFailsClosed(t *testing.T) {
	request := verifierTestRequest(t)
	cases := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"missing findings array", `{}`},
		{"null findings array", `{"findings":null}`},
		{"multiple objects", `{"findings":[]} {"findings":[]}`},
		{"repeated findings array", `{"findings":[],"findings":[]}`},
		{"array instead of object", `[{"findings":[]}]`},
		{"no json", "just text"},
		{"bad json", `{"findings":[`},
		{"unbound criterion", `{"findings":[{"id":"f","target_id":"o","category":"c","severity":"low","description":"d","status":"proposed","criterion_id":"unknown"}]}`},
		{"bad severity", `{"findings":[{"id":"f","target_id":"o","category":"c","severity":"urgent","description":"d"}]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseVerifierOutput(testCase.text, request); err == nil {
				t.Fatal("invalid verifier output was accepted")
			}
		})
	}
}

func TestVerifierFinalTextPrefersRunCompleted(t *testing.T) {
	turnID := domain.NewTurnID()
	events := []domain.Event{
		{RunID: "child", Type: domain.EventAssistantMessageAppended, TurnID: &turnID, Payload: mustJSON(t, domain.AssistantMessagePayload{Text: "早先文本"})},
		{RunID: "child", Type: domain.EventRunCompleted, Payload: mustJSON(t, domain.RunCompletedPayload{FinalResponse: `{"findings":[]}`})},
	}
	if got := verifierFinalText(events); !strings.Contains(got, "findings") {
		t.Fatalf("final text = %q, want run_completed response", got)
	}
}

func TestVerifierCriteriaRejectsUnknownOrRepeatedSelection(t *testing.T) {
	request := verifierTestRequest(t)
	for _, ids := range [][]string{{"unknown"}, {"compile", "compile"}} {
		request.CriterionIDs = ids
		if _, err := verifierCriteria(request); err == nil {
			t.Fatalf("invalid criterion selection accepted: %v", ids)
		}
	}
}
