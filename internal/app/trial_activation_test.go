package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// TestResolveTrialActivationWithoutStoreFailsClosed 锁定显式请求但记忆库缺
// 失时的失败关闭：不得沉默回基线。
func TestResolveTrialActivationWithoutStoreFailsClosed(t *testing.T) {
	service := Service{}
	if _, _, err := service.ResolveTrialActivation(context.Background(), t.TempDir(), t.TempDir(), config.Defaults()); err == nil {
		t.Fatal("explicit trial activation without a memory store must fail closed")
	}
}

// TestResolveTrialActivationReturnsBaselineWhenInactive 锁定无激活态：账本存
// 在但没有沙箱激活时原配置返回且激活标记为假。
func TestResolveTrialActivationReturnsBaselineWhenInactive(t *testing.T) {
	service, workspace, stateDir, configuration, _ := evolutionAppFixture(t)
	resolved, activated, err := service.ResolveTrialActivation(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatalf("store without ledger must resolve to baseline: %v", err)
	}
	if activated || resolved.Fingerprint() != configuration.Fingerprint() {
		t.Fatal("without an activation the configuration must pass through unchanged")
	}
}

// TestApplyCompactionVariantOverridesAllFourFields 锁定变体应用单一源：四项
// 压缩参数全部落到配置副本、指纹改变且整体校验通过。
func TestApplyCompactionVariantOverridesAllFourFields(t *testing.T) {
	base := withTestProvider(config.Defaults())
	variant := memory.CompactionParamsVariant{
		SchemaVersion: memory.EvolutionSchemaVersion, MaxPromptTokens: 32_000, MaxSummaryBytes: 8_192,
		InventoryBudgetPercent: 40, ThresholdReservePercent: 25,
	}
	applied, err := ApplyCompactionVariant(base, variant)
	if err != nil {
		t.Fatalf("apply variant: %v", err)
	}
	if applied.Context.MaxPromptTokens != 32_000 || applied.Context.MaxSummaryBytes != 8_192 ||
		applied.Context.InventoryBudgetPercent != 40 || applied.Context.ThresholdReservePercent != 25 {
		t.Fatalf("variant fields not applied: %+v", applied.Context)
	}
	if applied.Fingerprint() == base.Fingerprint() {
		t.Fatal("applied variant must change the configuration fingerprint")
	}
}

