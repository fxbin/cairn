package agent

import (
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

// TestIncompleteModelAttemptPicksNumericLatest 验证悬空模型尝试按时间戳
// 数值（而非字典序）选择最新：字典序与数值序相反的纳秒时间戳对必须选中数值更大者。
func TestIncompleteModelAttemptPicksNumericLatest(t *testing.T) {
	turnA, turnB := domain.TurnID("turn_a"), domain.TurnID("turn_b")
	// attempt_100_* 字典序小于 attempt_99_*，但时间戳数值更大，必须胜出。
	older := domain.AttemptID("attempt_99_zzzzzzzzzzzzzzzz")
	newer := domain.AttemptID("attempt_100_aaaaaaaaaaaaaaaa")
	events := []domain.Event{
		{Type: domain.EventModelRequestStarted, AttemptID: &older, TurnID: &turnA},
		{Type: domain.EventModelRequestStarted, AttemptID: &newer, TurnID: &turnB},
	}
	attempt, turn, found := incompleteModelAttempt(events)
	if !found || attempt != newer || turn != turnB {
		t.Fatalf("expected numeric-latest %s/%s, got %s/%s (%v)", newer, turnB, attempt, turn, found)
	}
}

// TestIncompleteModelAttemptSameTimestampFallsBackToLexicographic 验证同一
// 时间戳下用字典序稳定决胜，且已有终态的尝试不再参与选择。
func TestIncompleteModelAttemptSameTimestampFallsBackToLexicographic(t *testing.T) {
	turnA, turnB := domain.TurnID("turn_a"), domain.TurnID("turn_b")
	low := domain.AttemptID("attempt_100_aaaa")
	high := domain.AttemptID("attempt_100_bbbb")
	events := []domain.Event{
		{Type: domain.EventModelRequestStarted, AttemptID: &low, TurnID: &turnA},
		{Type: domain.EventModelRequestFailed, AttemptID: &low, TurnID: &turnA},
		{Type: domain.EventModelRequestStarted, AttemptID: &high, TurnID: &turnB},
	}
	attempt, turn, found := incompleteModelAttempt(events)
	if !found || attempt != high || turn != turnB {
		t.Fatalf("expected pending %s/%s, got %s/%s (%v)", high, turnB, attempt, turn, found)
	}
}

// TestAttemptTimestampRejectsMalformedIDs 验证非三段或非数值时间戳的 ID
// 回退字典序比较路径。
func TestAttemptTimestampRejectsMalformedIDs(t *testing.T) {
	for _, id := range []domain.AttemptID{"", "attempt_only", "attempt_abc_def", "attempt_-1_def", "attempt_1_2_3"} {
		if _, ok := attemptTimestamp(id); ok {
			t.Fatalf("attemptTimestamp(%q) unexpectedly succeeded", id)
		}
	}
	if value, ok := attemptTimestamp(domain.AttemptID("attempt_1723620000000000000_entropy")); !ok || value != 1723620000000000000 {
		t.Fatalf("attemptTimestamp failed to parse valid id: %d %v", value, ok)
	}
}
