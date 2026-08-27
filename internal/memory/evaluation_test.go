package memory

import (
	"strings"
	"testing"
)

// evalDigest 是 EvaluationID 测试共用的形态合法摘要。
const evalDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"

// TestEvaluationIDDeterminismAndSensitivity 锁定评测身份派生：相同五元组恒
// 得同一标识，任一输入变化都产生不同身份；空输入拒绝派生。
func TestEvaluationIDDeterminismAndSensitivity(t *testing.T) {
	first, err := EvaluationID("evo_abc", evalDigest, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "cairn-comparator-v1")
	if err != nil {
		t.Fatalf("derive evaluation id: %v", err)
	}
	second, err := EvaluationID("evo_abc", evalDigest, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "cairn-comparator-v1")
	if err != nil {
		t.Fatalf("derive again: %v", err)
	}
	if first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("identity must be a deterministic sha256 digest: %q vs %q", first, second)
	}
	variantInput, err := EvaluationID("evo_abc", evalDigest, "sha256:3333333333333333333333333333333333333333333333333333333333333333", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "cairn-comparator-v1")
	if err != nil {
		t.Fatalf("derive variant input: %v", err)
	}
	if variantInput == first {
		t.Fatal("config fingerprint drift must change the evaluation identity")
	}
	if _, err := EvaluationID("", evalDigest, evalDigest, evalDigest, "v"); err == nil {
		t.Fatal("empty candidate id must be rejected")
	}
	if _, err := EvaluationID("evo_abc", evalDigest, evalDigest, evalDigest, ""); err == nil {
		t.Fatal("empty comparator version must be rejected")
	}
}

// TestEvaluationStatusLexiconIsClosed 锁定评测状态词表：五个闭合态全部合法，
// 结论态之外的一切字符串失败关闭。
func TestEvaluationStatusLexiconIsClosed(t *testing.T) {
	for _, status := range []string{EvaluationStatusRunning, EvaluationStatusPassed, EvaluationStatusFailed, EvaluationStatusInconclusive, EvaluationStatusInvalid} {
		if !ValidEvaluationStatus(status) {
			t.Fatalf("declared status %q must be valid", status)
		}
	}
	if ValidEvaluationStatus("evaluating") || ValidEvaluationStatus("") {
		t.Fatal("undeclared evaluation statuses must fail closed")
	}
}
