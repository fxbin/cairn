// acceptance_test.go 回归验收合同在冻结验证计划中的覆盖门禁与证据生成。
//
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/runtime/artifact"
)

func testAcceptance(t *testing.T) contract.AcceptanceContract {
	t.Helper()
	accepted, err := contract.NewAcceptanceContract([]contract.AcceptanceCriterion{
		{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}},
		{ID: "tests", Statement: "测试通过", Required: true, Checks: []string{"test"}},
	})
	if err != nil {
		t.Fatalf("acceptance contract: %v", err)
	}
	return accepted
}

func TestCreateVerificationPlanEmbedsAcceptance(t *testing.T) {
	accepted := testAcceptance(t)
	loop := Loop{
		Verification: []VerificationCommand{
			{ID: "build", Arguments: json.RawMessage(`{"argv":["go","build"]}`)},
			{ID: "test", Arguments: json.RawMessage(`{"argv":["go","test"]}`)},
		},
		Acceptance: &accepted,
	}
	plan, _, err := loop.createVerificationPlan("verify-1", 3)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if plan.Acceptance == nil || plan.Acceptance.Digest != accepted.Digest {
		t.Fatalf("acceptance was not embedded: %+v", plan.Acceptance)
	}
}

func TestCreateVerificationPlanRejectsUncoveredCheck(t *testing.T) {
	accepted := testAcceptance(t)
	loop := Loop{
		Verification: []VerificationCommand{
			{ID: "build", Arguments: json.RawMessage(`{"argv":["go","build"]}`)},
		},
		Acceptance: &accepted,
	}
	if _, _, err := loop.createVerificationPlan("verify-1", 3); err == nil {
		t.Fatal("uncovered check was accepted into the frozen plan")
	}
}

func TestCreateVerificationPlanRejectsEmptyCommandsWithContract(t *testing.T) {
	accepted := testAcceptance(t)
	loop := Loop{Acceptance: &accepted}
	if _, _, err := loop.createVerificationPlan("verify-1", 0); err == nil {
		t.Fatal("contract with no verification commands was accepted")
	}
}

// TestAppendSuccessfulVerificationWritesZeroMutationSeq 锁定 P1 回归：验收场景
// 即使 last_mutation_seq==0 也必须写 verified_mutation_seq，否则零变更验收任务
// 会在读取侧被误判为状态损坏而永久无法完成。
func TestAppendSuccessfulVerificationWritesZeroMutationSeq(t *testing.T) {
	accepted := testAcceptance(t)
	artifacts, err := artifact.New(filepath.Join(t.TempDir(), "artifacts"), 1024)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	journal := memory.New()
	runID, turnID := domain.NewRunID(), domain.NewTurnID()
	planRef := &contract.Ref{SHA256: "sha256:" + strings.Repeat("a", 64), Bytes: 10, MediaType: "application/json"}
	drafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "sha256:ws", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"zero mutation","workspace":"/workspace"}`)},
		{RunID: runID, WorkspaceHash: "sha256:ws", Type: domain.EventRunStarted},
		{RunID: runID, WorkspaceHash: "sha256:ws", Type: domain.EventTurnOpened, TurnID: &turnID, Payload: json.RawMessage(`{"turn":1}`)},
		{RunID: runID, WorkspaceHash: "sha256:ws", Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: mustJSON(struct {
			VerificationID       string        `json:"verification_id"`
			LastMutationSeq      uint64        `json:"last_mutation_seq"`
			CommandsFingerprint  string        `json:"commands_fingerprint"`
			WorkspaceObservation string        `json:"workspace_observation"`
			PlanRef              *contract.Ref `json:"plan_ref"`
		}{VerificationID: "verify-1", LastMutationSeq: 0, CommandsFingerprint: "sha256:fp", WorkspaceObservation: "sha256:obs", PlanRef: planRef})},
	}
	ctx := context.Background()
	for index, draft := range drafts {
		if _, err := journal.Append(ctx, uint64(index), draft); err != nil {
			t.Fatalf("append prefix %d: %v", index, err)
		}
	}
	events, err := journal.Load(ctx, runID)
	if err != nil {
		t.Fatalf("load prefix: %v", err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatalf("replay prefix: %v", err)
	}
	loop := Loop{Journal: journal, Artifacts: artifacts}
	commands := []VerificationCommand{
		{ID: "build", Arguments: json.RawMessage(`{"argv":["true"]}`)},
		{ID: "test", Arguments: json.RawMessage(`{"argv":["true"]}`)},
	}
	err = loop.appendSuccessfulVerification(ctx, &projection, RunInput{RunID: runID, WorkspaceHash: "sha256:ws"}, turnID, "verify-1", planRef, "sha256:fp", commands, &accepted, "sha256:obs", 0)
	if err != nil {
		t.Fatalf("append successful verification: %v", err)
	}
	events, err = journal.Load(ctx, runID)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	var finished *domain.Event
	for index := range events {
		if events[index].Type == domain.EventVerificationFinished {
			finished = &events[index]
		}
	}
	if finished == nil {
		t.Fatal("verification_finished was not appended")
	}
	var payload domain.VerificationFinishedPayload
	if err := json.Unmarshal(finished.Payload, &payload); err != nil {
		t.Fatalf("decode finish payload: %v", err)
	}
	if payload.VerifiedMutationSeq == nil {
		t.Fatal("verified_mutation_seq was omitted for zero-mutation acceptance verification")
	}
	if *payload.VerifiedMutationSeq != 0 {
		t.Fatalf("verified_mutation_seq = %d, want 0", *payload.VerifiedMutationSeq)
	}
	if len(payload.EvidenceRef) == 0 || string(payload.EvidenceRef) == "null" {
		t.Fatal("evidence_ref was not written")
	}
	if payload.WorkspaceObservation != "sha256:obs" {
		t.Fatalf("workspace_observation = %q, want sha256:obs", payload.WorkspaceObservation)
	}
}
