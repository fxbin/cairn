// [INPUT]: 通用约束替身与真实Gate候选判定。
// [OUTPUT]: 验证只收紧、错误关闭、顺序及审批身份不变。
// [POS]: Policy扩展边界回归；不依赖具体能力实现。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package policy

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

type constraintProbe struct {
	proposed contract.AuthorizationConstraint
	err      error
	seen     []Effect
}

func (probe *constraintProbe) ConstrainTool(_ string, current Effect) (contract.AuthorizationConstraint, error) {
	probe.seen = append(probe.seen, current)
	return probe.proposed, probe.err
}

// TestToolConstraintsCannotExpandAuthorization 不信任扩展自称只收紧。
func TestToolConstraintsCannotExpandAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name          string
		current, next Effect
		wantErr       bool
	}{
		{"allow_to_ask", Allow, Ask, false}, {"allow_to_deny", Allow, Deny, false},
		{"ask_to_deny", Ask, Deny, false}, {"deny_unchanged", Deny, Deny, false},
		{"ask_to_allow", Ask, Allow, true}, {"deny_to_ask", Deny, Ask, true},
		{"deny_to_allow", Deny, Allow, true}, {"unknown", Allow, "unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &constraintProbe{proposed: contract.AuthorizationConstraint{Effect: tc.next, RuleID: "constraint", Summary: "constrained"}}
			gate := &Gate{ToolRules: []ToolRule{&toolRuleProbe{claimed: true, effect: tc.current}}, ToolConstraints: []ToolConstraint{probe}}
			got, err := gate.Decide("extension", json.RawMessage(`{}`))
			if (err != nil) != tc.wantErr {
				t.Fatalf("decision=%+v err=%v", got, err)
			}
			want := tc.next
			if tc.wantErr {
				want = tc.current
			}
			if got.Effect != want || got.Fingerprint != "bound" || got.ExecutionBinding != "identity" {
				t.Fatalf("authorization identity or effect changed: %+v", got)
			}
			if len(probe.seen) != 1 || probe.seen[0] != tc.current {
				t.Fatalf("constraint inputs=%v", probe.seen)
			}
		})
	}
}

// TestToolConstraintsOrderAndFailures 下游只观察已收紧效果；缺失和错误阻断授权。
func TestToolConstraintsOrderAndFailures(t *testing.T) {
	first := &constraintProbe{proposed: contract.AuthorizationConstraint{Effect: Ask}}
	second := &constraintProbe{proposed: contract.AuthorizationConstraint{Effect: Deny}}
	gate := &Gate{ToolRules: []ToolRule{&toolRuleProbe{claimed: true, effect: Allow}}, ToolConstraints: []ToolConstraint{first, second}}
	got, err := gate.Decide("extension", json.RawMessage(`{}`))
	if err != nil || got.Effect != Deny || first.seen[0] != Allow || second.seen[0] != Ask {
		t.Fatalf("order=%v/%v decision=%+v err=%v", first.seen, second.seen, got, err)
	}
	sentinel := errors.New("constraint unavailable")
	for _, constraint := range []ToolConstraint{nil, &constraintProbe{err: sentinel}} {
		gate.ToolConstraints = []ToolConstraint{constraint}
		got, err = gate.Decide("extension", json.RawMessage(`{}`))
		if err == nil || got.Effect != Allow || got.Fingerprint != "bound" || got.ExecutionBinding != "identity" {
			t.Fatalf("constraint failure lost: %+v %v", got, err)
		}
		if constraint != nil && !errors.Is(err, sentinel) {
			t.Fatalf("constraint error identity lost: %v", err)
		}
	}
	empty := &constraintProbe{proposed: contract.AuthorizationConstraint{RuleID: "ignored", Summary: "ignored"}}
	gate.ToolConstraints = []ToolConstraint{empty}
	got, err = gate.Decide("extension", json.RawMessage(`{}`))
	if err != nil || got.RuleID != "capability" || got.Summary != "" {
		t.Fatalf("empty constraint changed metadata: %+v %v", got, err)
	}
}
