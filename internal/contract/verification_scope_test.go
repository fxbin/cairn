// verification_scope_test.go 回归验证范围的冻结、摘要与必需性推导。
//
// Author: fxbin
package contract

import "testing"

func testScopeAcceptance(t *testing.T) AcceptanceContract {
	t.Helper()
	accepted, err := NewAcceptanceContract([]AcceptanceCriterion{
		{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}},
		{ID: "docs", Statement: "文档完整", Required: false, Checks: []string{"docs"}},
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	return accepted
}

func testScopeObjects() []VerificationObject {
	return []VerificationObject{
		{ID: "obj-1", Kind: "source", Version: "sha256:" + repeatHash('a'), Criteria: []string{"compile"}, Ruleset: "coding", RulesetVersion: "v1"},
		{ID: "obj-2", Kind: "document", Version: "sha256:" + repeatHash('b'), Criteria: []string{"docs"}, Ruleset: "coding", RulesetVersion: "v1"},
	}
}

func repeatHash(character byte) string {
	result := make([]byte, 64)
	for index := range result {
		result[index] = character
	}
	return string(result)
}

func TestNewVerificationScopeFreezesObjectsAndRequiredness(t *testing.T) {
	accepted := testScopeAcceptance(t)
	scope, err := NewVerificationScope(accepted, testScopeObjects())
	if err != nil {
		t.Fatalf("new scope: %v", err)
	}
	if err := scope.Validate(); err != nil {
		t.Fatalf("validate scope: %v", err)
	}
	if !scope.Objects[0].Required {
		t.Fatal("object serving a required criterion must be required")
	}
	if scope.Objects[1].Required {
		t.Fatal("object serving only an optional criterion must not be required")
	}
	// 必须复用同一个入参切片；生成新切片会让深拷贝测试假阳性。
	objects := testScopeObjects()
	scopeWithInput, err := NewVerificationScope(accepted, objects)
	if err != nil {
		t.Fatalf("new scope with shared input: %v", err)
	}
	objects[0].Version = "tampered"
	objects[0].Criteria[0] = "docs"
	if scopeWithInput.Objects[0].Version == "tampered" || scopeWithInput.Objects[0].Criteria[0] != "compile" {
		t.Fatal("frozen scope was mutated through caller input")
	}
}

func TestNewVerificationScopeRejectsInvalidObjects(t *testing.T) {
	accepted := testScopeAcceptance(t)
	cases := []struct {
		name    string
		objects []VerificationObject
	}{
		{"empty", nil},
		{"unknown criterion", []VerificationObject{{ID: "obj", Kind: "source", Version: "v1", Criteria: []string{"missing"}, Ruleset: "coding", RulesetVersion: "v1"}}},
		{"no criteria", []VerificationObject{{ID: "obj", Kind: "source", Version: "v1", Ruleset: "coding", RulesetVersion: "v1"}}},
		{"duplicate id", []VerificationObject{{ID: "obj", Kind: "source", Version: "v1", Criteria: []string{"compile"}, Ruleset: "coding", RulesetVersion: "v1"}, {ID: "obj", Kind: "source", Version: "v2", Criteria: []string{"compile"}, Ruleset: "coding", RulesetVersion: "v1"}}},
		{"empty version", []VerificationObject{{ID: "obj", Kind: "source", Version: "", Criteria: []string{"compile"}, Ruleset: "coding", RulesetVersion: "v1"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewVerificationScope(accepted, testCase.objects); err == nil {
				t.Fatal("invalid verification object was accepted")
			}
		})
	}
}

func TestNewVerificationScopeRejectsUncoveredRequiredCriterion(t *testing.T) {
	accepted := testScopeAcceptance(t)
	// 只登记可选标准的对象，遗漏必需标准 compile，必须失败关闭。
	objects := []VerificationObject{{ID: "obj", Kind: "document", Version: "sha256:" + repeatHash('b'), Criteria: []string{"docs"}, Ruleset: "coding", RulesetVersion: "v1"}}
	if _, err := NewVerificationScope(accepted, objects); err == nil {
		t.Fatal("scope missing a required criterion's object was accepted")
	}
}

func TestVerificationScopeRejectsTamperedDigest(t *testing.T) {
	accepted := testScopeAcceptance(t)
	scope, err := NewVerificationScope(accepted, testScopeObjects())
	if err != nil {
		t.Fatalf("new scope: %v", err)
	}
	scope.Digest = "sha256:deadbeef"
	if err := scope.Validate(); err == nil {
		t.Fatal("tampered scope digest was accepted")
	}
}
