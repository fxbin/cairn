// command_task_test.go 回归结构化验收参数的并列配对。
//
// Author: fxbin
package cli

import "testing"

func TestBuildAcceptanceCriteria(t *testing.T) {
	criteria, err := buildAcceptanceCriteria(
		[]string{"项目可编译", "测试通过"},
		[]string{"build", "test,lint"},
		[]string{"compile", "suite"},
	)
	if err != nil {
		t.Fatalf("build criteria: %v", err)
	}
	if len(criteria) != 2 {
		t.Fatalf("unexpected criteria count: %d", len(criteria))
	}
	if criteria[0].ID != "compile" || len(criteria[0].Checks) != 1 || criteria[0].Checks[0] != "build" {
		t.Fatalf("unexpected first criterion: %+v", criteria[0])
	}
	if criteria[1].ID != "suite" || len(criteria[1].Checks) != 2 || !criteria[1].Required {
		t.Fatalf("unexpected second criterion: %+v", criteria[1])
	}
}

func TestBuildAcceptanceCriteriaLegacy(t *testing.T) {
	criteria, err := buildAcceptanceCriteria([]string{"构建通过"}, nil, nil)
	if err != nil || len(criteria) != 0 {
		t.Fatalf("legacy acceptance must produce no contract: %v %+v", err, criteria)
	}
}

func TestBuildAcceptanceCriteriaRejectsMismatch(t *testing.T) {
	if _, err := buildAcceptanceCriteria([]string{"a", "b"}, []string{"build"}, nil); err == nil {
		t.Fatal("check count mismatch was accepted")
	}
	if _, err := buildAcceptanceCriteria([]string{"a"}, []string{"build"}, []string{"x", "y"}); err == nil {
		t.Fatal("criterion count mismatch was accepted")
	}
	if _, err := buildAcceptanceCriteria([]string{"a"}, nil, []string{"x"}); err == nil {
		t.Fatal("criterion without check was accepted")
	}
}
