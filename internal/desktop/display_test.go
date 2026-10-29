// display_test 锁定桌面桥接下发的展示字段与统一词表一致：阶段、状态与完成
// 证据可信等级都由 Go 侧词表产出，前端只渲染。
//
// Author: fxbin
package desktop

import (
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// TestRunDetailCarriesContractLabels 锁定运行详情携带统一词表的阶段与状态
// 展示词。
func TestRunDetailCarriesContractLabels(t *testing.T) {
	inspected := app.InspectResult{
		Projection: domain.RunProjection{
			ID:    "run-1",
			State: domain.RunCompleted,
		},
	}
	detail := runDetailFrom("run-1", inspected)
	if detail.Phase != string(domain.RunCompleted) {
		t.Fatalf("运行详情的阶段是 %q", detail.Phase)
	}
	if detail.PhaseLabel != contract.RunPhaseLabel(detail.Phase) {
		t.Fatalf("运行详情的阶段展示词是 %q，应为 %q", detail.PhaseLabel, contract.RunPhaseLabel(detail.Phase))
	}
	if detail.StateLabel != contract.RunPhaseLabel(string(detail.State)) {
		t.Fatalf("运行详情的状态展示词是 %q，应为 %q", detail.StateLabel, contract.RunPhaseLabel(string(detail.State)))
	}
}

// TestRunDetailCarriesWaitingReconcileLabel 锁定待对账运行在桥接层就带有统一
// 词表的"待对账"展示词。
func TestRunDetailCarriesWaitingReconcileLabel(t *testing.T) {
	inspected := app.InspectResult{
		Projection: domain.RunProjection{
			ID:    "run-2",
			State: domain.RunRunning,
			PendingOperation: &domain.PendingOperation{
				ID:              "op-1",
				SideEffectClass: "process",
			},
		},
	}
	detail := runDetailFrom("run-2", inspected)
	if detail.Phase != string(contract.RunPhaseWaitingReconcile) {
		t.Fatalf("运行详情的阶段是 %q，应为 %q", detail.Phase, contract.RunPhaseWaitingReconcile)
	}
	if detail.PhaseLabel != contract.RunPhaseLabel(string(contract.RunPhaseWaitingReconcile)) {
		t.Fatalf("运行详情的阶段展示词是 %q", detail.PhaseLabel)
	}
}

// TestEvidenceViewCarriesContractTrustLabel 锁定完成证据视图携带统一词表的
// 状态、结果与可信等级展示词。
func TestEvidenceViewCarriesContractTrustLabel(t *testing.T) {
	result := contract.Result{
		State:   string(domain.RunCompleted),
		Outcome: string(contract.RunPhaseCompleted),
		Evidence: contract.CompletionEvidence{
			Trusted:         true,
			VerificationSeq: 2,
		},
	}
	view := evidenceViewFrom(result)
	if view.StateLabel != contract.RunPhaseLabel(result.State) {
		t.Fatalf("证据视图的状态展示词是 %q", view.StateLabel)
	}
	if view.OutcomeLabel != contract.RunPhaseLabel(result.Outcome) {
		t.Fatalf("证据视图的结果展示词是 %q", view.OutcomeLabel)
	}
	if view.TrustLabel != contract.CompletionTrustLabel(contract.CompletionTrustTrusted) {
		t.Fatalf("证据视图的可信等级展示词是 %q", view.TrustLabel)
	}
}

// TestStatusViewCarriesContractLabels 锁定运行状态视图携带统一词表的阶段与
// 状态展示词。
func TestStatusViewCarriesContractLabels(t *testing.T) {
	controller := &RunController{}
	outcome := app.RunResult{Result: &contract.Result{
		State:   string(domain.RunRunning),
		Outcome: string(contract.RunPhaseWaitingApproval),
	}}
	view := controller.statusFromOutcome("run-3", outcome, nil)
	if view.Phase != string(contract.RunPhaseWaitingApproval) {
		t.Fatalf("状态视图的阶段是 %q", view.Phase)
	}
	if view.PhaseLabel != contract.RunPhaseLabel(string(contract.RunPhaseWaitingApproval)) {
		t.Fatalf("状态视图的阶段展示词是 %q", view.PhaseLabel)
	}
	if view.StateLabel != contract.RunPhaseLabel(view.State) {
		t.Fatalf("状态视图的状态展示词是 %q", view.StateLabel)
	}
}
