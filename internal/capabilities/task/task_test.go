// task_test.go 回归任务冻结结构化验收合同与旧任务兼容。
//
// Author: fxbin
package task

import (
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/contract"
)

func TestNewFreezesAcceptanceContract(t *testing.T) {
	criteria := []contract.AcceptanceCriterion{
		{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}},
		{ID: "tests", Statement: "测试通过", Required: true, Checks: []string{"test"}},
	}
	current, err := New("sha256:ws", Definition{Goal: "修复构建", Criteria: criteria}, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("new task: %v", err)
	}
	if current.Definition.AcceptanceDigest == "" {
		t.Fatal("acceptance digest was not frozen")
	}
	if len(current.Definition.Acceptance) != 2 {
		t.Fatalf("acceptance statements were not derived: %v", current.Definition.Acceptance)
	}
	accepted, found, err := current.Definition.Contract()
	if err != nil || !found {
		t.Fatalf("contract not found: found=%t err=%v", found, err)
	}
	if accepted.Digest != current.Definition.AcceptanceDigest {
		t.Fatal("contract digest does not match frozen definition")
	}
	// 修改输入不应改变已冻结合同。
	criteria[0].Checks[0] = "tampered"
	again, found, err := current.Definition.Contract()
	if err != nil || !found || again.Criteria[0].Checks[0] != "build" {
		t.Fatalf("frozen contract was mutated: %+v err=%v", again, err)
	}
}

func TestNewAcceptsLegacyTextAcceptance(t *testing.T) {
	current, err := New("sha256:ws", Definition{Goal: "旧任务", Acceptance: []string{"构建通过"}}, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("new legacy task: %v", err)
	}
	_, found, err := current.Definition.Contract()
	if err != nil || found {
		t.Fatalf("legacy task must not report a contract: found=%t err=%v", found, err)
	}
}

func TestEvidenceEqualComparesReferencedValue(t *testing.T) {
	first := Evidence{RunID: "run-1", Sequence: 3, EventHash: "hash", AcceptanceDigest: "sha256:abc", EvidenceRef: &contract.Ref{SHA256: "sha256:ref"}}
	second := Evidence{RunID: "run-1", Sequence: 3, EventHash: "hash", AcceptanceDigest: "sha256:abc", EvidenceRef: &contract.Ref{SHA256: "sha256:ref"}}
	if !first.Equal(second) {
		t.Fatal("equal evidence with distinct pointers was not equal")
	}
	second.EvidenceRef.SHA256 = "sha256:other"
	if first.Equal(second) {
		t.Fatal("different referenced values were considered equal")
	}
}

func TestNewRejectsMismatchedAcceptanceStatements(t *testing.T) {
	criteria := []contract.AcceptanceCriterion{{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}}}
	_, err := New("sha256:ws", Definition{Goal: "修复构建", Acceptance: []string{"另一份描述"}, Criteria: criteria}, time.Unix(1, 0))
	if err == nil {
		t.Fatal("mismatched acceptance statements were accepted")
	}
}
