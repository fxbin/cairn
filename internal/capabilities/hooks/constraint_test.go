// [INPUT]: 原规则合同、可变来源切片与空能力实例。
// [OUTPUT]: 验证规则校验、冻结来源、关闭缺失与空配置行为。
// [POS]: Hooks授权适配器回归；不创建工具或网络连接。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package hooks

import (
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

// TestConstraintValidatesAndFreezesRules 外部修改不能撤销已冻结的拒绝规则。
func TestConstraintValidatesAndFreezesRules(t *testing.T) {
	rules := []Rule{{ID: "deny", Event: EventPreTool, Effect: EffectDeny, Reason: "frozen", Enabled: true, Actions: []Action{{Kind: ActionQueueFollowup, Content: "frozen"}}}}
	constraint, err := NewConstraint(rules)
	if err != nil {
		t.Fatal(err)
	}
	rules[0].Effect = EffectAllow
	rules[0].Reason = "changed"
	rules[0].Actions[0].Content = "changed"
	got, err := constraint.ConstrainTool("read_file", contract.AuthorizationAllow)
	if err != nil || got.Effect != contract.AuthorizationDeny || got.RuleID != "hook_deny" || got.Summary != "frozen" {
		t.Fatalf("frozen decision=%+v err=%v", got, err)
	}
	if actions := ActionsFor(constraint.rules, EventPreTool, "read_file"); len(actions) != 1 || actions[0].Content != "frozen" {
		t.Fatalf("frozen actions=%v", actions)
	}
	for _, rule := range []Rule{{ID: "bad", Event: "unknown"}, {ID: "bad", Event: EventPreTool, Effect: "unknown"}, {ID: "bad", Event: EventPreTool, Actions: []Action{{Kind: "exec", Content: "forbidden"}}}} {
		if _, err := NewConstraint([]Rule{rule}); err == nil {
			t.Fatalf("invalid hook accepted: %+v", rule)
		}
	}
}

// TestConstraintEmptyAndMissing 关闭能力不建实例，错误接线的空实例失败关闭。
func TestConstraintEmptyAndMissing(t *testing.T) {
	constraint, err := NewConstraint(nil)
	if err != nil || constraint != nil {
		t.Fatalf("empty constraint=%v err=%v", constraint, err)
	}
	if _, err := constraint.ConstrainTool("read_file", contract.AuthorizationAllow); err == nil {
		t.Fatal("missing constraint accepted")
	}
}
