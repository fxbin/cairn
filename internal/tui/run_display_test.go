// run_display_test 锁定 TUI 复用统一展示词表：终态判定行给出阶段词与完成
// 证据可信等级，等待对账时给出与 CLI 一致的显式动作指引。
//
// Author: fxbin
package tui

import (
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// TestVerdictLineUsesContractVocabulary 锁定终态判定行使用统一阶段词与完成
// 证据可信等级，不自造第二套说法。
func TestVerdictLineUsesContractVocabulary(t *testing.T) {
	console := RunConsole{result: &app.RunResult{Result: &contract.Result{
		State:   string(domain.RunCompleted),
		Outcome: string(contract.RunPhaseCompleted),
		Evidence: contract.CompletionEvidence{
			Trusted:         true,
			VerificationSeq: 3,
		},
	}}}
	line := console.verdictLine()
	for _, required := range []string{
		contract.RunPhaseLabel(string(contract.RunPhaseCompleted)),
		contract.CompletionTrustLabel(contract.CompletionTrustTrusted),
	} {
		if !strings.Contains(line, required) {
			t.Fatalf("终态判定行缺少 %q: %q", required, line)
		}
	}
}

// TestVerdictLineShowsUntrustedCompletion 锁定未绑定全部变更的完成证据显示
// "非可信"，没有验证事实时显示"未验证"。
func TestVerdictLineShowsUntrustedCompletion(t *testing.T) {
	cases := []struct {
		name     string
		evidence contract.CompletionEvidence
		want     string
	}{
		{
			name:     "非可信",
			evidence: contract.CompletionEvidence{Trusted: false, VerificationSeq: 2},
			want:     contract.CompletionTrustLabel(contract.CompletionTrustUntrusted),
		},
		{
			name:     "未验证",
			evidence: contract.CompletionEvidence{Trusted: false},
			want:     contract.CompletionTrustLabel(contract.CompletionTrustUnknown),
		},
	}
	for _, item := range cases {
		console := RunConsole{result: &app.RunResult{Result: &contract.Result{
			State:    string(domain.RunCompleted),
			Outcome:  string(contract.RunPhaseCompleted),
			Evidence: item.evidence,
		}}}
		line := console.verdictLine()
		if !strings.Contains(line, item.want) {
			t.Fatalf("%s：判定行缺少 %q: %q", item.name, item.want, line)
		}
	}
}

// TestReconcilePauseShowsUnifiedActionHint 锁定等待对账时给出显式提示：阶段词
// 与动作指引都取自统一词表，与 CLI 的恢复指引一致。
func TestReconcilePauseShowsUnifiedActionHint(t *testing.T) {
	console := RunConsole{}
	model, _ := console.receiveResult(runFinishedMsg{
		err: &ports.WaitingReconcileError{RunID: "run-1", OperationID: "op-1", Reason: "unknown"},
	})
	updated, ok := model.(RunConsole)
	if !ok {
		t.Fatalf("等待对账返回了非控制台模型: %T", model)
	}
	text := strings.Join(updated.lines, "\n")
	for _, required := range []string{
		contract.RunPhaseLabel(string(contract.RunPhaseWaitingReconcile)),
		contract.ReconcileActionHint,
		"op-1",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("等待对账提示缺少 %q: %q", required, text)
		}
	}
	if strings.Contains(text, "运行失败") {
		t.Fatalf("等待对账被当成失败展示: %q", text)
	}
}

// TestReconcilePauseDoesNotAdvanceSessionChain 锁定等待对账的运行尚未收束，
// 不得推进会话链尾。
func TestReconcilePauseDoesNotAdvanceSessionChain(t *testing.T) {
	console := RunConsole{sessionID: "session-1"}
	model, _ := console.receiveResult(runFinishedMsg{
		result: &app.RunResult{ID: "run-1", SessionID: "session-1"},
		err:    &ports.WaitingReconcileError{RunID: "run-1", OperationID: "op-1", Reason: "unknown"},
	})
	updated, ok := model.(RunConsole)
	if !ok {
		t.Fatalf("等待对账返回了非控制台模型: %T", model)
	}
	if updated.sessionPrev != "" {
		t.Fatalf("等待对账推进了会话链尾: %q", updated.sessionPrev)
	}
	if len(updated.sessionRuns) != 0 {
		t.Fatalf("等待对账写入了会话链记录: %+v", updated.sessionRuns)
	}
}