// TestResolveTrialActivationAppliesActivatedVersion 锁定激活正路径：完整演化
// 链（评测通过→人工决策→沙箱激活）后，解析把激活变体的四项参数应用到配
// 置副本并改变指纹。
func TestResolveTrialActivationAppliesActivatedVersion(t *testing.T) {
	service, workspace, stateDir, configuration, source := evolutionAppFixture(t)
	variant := memory.CompactionParamsVariant{
		SchemaVersion: memory.EvolutionSchemaVersion, MaxPromptTokens: 32_000, MaxSummaryBytes: 8_192,
		InventoryBudgetPercent: 30, ThresholdReservePercent: 22,
	}
	baselineFingerprint := configuration.Fingerprint()
	candidate, err := service.CreateEvolutionCandidate(context.Background(), MemoryEvolutionCreateOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		SourceDerivedID: source.DerivedID, SourceConsolidationID: source.ConsolidationID,
		BaselineFingerprint: baselineFingerprint, Variant: variant,
		EvalSetFingerprint: "sha256:" + strings.Repeat("ab", 32),
	})
	if err != nil {
		t.Fatalf("create ready candidate: %v", err)
	}
	_ = candidate
	evalSetFingerprint := "sha256:" + strings.Repeat("ab", 32)
	suiteRoot := t.TempDir()
	variantFingerprint, err := memory.VariantFingerprint(memory.TargetKindContextCompactionParams, variant)
	if err != nil {
		t.Fatal(err)
	}
	variantSpec, err := memory.VariantSpecDigest(variant)
	if err != nil {
		t.Fatal(err)
	}
	evaluationID, err := memory.EvaluationID(candidate.ID, evalSetFingerprint, baselineFingerprint, variantFingerprint, "trial-comparator")
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.StartEvolutionEvaluation(context.Background(), MemoryEvaluationStartOptions{
		Evaluation: memory.Evaluation{
			SchemaVersion: memory.EvaluationSchemaVersion, ID: evaluationID, CandidateID: candidate.ID,
			EvalSetFingerprint: evalSetFingerprint, BaselineConfigFingerprint: baselineFingerprint,
			VariantConfigFingerprint: variantFingerprint, ComparatorVersion: "trial-comparator",
			Status: memory.EvaluationStatusRunning,
		},
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		Binding: memory.EvaluationBinding{
			BaselineFingerprint: baselineFingerprint, VariantSpecSHA256: variantSpec,
			CandidateVariantFingerprint: variantFingerprint,
		},
	})
	if err != nil {
		t.Fatalf("start evaluation: %v", err)
	}
	result := struct {
		SchemaVersion int    `json:"schema_version"`
		EvaluationID  string `json:"evaluation_id"`
		CandidateID   string `json:"candidate_id"`
		Status        string `json:"status"`
		Records       []struct {
			SchemaVersion int `json:"schema_version"`
		} `json:"records"`
	}{
		SchemaVersion: sandboxeval.TrustedResultSchemaVersion, EvaluationID: started.ID,
		CandidateID: candidate.ID, Status: memory.EvaluationStatusPassed,
	}
	result.Records = append(result.Records, struct {
		SchemaVersion int `json:"schema_version"`
	}{SchemaVersion: sandboxeval.TrustedResultSchemaVersion})
	content, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(suiteRoot, "evaluations", strings.ReplaceAll(started.ID, ":", "_"), "suite-result.json")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	suiteDigest := "sha256:" + hex.EncodeToString(digest[:])
	if _, err := service.FinishEvolutionEvaluation(context.Background(), started.ID, workspace, stateDir, configuration, memory.EvaluationStatusPassed, suiteDigest); err != nil {
		t.Fatalf("finish evaluation: %v", err)
	}
	promotionID, err := memory.PromotionID(memory.TargetKindContextCompactionParams, 1)
	if err != nil {
		t.Fatal(err)
	}
	promotion := memory.Promotion{
		ID: promotionID, SchemaVersion: memory.PromotionSchemaVersion,
		CandidateID: candidate.ID, EvaluationID: started.ID, TargetKind: memory.TargetKindContextCompactionParams,
		Version: 1, Variant: variant, VariantFingerprint: variantFingerprint, VariantSpecSHA256: variantSpec,
		BaselineFingerprint: baselineFingerprint, EvalSetFingerprint: evalSetFingerprint,
		SuiteResultDigest: suiteDigest, ComparatorVersion: "trial-comparator",
		Conclusion: memory.EvaluationStatusPassed, ConclusionTrigger: "mode_b_efficiency",
		ApprovalNonce: "sha256:" + strings.Repeat("ef", 32), Approver: "fxbin",
		Status: memory.PromotionStatusPromotedInactive,
	}
	approval, err := memory.PromotionApprovalFingerprint(promotion)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := service.PromoteEvolutionCandidate(context.Background(), MemoryPromotionOptions{
		Promotion: promotion, ApprovalFingerprint: approval,
		Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err == nil {
		t.Fatal("direct service promotion without suite evidence must fail closed")
	}
	promoted, err = service.PromoteEvolutionCandidate(context.Background(), MemoryPromotionOptions{
		Promotion: promotion, ApprovalFingerprint: approval, SuiteStateRoot: suiteRoot,
		Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, _, err := service.ActivateEvolutionPromotion(context.Background(), MemoryPromotionActivationOptions{
		TargetKind: memory.TargetKindContextCompactionParams, PromotionID: promoted.ID,
		Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	resolved, activated, err := service.ResolveTrialActivation(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatalf("resolve activated version: %v", err)
	}
	if !activated {
		t.Fatal("an active sandbox promotion must resolve as activated")
	}
	if resolved.Context.MaxPromptTokens != 32_000 || resolved.Context.MaxSummaryBytes != 8_192 ||
		resolved.Context.InventoryBudgetPercent != 30 || resolved.Context.ThresholdReservePercent != 22 {
		t.Fatalf("activated variant fields not applied: %+v", resolved.Context)
	}
	if resolved.Fingerprint() == baselineFingerprint {
		t.Fatal("activated variant must change the configuration fingerprint")
	}
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", memoryDBPath(stateDir, workspaceHash))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "UPDATE evolution_promotions SET schema_version = ? WHERE promotion_id = ?", memory.PromotionSchemaVersion-1, promoted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "UPDATE evolution_promotion_decisions SET schema_version = ? WHERE decision_id = ?", memory.PromotionSchemaVersion-1, promoted.DecisionID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ResolveTrialActivation(context.Background(), workspace, stateDir, configuration); err == nil {
		t.Fatal("legacy activated success evidence must not be applied to trial config")
	}
}

// TestVerifyTrustedPromotionSuiteRejectsLegacyVersions 锁定服务侧晋升证据门：
// 即使摘要与身份完全一致，旧套件或旧 Run 也不能伪装成当前可信成功。
func TestVerifyTrustedPromotionSuiteRejectsLegacyVersions(t *testing.T) {
	root := t.TempDir()
	evaluationID := "sha256:" + strings.Repeat("a", 64)
	path := filepath.Join(root, "evaluations", strings.ReplaceAll(evaluationID, ":", "_"), "suite-result.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, content string
		accept        bool
	}{
		{name: "legacy suite", content: `{"schema_version":2,"evaluation_id":"` + evaluationID + `","candidate_id":"candidate","status":"passed","records":[{"schema_version":2}]}`},
		{name: "legacy record", content: `{"schema_version":3,"evaluation_id":"` + evaluationID + `","candidate_id":"candidate","status":"passed","records":[{"schema_version":2}]}`},
		{name: "trusted versions", content: `{"schema_version":3,"evaluation_id":"` + evaluationID + `","candidate_id":"candidate","status":"passed","records":[{"schema_version":3}]}`, accept: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(test.content))
			opts := MemoryPromotionOptions{SuiteStateRoot: root, Promotion: memory.Promotion{
				EvaluationID: evaluationID, CandidateID: "candidate", SuiteResultDigest: "sha256:" + hex.EncodeToString(digest[:]),
			}}
			if err := verifyTrustedPromotionSuite(opts); (err == nil) != test.accept {
				t.Fatalf("accepted=%v want=%v err=%v", err == nil, test.accept, err)
			}
		})
	}
}
