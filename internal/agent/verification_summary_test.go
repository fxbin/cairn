package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/tools"
)

// TestVerificationFailureFeedbackParsesExecResult 锁定结构化失败摘要字段。
func TestVerificationFailureFeedbackParsesExecResult(t *testing.T) {
	exit := 2
	result := tools.Result{
		OK:   false,
		Code: "failed",
		Data: json.RawMessage(`{"exit_code":2,"signal":"","stderr":"build failed\n# pkg\n","stdout":"partial\n"}`),
	}
	text := verificationFailureFeedback("verify-1", result)
	for _, want := range []string{"verify-1", "code=failed", "exit=2", "stderr_tail:", "build failed", "next:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("feedback missing %q:\n%s", want, text)
		}
	}
	facts := parseVerificationFailure(result)
	if facts.ExitCode == nil || *facts.ExitCode != exit {
		t.Fatalf("expected exit 2, got %+v", facts)
	}
}

// TestVerificationFailureFeedbackFallbackPreview 无 Data 时回退 Preview。
func TestVerificationFailureFeedbackFallbackPreview(t *testing.T) {
	result := tools.Result{OK: false, Code: "policy_denied", Preview: "denied by policy"}
	text := verificationFailureFeedback("verify-2", result)
	if !strings.Contains(text, "preview: denied by policy") || !strings.Contains(text, "code=policy_denied") {
		t.Fatalf("unexpected feedback:\n%s", text)
	}
}
