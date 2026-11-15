// [INPUT]: 通用能力规则替身与真实 hooks 求值。
// [OUTPUT]: 验证固定工具优先、统一收紧与错误阻断。
// [POS]: Policy 规则槽合同回归，不依赖能力名称。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package policy

import (
	"encoding/json"
	"errors"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/runtime/hooks"
	"testing"
)

type toolRuleProbe struct {
	calls   int
	claimed bool
	err     error
	effect  Effect
}

func (rule *toolRuleProbe) DecideTool(string, json.RawMessage) (contract.AuthorizationDecision, bool, error) {
	rule.calls++
	return Decision{Effect: rule.effect, RuleID: "capability", Fingerprint: "bound", ExecutionBinding: "identity"}, rule.claimed, rule.err
}

// TestToolRulesKeepBuiltinPriorityAndHooks 能力不能覆盖固定工具，候选必须接受 hooks 收紧。
func TestToolRulesKeepBuiltinPriorityAndHooks(t *testing.T) {
	rule := &toolRuleProbe{claimed: true, effect: Allow}
	gate := &Gate{ToolRules: []ToolRule{rule}}
	gate.SetHooks([]hooks.Rule{{ID: "restrict", Event: hooks.EventPreTool, ToolMatch: "*", Effect: hooks.EffectDeny, Enabled: true}})
	decision, err := gate.Decide("read_file", json.RawMessage(`{}`))
	if err != nil || rule.calls != 0 || decision.Effect != Deny || decision.RuleID != "hook_deny" {
		t.Fatalf("builtin decision=%+v calls=%d err=%v", decision, rule.calls, err)
	}
	for _, hook := range []hooks.Effect{hooks.EffectAsk, hooks.EffectDeny, hooks.EffectAllow} {
		gate.SetHooks([]hooks.Rule{{ID: "restrict", Event: hooks.EventPreTool, ToolMatch: "*", Effect: hook, Enabled: true}})
		decision, err = gate.Decide("extension", json.RawMessage(`{}`))
		if err != nil || string(decision.Effect) != string(hook) || decision.Fingerprint != "bound" || decision.ExecutionBinding != "identity" {
			t.Fatalf("candidate=%+v err=%v", decision, err)
		}
	}
	rule.effect = Deny
	decision, err = gate.Decide("extension", json.RawMessage(`{}`))
	if err != nil || decision.Effect != Deny {
		t.Fatal("allow hook expanded rule denial")
	}
}

// TestToolRulesRejectNilAndErrors 未认领继续，空规则及错误阻断授权。
func TestToolRulesRejectNilAndErrors(t *testing.T) {
	skipped := &toolRuleProbe{claimed: false}
	claimed := &toolRuleProbe{claimed: true, effect: Ask}
	gate := &Gate{ToolRules: []ToolRule{skipped, claimed}}
	got, err := gate.Decide("extension", json.RawMessage(`{}`))
	if err != nil || got.Effect != Ask || skipped.calls != 1 || claimed.calls != 1 {
		t.Fatal("claim ordering changed")
	}
	gate.ToolRules = []ToolRule{skipped}
	got, err = gate.Decide("extension", json.RawMessage(`{}`))
	if err != nil || got.Effect != Deny || got.RuleID != "unknown_tool" {
		t.Fatal("unclaimed tool allowed")
	}
	gate.ToolRules = []ToolRule{nil}
	if _, err = gate.Decide("extension", json.RawMessage(`{}`)); err == nil {
		t.Fatal("nil rule accepted")
	}
	sentinel := errors.New("rule failed")
	claimed.err = sentinel
	gate.ToolRules = []ToolRule{claimed}
	if _, err = gate.Decide("extension", json.RawMessage(`{}`)); !errors.Is(err, sentinel) {
		t.Fatal("rule error lost")
	}
}
