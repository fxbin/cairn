// subagent_review_test 锁定 AGT-04 Worker/Verifier 分离与集成门禁。
//
// Author: fxbin
package app

import (
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

// TestBuildSubagentReviewSeparatesRoles 锁定 Worker 与 Verifier 结论分开。
func TestBuildSubagentReviewSeparatesRoles(t *testing.T) {
	events := []domain.Event{
		{Type: domain.EventAssistantMessageAppended, Payload: []byte(`{"payload_version":1,"text":"worker output\nsecond"}`)},
		{Type: domain.EventVerificationFinished, Payload: []byte(`{"payload_version":1,"ok":true}`)},
		{Type: domain.EventRunCompleted, Payload: []byte(`{"payload_version":1,"final_response":"final text"}`)},
	}
	review := BuildSubagentReview("run-c", events)
	if review.Worker.Role != ReviewRoleWorker || review.Worker.Summary != "final text" {
		t.Fatalf("Worker 结论不符: %+v", review.Worker)
	}
	if review.Verifier.Role != ReviewRoleVerifier || !review.Verifier.OK {
		t.Fatalf("Verifier 结论不符: %+v", review.Verifier)
	}
	if len(review.Conflicts) != 0 {
		t.Fatalf("一致时不应有冲突: %+v", review.Conflicts)
	}
	ok, reason := review.CanIntegrate()
	if !ok || reason != "" {
		t.Fatalf("应可集成: %v %s", ok, reason)
	}
}

// TestDetectReviewConflictsBlocksIntegration 锁定冲突阻断集成。
func TestDetectReviewConflictsBlocksIntegration(t *testing.T) {
	events := []domain.Event{
		{Type: domain.EventRunCompleted, Payload: []byte(`{"payload_version":1,"final_response":"claimed done"}`)},
		{Type: domain.EventVerificationFinished, Payload: []byte(`{"payload_version":1,"ok":false}`)},
	}
	review := BuildSubagentReview("run-c", events)
	if len(review.Conflicts) != 1 {
		t.Fatalf("应检出冲突: %+v", review)
	}
	ok, reason := review.CanIntegrate()
	if ok || reason == "" {
		t.Fatal("冲突时禁止集成")
	}
	review.MarkIntegrated()
	if !review.Integrated {
		t.Fatal("MarkIntegrated 应标记")
	}
}

// TestCanIntegrateRequiresBothSides 锁定缺任一侧结论均不可集成。
func TestCanIntegrateRequiresBothSides(t *testing.T) {
	empty := BuildSubagentReview("run-c", nil)
	if ok, _ := empty.CanIntegrate(); ok {
		t.Fatal("空结论不应可集成")
	}
	workerOnly := BuildSubagentReview("run-c", []domain.Event{
		{Type: domain.EventRunCompleted, Payload: []byte(`{"payload_version":1,"final_response":"x"}`)},
	})
	if ok, _ := workerOnly.CanIntegrate(); ok {
		t.Fatal("无 Verifier 通过时不应可集成")
	}
}
