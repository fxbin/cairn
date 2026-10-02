package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/benchmark"
	"github.com/fxbin/cairn/internal/memory"
)

// TestLoadSuiteResultForPromotionRejectsLegacySuccess 锁定摘要正确的旧套件
// 仍不能把 v2 Success 送进当前晋升指标。
func TestLoadSuiteResultForPromotionRejectsLegacySuccess(t *testing.T) {
	stateRoot := t.TempDir()
	evaluation := memory.Evaluation{ID: "sha256:evaluation", CandidateID: "candidate-1", Status: memory.EvaluationStatusPassed}
	resultPath := filepath.Join(stateRoot, "evaluations", strings.ReplaceAll(evaluation.ID, ":", "_"), "suite-result.json")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		suite      int
		record     int
		wantAccept bool
	}{
		{name: "legacy suite and record", suite: 2, record: 2},
		{name: "new suite with legacy record", suite: 3, record: 2},
		{name: "trusted evidence version", suite: 3, record: 3, wantAccept: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := benchmark.SandboxSuiteResult{
				SchemaVersion: test.suite, EvaluationID: evaluation.ID,
				CandidateID: evaluation.CandidateID, Status: memory.EvaluationStatusPassed,
				Records: []benchmark.SandboxRunRecord{{SchemaVersion: test.record, Success: true}},
			}
			content, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(resultPath, content, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			evaluation.SuiteResultDigest = "sha256:" + hex.EncodeToString(digest[:])
			_, err = loadSuiteResultForPromotion(evaluation, stateRoot)
			if (err == nil) != test.wantAccept {
				t.Fatalf("accepted=%v, want %v, err=%v", err == nil, test.wantAccept, err)
			}
		})
	}
}
