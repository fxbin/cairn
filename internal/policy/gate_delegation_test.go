package policy

import (
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/tools"
)

// TestGateClassifiesDelegationWithExecMode 锁定 delegate_task 与命令执行同级：
// 默认 ask，exec=allow 时放行，非法参数拒绝。
func TestGateClassifiesDelegationWithExecMode(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	askGate, err := New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := askGate.Decide("delegate_task", json.RawMessage(`{"task":"do work"}`))
	if err != nil || decision.Effect != Ask || decision.RuleID != "subagent_delegation" || decision.Fingerprint == "" {
		t.Fatalf("decision = %#v, %v", decision, err)
	}
	allowGate, err := New("ask", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := allowGate.Decide("delegate_task", json.RawMessage(`{"task":"do work","max_turns":5}`))
	if err != nil || allowed.Effect != Allow {
		t.Fatalf("allowed decision = %#v, %v", allowed, err)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"max_turns":5}`),
		json.RawMessage(`{"task":"   "}`),
		json.RawMessage(`{"task":"do","max_turns":1000}`),
		json.RawMessage(`{"task":123}`),
	} {
		denied, err := askGate.Decide("delegate_task", raw)
		if err != nil || denied.Effect != Deny || denied.RuleID != "invalid_delegation" {
			t.Fatalf("invalid arguments must deny: raw=%s decision=%#v err=%v", raw, denied, err)
		}
	}
}
