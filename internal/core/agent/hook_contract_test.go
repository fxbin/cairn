// hook_contract_test.go 锁定 Core 侧钩子动作分发的求值顺序，并与运行时实现做等价对照。
// Author: fxbin
package agent

import (
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/hooks"
)

// TestHookActionsForPreservesRuleOrder 锁定只拼接启用且命中规则的动作，顺序与规则声明一致。
func TestHookActionsForPreservesRuleOrder(t *testing.T) {
	rules := []HookRule{
		{Event: HookEventPostTool, ToolMatch: "read_*", Enabled: true, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "first"}}},
		{Event: HookEventPostTool, ToolMatch: "read_*", Enabled: false, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "disabled"}}},
		{Event: HookEventTurnStart, ToolMatch: "read_*", Enabled: true, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "other-event"}}},
		{Event: HookEventPostTool, ToolMatch: "*", Enabled: true, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "second"}, {Kind: HookActionQueueFollowup, Content: "third"}}},
		{Event: HookEventPostTool, ToolMatch: "write_*", Enabled: true, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "no-match"}}},
	}
	actions := hookActionsFor(rules, HookEventPostTool, "read_file")
	want := []string{"first", "second", "third"}
	if len(actions) != len(want) {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	for index, action := range actions {
		if action.Content != want[index] {
			t.Fatalf("actions[%d] = %q, want %q", index, action.Content, want[index])
		}
	}
}

// TestHookRuleMatchTool 锁定空匹配、通配、前缀通配与 path.Match 语义。
func TestHookRuleMatchTool(t *testing.T) {
	cases := []struct {
		pattern string
		tool    string
		want    bool
	}{
		{"", "read_file", true},
		{"  ", "read_file", true},
		{"*", "read_file", true},
		{" read_* ", "read_file", true},
		{"read_*", "write_file", false},
		{"read_?ile", "read_file", true},
		{"[", "read_file", false},
	}
	for _, item := range cases {
		rule := HookRule{ToolMatch: item.pattern}
		if got := rule.MatchTool(item.tool); got != item.want {
			t.Fatalf("MatchTool(%q, %q) = %v, want %v", item.pattern, item.tool, got, item.want)
		}
	}
}

// TestHookActionsMatchRuntimeEvaluation 以运行时实现为参照，逐事件、逐工具比较动作序列。
func TestHookActionsMatchRuntimeEvaluation(t *testing.T) {
	contractRules := []HookRule{
		{Event: HookEventPreTool, ToolMatch: "exec_*", Enabled: true, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "pre"}}},
		{Event: HookEventPostTool, ToolMatch: "read_*", Enabled: true, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "post"}}},
		{Event: HookEventTurnStart, ToolMatch: "read_*", Enabled: true, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "turn"}}},
		{Event: HookEventNotification, Enabled: true, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "notify"}}},
		{Event: HookEventPostTool, ToolMatch: "*", Enabled: false, Actions: []HookAction{{Kind: HookActionQueueFollowup, Content: "disabled"}}},
	}
	runtimeRules := []hooks.Rule{
		{Event: hooks.EventPreTool, ToolMatch: "exec_*", Enabled: true, Actions: []hooks.Action{{Kind: hooks.ActionQueueFollowup, Content: "pre"}}},
		{Event: hooks.EventPostTool, ToolMatch: "read_*", Enabled: true, Actions: []hooks.Action{{Kind: hooks.ActionQueueFollowup, Content: "post"}}},
		{Event: hooks.EventTurnStart, ToolMatch: "read_*", Enabled: true, Actions: []hooks.Action{{Kind: hooks.ActionQueueFollowup, Content: "turn"}}},
		{Event: hooks.EventNotification, Enabled: true, Actions: []hooks.Action{{Kind: hooks.ActionQueueFollowup, Content: "notify"}}},
		{Event: hooks.EventPostTool, ToolMatch: "*", Enabled: false, Actions: []hooks.Action{{Kind: hooks.ActionQueueFollowup, Content: "disabled"}}},
	}
	events := []HookEvent{HookEventPreTool, HookEventPostTool, HookEventTurnStart, HookEventNotification}
	tools := []string{"", "read_file", "write_file", "exec_command"}
	for _, event := range events {
		for _, tool := range tools {
			got := hookActionsFor(contractRules, event, tool)
			want := hooks.ActionsFor(runtimeRules, hooks.Event(event), tool)
			if len(got) != len(want) {
				t.Fatalf("event %q tool %q: %d actions, want %d", event, tool, len(got), len(want))
			}
			for index := range got {
				if string(got[index].Kind) != string(want[index].Kind) || got[index].Content != want[index].Content || got[index].Reason != want[index].Reason {
					t.Fatalf("event %q tool %q action %d differs: %+v vs %+v", event, tool, index, got[index], want[index])
				}
			}
		}
	}
}

// TestHookActionJSONMatchesRuntimeAction 锁定动作载荷的 JSON 字节与运行时类型一致。
func TestHookActionJSONMatchesRuntimeAction(t *testing.T) {
	contractBytes, err := json.Marshal(HookAction{Kind: HookActionQueueFollowup, Content: "next", Reason: "because"})
	if err != nil {
		t.Fatalf("marshal contract action: %v", err)
	}
	runtimeBytes, err := json.Marshal(hooks.Action{Kind: hooks.ActionQueueFollowup, Content: "next", Reason: "because"})
	if err != nil {
		t.Fatalf("marshal runtime action: %v", err)
	}
	if string(contractBytes) != string(runtimeBytes) {
		t.Fatalf("action JSON differs: %s vs %s", contractBytes, runtimeBytes)
	}
}
