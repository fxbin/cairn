//go:build e2e

// Package e2e exercises the four offline-rebuildable Cairn V0 fixture flows.
// 该套件按 AGENTS.md 的验证流程显式启用：go test ./test/e2e -tags=e2e。
package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

type scriptedProvider struct {
	scripts  [][]ports.ModelEvent
	requests []ports.ModelRequest
}

func (provider *scriptedProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.requests = append(provider.requests, request)
	index := len(provider.requests) - 1
	if index >= len(provider.scripts) {
		return errors.New("unexpected model request")
	}
	for _, event := range provider.scripts[index] {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func providerService(provider ports.Provider) app.Service {
	return app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
}

func TestFixtureReadOnlyRepositoryQuestion(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("Cairn keeps an event journal.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"answer.txt"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "The repository keeps an event journal."}, {Kind: ports.Done}},
	}}
	result, err := providerService(provider).Run(context.Background(), app.RunOptions{Task: "What does this repository keep?", Workspace: workspace, StateDir: stateDir, Config: config.Defaults()})
	if err != nil || result.Response != "The repository keeps an event journal." {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	inspected, err := providerService(provider).Inspect(context.Background(), workspace, stateDir, config.Defaults(), result.ID, true)
	if err != nil || inspected.Projection.Phase() != "completed" || len(inspected.Events) == 0 {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	verificationStarted, verificationFinished := false, false
	for _, event := range inspected.Events {
		verificationStarted = verificationStarted || event.Type == domain.EventVerificationStarted
		verificationFinished = verificationFinished || event.Type == domain.EventVerificationFinished
	}
	if !verificationStarted || !verificationFinished {
		t.Fatalf("read-only completion skipped verification evidence: %#v", inspected.Events)
	}
}

func TestFixtureApprovedSingleFilePatch(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"note.txt","mode":"create","content":"created"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "created and verified"}, {Kind: ports.Done}},
	}}
	configuration := config.Defaults()
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	service := providerService(provider)
	initial, err := service.Run(context.Background(), app.RunOptions{Task: "create note", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	result, err := service.Resume(context.Background(), app.ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: agent.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
	if err != nil || result.Response != "created and verified" {
		t.Fatalf("resume=%#v err=%v", result, err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "note.txt"))
	if err != nil || string(content) != "created" {
		t.Fatalf("content=%q err=%v", content, err)
	}
}

func TestFixtureVerificationFailureThenRepair(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	verificationScript := filepath.Join(workspace, "verify.sh")
	if err := os.WriteFile(verificationScript, []byte("#!/bin/sh\ngrep -q fixed target.txt\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("broken"))
	update := map[string]any{
		"path":            "target.txt",
		"mode":            "update",
		"expected_sha256": "sha256:" + hex.EncodeToString(digest[:]),
		"replacements":    []map[string]any{{"old": "broken", "new": "fixed", "occurrence": 1}},
	}
	updateArguments, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "create", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"target.txt","mode":"create","content":"broken"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "initial attempt"}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "repair", Name: "apply_patch", Arguments: updateArguments}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "fixed and verified"}, {Kind: ports.Done}},
	}}
	configuration := config.Defaults()
	configuration.Policy.Write, configuration.Policy.Exec = "allow", "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"./verify.sh"}}}
	result, err := providerService(provider).Run(context.Background(), app.RunOptions{Task: "repair target", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || result.Response != "fixed and verified" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(provider.requests) != 4 {
		t.Fatalf("model requests=%d", len(provider.requests))
	}
	foundFeedback := false
	for _, message := range provider.requests[2].Messages {
		foundFeedback = foundFeedback || message.Role == "user" && strings.Contains(message.Content, "Verification command")
	}
	if !foundFeedback {
		t.Fatalf("repair request did not contain verification feedback: %#v", provider.requests[2].Messages)
	}
}

func TestFixtureReconcileDanglingPatchWithoutRetryingIt(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(canonical))
	workspaceHash := "sha256:" + hex.EncodeToString(digest[:])
	journal, err := sqlite.Open(filepath.Join(stateDir, "workspaces", workspaceHash, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.Defaults()
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"recover","workspace":"` + workspace + `","config_fingerprint":"` + configuration.Fingerprint() + `"}`)})
	appendEvent(1, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted})
	appendEvent(2, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventTurnOpened, TurnID: &turnID})
	appendEvent(3, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"patch","tool_name":"apply_patch","arguments":{"path":"target.txt","mode":"create","content":"x"}}`)})
	appendEvent(4, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)})
	appendEvent(5, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)})
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "reconciled and verified"}, {Kind: ports.Done}}}}
	service := providerService(provider)
	result, err := service.Reconcile(context.Background(), app.ReconcileOptions{RunID: runID, OperationID: operationID, Verdict: "not_applied", Note: "crash before write", Evidence: "operator checked workspace", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || result.Response != "reconciled and verified" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, runID, true)
	if err != nil || inspected.Projection.Phase() != "completed" {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	resumed := false
	for _, event := range inspected.Events {
		resumed = resumed || event.Type == domain.EventRunResumed
		if event.Type == domain.EventOperationStarted && event.Seq > 6 && strings.Contains(string(event.Payload), "apply_patch") {
			t.Fatalf("reconciliation retried original patch: %#v", event)
		}
	}
	if !resumed {
		t.Fatalf("reconciliation did not record a post-verdict resume boundary: %#v", inspected.Events)
	}
}
