package hooks

import "testing"

// TestEvaluateToolDenyWins 锁定钩子 Deny 优先且不可放宽 Policy。
func TestEvaluateToolDenyWins(t *testing.T) {
	rules := []Rule{
		{ID: "h1", Event: EventPreTool, ToolMatch: "exec_*", Effect: EffectDeny, Reason: "block exec", Enabled: true},
		{ID: "h2", Event: EventPreTool, ToolMatch: "exec_command", Effect: EffectAllow, Enabled: true},
	}
	d := EvaluateTool(rules, "exec_command", true)
	if d.Effect != EffectDeny || d.HookID != "h1" {
		t.Fatalf("decision = %+v", d)
	}
}

// TestEvaluateToolCannotRelaxPolicy Policy=ask 时钩子 allow 不得变 allow。
func TestEvaluateToolCannotRelaxPolicy(t *testing.T) {
	rules := []Rule{{ID: "h", Event: EventPreTool, Effect: EffectAllow, Enabled: true}}
	d := EvaluateTool(rules, "read_file", false)
	if d.Effect != EffectAsk {
		t.Fatalf("decision = %+v", d)
	}
}

// TestEvaluateToolAskTightens 钩子可将 allow 升为 ask。
func TestEvaluateToolAskTightens(t *testing.T) {
	rules := []Rule{{ID: "h", Event: EventPreTool, ToolMatch: "web_*", Effect: EffectAsk, Reason: "net", Enabled: true}}
	d := EvaluateTool(rules, "web_fetch", true)
	if d.Effect != EffectAsk || d.Reason != "net" {
		t.Fatalf("decision = %+v", d)
	}
	if EvaluateTool(rules, "read_file", true).Effect != EffectAllow {
		t.Fatal("non-matching tool must stay allow")
	}
}

// TestValidateRule 锁定非法事件/动作。
func TestValidateRule(t *testing.T) {
	if err := ValidateRule(Rule{ID: "x", Event: "boot", Enabled: true}); err == nil {
		t.Fatal("expected event error")
	}
	if err := ValidateRule(Rule{ID: "x", Event: EventRunFinished, Actions: []Action{{Kind: "rm_rf"}}, Enabled: true}); err == nil {
		t.Fatal("expected action error")
	}
	if err := ValidateRule(Rule{ID: "x", Event: EventRunFinished, Actions: []Action{{Kind: ActionQueueFollowup, Content: "go"}}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

// TestActionsForEvent 覆盖 run_finished 动作收集。
func TestActionsForEvent(t *testing.T) {
	rules := []Rule{{
		ID: "h", Event: EventRunFinished, Enabled: true,
		Actions: []Action{{Kind: ActionQueueFollowup, Content: "next"}},
	}}
	actions := ActionsFor(rules, EventRunFinished, "")
	if len(actions) != 1 || actions[0].Content != "next" {
		t.Fatalf("actions = %+v", actions)
	}
	if len(ActionsFor(rules, EventPreTool, "x")) != 0 {
		t.Fatal("wrong event must not match")
	}
}
