// display_test 锁定运行展示词表：阶段与完成证据可信等级都是封闭枚举，未知
// 取值回落到"未知"，同一事实只有一个展示词。词表不得引入事件 schema 依赖。
//
// Author: fxbin
package contract

import (
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

// allRunPhases 返回词表覆盖的全部运行阶段取值。
func allRunPhases() []RunPhase {
	return []RunPhase{
		RunPhaseCreated,
		RunPhaseRunning,
		RunPhaseCancelling,
		RunPhaseCompleted,
		RunPhaseFailed,
		RunPhaseCancelled,
		RunPhaseWaitingApproval,
		RunPhaseWaitingReconcile,
	}
}

// TestRunPhaseLabelCoversEveryDomainState 锁定词表覆盖 domain 的全部运行状态；
// 任何真实状态都不允许显示为"未知"。
func TestRunPhaseLabelCoversEveryDomainState(t *testing.T) {
	states := []domain.RunState{
		domain.RunCreated,
		domain.RunRunning,
		domain.RunCancelling,
		domain.RunCompleted,
		domain.RunFailed,
		domain.RunCancelled,
	}
	for _, state := range states {
		if label := RunPhaseLabel(string(state)); label == UnknownDisplayLabel {
			t.Fatalf("运行状态 %q 没有展示词", state)
		}
	}
}

// TestRunPhaseLabelCoversProjectionPhases 锁定投影对外暴露的阶段（含待审批与
// 待对账）都在词表内，界面不会把正常阶段显示成"未知"。
func TestRunPhaseLabelCoversProjectionPhases(t *testing.T) {
	approval := domain.RunProjection{State: domain.RunRunning, PendingApproval: &domain.PendingApproval{OperationID: "op-1"}}
	if phase := approval.Phase(); RunPhaseLabel(phase) == UnknownDisplayLabel {
		t.Fatalf("待审批阶段 %q 没有展示词", phase)
	}
	pending := domain.RunProjection{State: domain.RunRunning, PendingOperation: &domain.PendingOperation{ID: "op-2", SideEffectClass: "process"}}
	if phase := pending.Phase(); RunPhaseLabel(phase) == UnknownDisplayLabel {
		t.Fatalf("待对账阶段 %q 没有展示词", phase)
	}
}

// TestRunPhaseLabelsAreUnique 锁定同一状态在词表里只有一个展示词。
func TestRunPhaseLabelsAreUnique(t *testing.T) {
	owners := map[string]RunPhase{}
	for _, phase := range allRunPhases() {
		label := RunPhaseLabel(string(phase))
		if previous, exists := owners[label]; exists {
			t.Fatalf("阶段 %q 与 %q 共用展示词 %q", previous, phase, label)
		}
		owners[label] = phase
	}
}

// TestRunPhaseLabelFallsBackToUnknown 锁定词表未覆盖的取值回落到"未知"而不是
// 猜测运行语义。
func TestRunPhaseLabelFallsBackToUnknown(t *testing.T) {
	for _, phase := range []string{"", "Running", "paused", "unknown", "completed "} {
		if label := RunPhaseLabel(phase); label != UnknownDisplayLabel {
			t.Fatalf("未知阶段 %q 的展示词是 %q，应为 %q", phase, label, UnknownDisplayLabel)
		}
	}
}

// TestCompletionTrustLevelsAreClosed 锁定完成证据可信等级是三值封闭枚举：可信
// 判定成立即可信，有验证事实但未绑定全部变更即非可信，没有验证事实即未验证。
func TestCompletionTrustLevelsAreClosed(t *testing.T) {
	cases := []struct {
		name            string
		trusted         bool
		verificationSeq uint64
		want            CompletionTrust
	}{
		{name: "可信", trusted: true, verificationSeq: 3, want: CompletionTrustTrusted},
		{name: "非可信", trusted: false, verificationSeq: 3, want: CompletionTrustUntrusted},
		{name: "未验证", trusted: false, verificationSeq: 0, want: CompletionTrustUnknown},
	}
	owners := map[string]CompletionTrust{}
	for _, item := range cases {
		got := ClassifyCompletionTrust(item.trusted, item.verificationSeq)
		if got != item.want {
			t.Fatalf("%s：等级是 %q，应为 %q", item.name, got, item.want)
		}
		label := CompletionTrustLabel(got)
		if label == UnknownDisplayLabel {
			t.Fatalf("%s：可信等级没有展示词", item.name)
		}
		if previous, exists := owners[label]; exists {
			t.Fatalf("可信等级 %q 与 %q 共用展示词 %q", previous, got, label)
		}
		owners[label] = got
	}
	if label := CompletionTrustLabel(CompletionTrust("bogus")); label != UnknownDisplayLabel {
		t.Fatalf("未知可信等级的展示词是 %q，应为 %q", label, UnknownDisplayLabel)
	}
}

// TestCompletionTrustOfMatchesClassify 锁定证据投影与判定公式同源，避免两处
// 各自推导等级。
func TestCompletionTrustOfMatchesClassify(t *testing.T) {
	evidence := CompletionEvidence{Trusted: false, VerificationSeq: 5}
	if got, want := CompletionTrustOf(evidence), ClassifyCompletionTrust(evidence.Trusted, evidence.VerificationSeq); got != want {
		t.Fatalf("证据推导等级 %q 与判定公式 %q 不一致", got, want)
	}
}

// TestReconcileActionHintIsExecutable 锁定对账提示是可执行命令：必须同时给出
// 操作标识、判定、说明与证据引用，界面不得自造措辞。
func TestReconcileActionHintIsExecutable(t *testing.T) {
	for _, required := range []string{"resume", "--resolve", "--note", "--evidence"} {
		if !strings.Contains(ReconcileActionHint, required) {
			t.Fatalf("对账提示缺少 %q: %q", required, ReconcileActionHint)
		}
	}
}
