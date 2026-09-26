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

	"fmt"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/tools"
)

// withTestProvider 为测试配置补齐能力卡所需的模型与显式声明。

// encode 将任意值序列化为事件载荷，供手工事件夹具构造 JSON 原文。
func encode(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// putTestFrozenRunConfig 写入符合当前冻结合同的配置制品，供手工事件夹具引用。
func putTestFrozenRunConfig(t *testing.T, stateDir, workspaceHash string, configuration config.Config) artifact.Ref {
	t.Helper()
	fingerprint := configuration.Fingerprint()
	commands := make([]agent.VerificationCommand, 0, len(configuration.Verification.Commands))
	for index, command := range configuration.Verification.Commands {
		arguments, err := json.Marshal(tools.ExecArguments{Argv: command.Argv, CWD: command.CWD, TimeoutMS: 120000, MaxOutputBytes: 256 * 1024, Env: command.Env})
		if err != nil {
			t.Fatal(err)
		}
		commands = append(commands, agent.VerificationCommand{ID: fmt.Sprintf("verification-%d", index+1), Arguments: arguments})
	}
	verificationFingerprint, err := agent.FingerprintVerificationCommands(commands)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := contract.NewFrozenRunConfig(fingerprint, contract.FrozenConfigProjection{
		ConfigVersion: configuration.Version,
		Preset:        configuration.Preset,
		Profile:       configuration.Profile,
		Provider: contract.FrozenProviderProjection{
			Kind: contract.ProviderKindOpenAICompatible, BaseURL: configuration.Provider.BaseURL,
			Model: configuration.Provider.Model, ID: contract.ProviderIDCustom, Admission: configuration.Provider.Admission,
		},
		Policy: contract.PolicyIdentity{
			Write: configuration.Policy.Write, Exec: configuration.Policy.Exec,
			ProtectedPaths: append([]string(nil), configuration.Policy.ProtectedPaths...),
		},
		Verification: contract.FrozenVerificationProjection{Commands: len(commands), Fingerprint: verificationFingerprint},
	})
	if err != nil {
		t.Fatal(err)
	}
	frozenJSON, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.New(filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := artifacts.Put(frozenJSON, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

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

type scriptedProvider struct {
	scripts  [][]ports.ModelEvent
	requests []ports.ModelRequest
}

// Stream 回放固定脚本，并为每个终态补齐能力卡所需的 usage 与 finish_reason；
// 夹具只描述任务语义，协议完成事实由此统一保证。
func (provider *scriptedProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.requests = append(provider.requests, request)
	index := len(provider.requests) - 1
	if index >= len(provider.scripts) {
		return errors.New("unexpected model request")
	}
	toolCalls, usageSeen := false, false
	for _, event := range provider.scripts[index] {
		toolCalls = toolCalls || event.Kind == ports.ToolCallEvent
		usageSeen = usageSeen || event.Kind == ports.UsageEvent
		if event.Kind == ports.Done {
			if !usageSeen {
				if err := emit(ports.ModelEvent{Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}); err != nil {
					return err
				}
			}
			if event.StopReason == "" {
				if toolCalls {
					event.StopReason = "tool_calls"
				} else {
					event.StopReason = "stop"
				}
			}
		}
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
	result, err := providerService(provider).Run(context.Background(), app.RunOptions{Task: "What does this repository keep?", Workspace: workspace, StateDir: stateDir, Config: withTestProvider(config.Defaults())})
	if err != nil || result.Response != "The repository keeps an event journal." {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	inspected, err := providerService(provider).Inspect(context.Background(), workspace, stateDir, withTestProvider(config.Defaults()), result.ID, true)
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
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	service := providerService(provider)
	initial, err := service.Run(context.Background(), app.RunOptions{Task: "create note", Workspace: workspace, StateDir: stateDir, Config: configuration})
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	result, err := service.Resume(context.Background(), app.ResumeOptions{RunID: initial.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true}})
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
	configuration := withTestProvider(config.Defaults())
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
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	appendEvent := func(expected uint64, draft domain.EventDraft) {
		if _, err := journal.Append(context.Background(), expected, draft); err != nil {
			t.Fatal(err)
		}
	}
	configRef := putTestFrozenRunConfig(t, stateDir, workspaceHash, configuration)
	appendEvent(0, domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated, Payload: encode(struct {
		Task              string       `json:"task"`
		Workspace         string       `json:"workspace"`
		ConfigRef         artifact.Ref `json:"config_ref"`
		ConfigFingerprint string       `json:"config_fingerprint"`
	}{Task: "recover", Workspace: workspace, ConfigRef: configRef, ConfigFingerprint: configuration.Fingerprint()})})
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
