// subagent_review.go 实现 AGT-04 结果审查与集成：Worker 输出与 Verifier
// 结论分开呈现；差异、证据与冲突可检查；集成动作仍走既有授权链。
// Author: fxbin
package app

import (
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/domain"
)

// ReviewRole 区分产出来源，避免 Worker 与 Verifier 结论混同。
type ReviewRole string

const (
	// ReviewRoleWorker 是子运行的执行产出。
	ReviewRoleWorker ReviewRole = "worker"
	// ReviewRoleVerifier 是独立验证结论。
	ReviewRoleVerifier ReviewRole = "verifier"
)

// ReviewFinding 是一条可检查的审查事实。
type ReviewFinding struct {
	Role    ReviewRole `json:"role"`
	Summary string     `json:"summary"`
	OK      bool       `json:"ok"`
	Source  string     `json:"source,omitempty"`
}

// ReviewConflict 描述 Worker 与 Verifier 之间的不一致。
type ReviewConflict struct {
	WorkerSummary   string `json:"worker_summary"`
	VerifierSummary string `json:"verifier_summary"`
	Reason          string `json:"reason"`
}

// SubagentReview 是一次完整的集成前审查。
type SubagentReview struct {
	ChildRunID string           `json:"child_run_id"`
	Worker     ReviewFinding    `json:"worker"`
	Verifier   ReviewFinding    `json:"verifier"`
	Conflicts  []ReviewConflict `json:"conflicts,omitempty"`
	// Integrated 为 false 表示仅审查、尚未走授权集成。
	Integrated bool `json:"integrated"`
}

// BuildSubagentReview 从子运行事件分离 Worker 产出与 Verifier 结论。
// Worker 侧取 run 终态文本；Verifier 侧取 verification_finished。
func BuildSubagentReview(childRunID domain.RunID, childEvents []domain.Event) SubagentReview {
	review := SubagentReview{ChildRunID: string(childRunID)}
	workerText := ""
	verifierOK := false
	verifierSeen := false
	for _, event := range childEvents {
		switch event.Type {
		case domain.EventAssistantMessageAppended:
			var payload domain.AssistantMessagePayload
			if err := jsonUnmarshal(event.Payload, &payload); err == nil && payload.Text != "" {
				workerText = payload.Text
			}
		case domain.EventRunCompleted:
			var payload struct {
				FinalResponse string `json:"final_response"`
			}
			if err := jsonUnmarshal(event.Payload, &payload); err == nil && payload.FinalResponse != "" {
				workerText = payload.FinalResponse
			}
		case domain.EventVerificationFinished:
			var payload domain.VerificationFinishedPayload
			if err := jsonUnmarshal(event.Payload, &payload); err == nil {
				verifierOK = payload.OK
				verifierSeen = true
			}
		}
	}
	review.Worker = ReviewFinding{
		Role:    ReviewRoleWorker,
		Summary: firstLineBounded(workerText, 200),
		OK:      workerText != "",
		Source:  "run_output",
	}
	if verifierSeen {
		review.Verifier = ReviewFinding{
			Role:    ReviewRoleVerifier,
			Summary: fmt.Sprintf("verification ok=%v", verifierOK),
			OK:      verifierOK,
			Source:  "verification_finished",
		}
	} else {
		review.Verifier = ReviewFinding{
			Role:    ReviewRoleVerifier,
			Summary: "no verification recorded",
			OK:      false,
			Source:  "none",
		}
	}
	review.Conflicts = DetectReviewConflicts(review.Worker, review.Verifier)
	return review
}

// DetectReviewConflicts 找出 Worker 成功但 Verifier 未通过等不一致。
func DetectReviewConflicts(worker, verifier ReviewFinding) []ReviewConflict {
	var conflicts []ReviewConflict
	if worker.OK && !verifier.OK {
		conflicts = append(conflicts, ReviewConflict{
			WorkerSummary:   worker.Summary,
			VerifierSummary: verifier.Summary,
			Reason:          "worker produced output but verifier did not pass",
		})
	}
	if !worker.OK && verifier.OK {
		conflicts = append(conflicts, ReviewConflict{
			WorkerSummary:   worker.Summary,
			VerifierSummary: verifier.Summary,
			Reason:          "verifier passed without worker output",
		})
	}
	return conflicts
}

// CanIntegrate 判断是否允许进入集成阶段：存在冲突时禁止，且必须双方都有结论。
func (review SubagentReview) CanIntegrate() (bool, string) {
	if len(review.Conflicts) > 0 {
		return false, "resolve conflicts before integration"
	}
	if !review.Worker.OK {
		return false, "worker output is empty"
	}
	if !review.Verifier.OK {
		return false, "verifier has not passed"
	}
	return true, ""
}

// MarkIntegrated 在授权链通过后标记已集成；不自动调用。
func (review *SubagentReview) MarkIntegrated() {
	review.Integrated = true
}

// firstLineBounded 截取首行并限长。
func firstLineBounded(text string, limit int) string {
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	runes := []rune(line)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return line
}
