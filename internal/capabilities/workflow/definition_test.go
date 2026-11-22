// [INPUT]: 使用冻结验收合同、工作流定义及事件草稿。
// [OUTPUT]: 回归合同快照、验证步骤边界和版本化尝试重放。
// [POS]: 纯合同测试。能力层以步骤锚点重放独立审查。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件验证独立审查模板的冻结与回放。
// Author: fxbin
package workflow

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

const definitionTestHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func verificationDraft(t *testing.T) DefinitionDraft {
	t.Helper()
	acceptance, err := contract.NewAcceptanceContract([]contract.AcceptanceCriterion{{
		ID: "build", Statement: "项目能构建", Required: true, Checks: []string{"compile"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return DefinitionDraft{
		ID: "verification", Name: "验证", Verification: &acceptance,
		Nodes: []Node{
			{ID: "worker", Kind: StepKindExecute, Prompt: "修复项目", MaxAttempts: 2},
			{ID: "program", Kind: StepKindVerify, RetryFrom: "worker", MaxAttempts: 2},
			{ID: "review", Kind: StepKindReview, RetryFrom: "worker", MaxAttempts: 2},
			{ID: "accept", Kind: StepKindAccept},
		},
	}
}

func TestVerificationDefinitionFreezesAcceptance(t *testing.T) {
	draft := verificationDraft(t)
	definition, err := NewDefinition(definitionTestHash, draft, 1)
	if err != nil {
		t.Fatal(err)
	}
	draft.Verification.Criteria[0].Checks[0] = "changed"
	if err := definition.Validate(); err != nil {
		t.Fatalf("draft mutation changed the frozen definition: %v", err)
	}
	frozen, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	var restored Definition
	if err := json.Unmarshal(frozen, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(); err != nil {
		t.Fatalf("restored definition is invalid: %v", err)
	}
	if restored.Verification.Digest != definition.Verification.Digest || restored.Verification.Criteria[0].Checks[0] != "compile" {
		t.Fatalf("restored acceptance differs: %+v", restored.Verification)
	}

	legacy := DefinitionDraft{
		ID: "legacy", Name: "原工作流",
		Nodes: []Node{{ID: "worker", Kind: StepKindExecute, Prompt: "读取说明"}},
	}
	old, err := NewDefinition(definitionTestHash, legacy, 1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"verification":`)) {
		t.Fatalf("legacy definition serializes a new field: %s", encoded)
	}
}

func TestVerificationDefinitionRejectsUnboundReview(t *testing.T) {
	cases := []struct {
		name   string
		change func(*DefinitionDraft)
	}{
		{"missing acceptance", func(draft *DefinitionDraft) { draft.Verification = nil }},
		{"invalid acceptance", func(draft *DefinitionDraft) { draft.Verification.Digest = definitionTestHash }},
		{"read only worker", func(draft *DefinitionDraft) { draft.Nodes[0].ReadOnly = true }},
		{"missing program check", func(draft *DefinitionDraft) { draft.Nodes[1].Kind = StepKindAccept }},
		{"review instructions", func(draft *DefinitionDraft) { draft.Nodes[2].Prompt = "自行选择验收标准" }},
		{"review command", func(draft *DefinitionDraft) { draft.Nodes[2].Argv = []string{"check"} }},
		{"review model override", func(draft *DefinitionDraft) { draft.Nodes[2].Model = "override" }},
		{"review protection override", func(draft *DefinitionDraft) { draft.Nodes[2].ReadOnly = true }},
		{"review inputs", func(draft *DefinitionDraft) { draft.Nodes[2].Inputs = []string{"worker"} }},
		{"review rework outside worker", func(draft *DefinitionDraft) { draft.Nodes[2].RetryFrom = "program" }},
		{"gate rework", func(draft *DefinitionDraft) { draft.Nodes[3].RetryFrom = "worker" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			draft := verificationDraft(t)
			test.change(&draft)
			if _, err := NewDefinition(definitionTestHash, draft, 1); err == nil {
				t.Fatal("invalid verification definition accepted")
			}
		})
	}
}

func TestVersionedWorkflowReplaysReviewAttempt(t *testing.T) {
	draft := verificationDraft(t)
	steps := make([]StepSpec, 0, len(draft.Nodes))
	for index, node := range draft.Nodes {
		steps = append(steps, StepSpec{ID: node.ID, Index: index, Kind: node.Kind, MaxAttempts: 2, RetryFrom: node.RetryFrom})
	}
	id := NewID()
	created := RunCreatedPayload{
		Steps: steps, PlanSHA256: definitionTestHash, ConfigFingerprint: definitionTestHash,
		AuthorizationSource: "user", SessionID: "session-test",
	}
	event := func(kind EventType, body any) EventDraft {
		return EventDraft{WorkflowID: id, WorkspaceHash: definitionTestHash, Type: kind, Payload: payload(t, body)}
	}
	if _, err := NewEvent(event(EventRunCreated, created), 1, GenesisHash); err == nil {
		t.Fatal("legacy workflow accepted an independent review")
	}
	created.Definition = &DefinitionRef{ID: draft.ID, Revision: 1, Artifact: contract.Ref{
		SHA256: definitionTestHash, Bytes: 1, MediaType: "application/json",
	}}
	created.Steps[2].Inputs = []string{"worker"}
	if _, err := NewEvent(event(EventRunCreated, created), 1, GenesisHash); err == nil {
		t.Fatal("versioned replay accepted review inputs outside the host binding")
	}
	created.Steps[2].Inputs = nil
	events := []EventDraft{event(EventRunCreated, created)}
	for _, step := range steps[:3] {
		events = append(events,
			event(EventStepStarted, StepStartedPayload{
				StepID: step.ID, Attempt: 1, RunID: "run-" + step.ID,
				CreationEventID: "created-" + step.ID, ConfigFingerprint: definitionTestHash,
			}),
			event(EventStepFinished, StepFinishedPayload{
				StepID: step.ID, Attempt: 1, State: StepSucceeded,
				Output:   StepIO{Ref: definitionTestHash, SHA: definitionTestHash},
				Terminal: &RunAnchor{Sequence: 2, EventHash: strings.Repeat("a", 64)},
			}),
		)
	}
	snapshot, err := Project(buildChain(t, events...))
	if err != nil {
		t.Fatal(err)
	}
	review := snapshot.Attempts["review"]
	if snapshot.ResumeIndex != 3 || snapshot.Kinds["review"] != StepKindReview || len(review) != 1 ||
		review[0].RunID != "run-review" || review[0].State != StepSucceeded || review[0].Terminal == nil {
		t.Fatalf("review anchor did not replay: %+v", snapshot)
	}
}
