package tui

import (
	"context"
	"strings"
	"testing"
)

func TestReviewDoctorResultReachesTranscript(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	defer console.rootCancel()
	model, _ := console.Update(doctorDoneMsg{lines: []string{"REVIEW_DOCTOR_RESULT"}})
	updated := model.(RunConsole)
	if !strings.Contains(strings.Join(updated.lines, "\n"), "REVIEW_DOCTOR_RESULT") {
		t.Fatal("doctor result was dropped by unreachable message handler")
	}
}
