// rework_test.go 回归自动返工资格检查的强制停止条件。
//
// Author: fxbin
package app

import (
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

func TestEvaluateReworkEligible(t *testing.T) {
	decision := EvaluateRework([]string{"f-1"}, 2, false, []string{"impl"}, false)
	if !decision.Eligible {
		t.Fatalf("expected eligible, reason=%s", decision.Reason)
	}
}

func TestEvaluateReworkStopConditions(t *testing.T) {
	cases := []struct {
		name       string
		findings   []string
		remaining  int
		unresolved bool
		targets    []string
		scope      bool
	}{
		{"no finding", nil, 2, false, []string{"impl"}, false},
		{"scope exceeded", []string{"f-1"}, 2, false, []string{"impl"}, true},
		{"unresolved side effect", []string{"f-1"}, 2, true, []string{"impl"}, false},
		{"budget exhausted", []string{"f-1"}, 0, false, []string{"impl"}, false},
		{"no target", []string{"f-1"}, 2, false, nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision := EvaluateRework(testCase.findings, testCase.remaining, testCase.unresolved, testCase.targets, testCase.scope)
			if decision.Eligible {
				t.Fatalf("rework should not be eligible: %+v", decision)
			}
			if decision.Reason == "" {
				t.Fatal("stop condition has no reason")
			}
		})
	}
}

func TestApplyReworkSkipsWhenIneligible(t *testing.T) {
	service := Service{}
	decision := EvaluateRework(nil, 0, false, nil, false)
	outcome, returned, err := service.ApplyRework(nil, decision, WorkflowRetryOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if returned.Eligible || outcome.View.WorkflowID != "" {
		t.Fatal("ineligible rework must not trigger RetryStep")
	}
}

func TestReworkReasonFromFindings(t *testing.T) {
	finding := proposedFindingWith("f-1", contract.SeverityHigh, "obj:1", []contract.Ref{validRef()})
	validated, err := ValidateFinding(finding, "validator-1", acceptKnownEvidence)
	if err != nil {
		t.Fatalf("validate finding: %v", err)
	}
	reason, err := reworkReasonFromFindings([]contract.Finding{validated.Finding})
	if err != nil {
		t.Fatalf("rework reason: %v", err)
	}
	if reason == "" {
		t.Fatal("empty rework reason")
	}
	if _, err := reworkReasonFromFindings(nil); err == nil {
		t.Fatal("rework reason from no findings should fail")
	}
}
