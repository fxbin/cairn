// coverage_manifest_test.go 回归覆盖清单的状态机、分母保护、复用与失效。
//
// Author: fxbin
package contract

import "testing"

func testCoverageScope(t *testing.T) VerificationScope {
	t.Helper()
	accepted := testScopeAcceptance(t)
	scope, err := NewVerificationScope(accepted, testScopeObjects())
	if err != nil {
		t.Fatalf("new scope: %v", err)
	}
	return scope
}

func testEvidenceRef() *Ref {
	return &Ref{SHA256: "sha256:" + repeatHash('c'), Bytes: 64, MediaType: "application/json"}
}

func TestCoverageManifestInitialState(t *testing.T) {
	scope := testCoverageScope(t)
	manifest, err := NewCoverageManifest(scope)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	if err := manifest.ValidateAgainst(scope); err != nil {
		t.Fatalf("validate against scope: %v", err)
	}
	for _, entry := range manifest.Entries {
		if entry.Status != CoverageSelected {
			t.Fatalf("initial status = %s, want selected", entry.Status)
		}
	}
}

func TestCoverageManifestStatesAndCoverageSeparation(t *testing.T) {
	scope := testCoverageScope(t)
	manifest, err := NewCoverageManifest(scope)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	// obj-1 走完整执行链；obj-2 仅跳过（可选对象）。
	manifest, err = manifest.Apply([]CoverageUpdate{
		{ObjectID: "obj-1", Status: CoveragePending},
		{ObjectID: "obj-2", Status: CoveragePending},
	})
	if err != nil {
		t.Fatalf("to pending: %v", err)
	}
	manifest, err = manifest.Apply([]CoverageUpdate{{ObjectID: "obj-1", Status: CoverageRunning}})
	if err != nil {
		t.Fatalf("to running: %v", err)
	}
	manifest, err = manifest.Apply([]CoverageUpdate{{
		ObjectID: "obj-1", Status: CoverageCompleted, EvidenceRef: testEvidenceRef(),
		SubjectVersion: scope.Objects[0].Version, VerifierID: "verifier-1", RulesetVersion: "v1",
	}})
	if err != nil {
		t.Fatalf("to completed: %v", err)
	}
	manifest, err = manifest.Apply([]CoverageUpdate{{ObjectID: "obj-2", Status: CoverageSkipped, Reason: "可选对象预算耗尽"}})
	if err != nil {
		t.Fatalf("to skipped: %v", err)
	}
	summary, err := manifest.Summary(scope)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Required != 1 || summary.Covered != 1 || !summary.Complete {
		t.Fatalf("unexpected coverage summary: %+v", summary)
	}
	// 覆盖完整不代表通过：清单不携带通过判定。
	if summary.Executed != 1 || summary.AuthorizedExceptions != 0 {
		t.Fatalf("unexpected executed/exception counts: %+v", summary)
	}
}

func TestCoverageManifestDenominatorProtection(t *testing.T) {
	scope := testCoverageScope(t)
	manifest, err := NewCoverageManifest(scope)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	// 必需对象被排除也不得缩小分母；排必须有理由与授权。
	manifest, err = manifest.Apply([]CoverageUpdate{{ObjectID: "obj-1", Status: CoverageExcluded, Reason: "受控排除", Authorization: "user-approved"}})
	if err != nil {
		t.Fatalf("exclude required object: %v", err)
	}
	summary, err := manifest.Summary(scope)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Required != 1 || summary.Covered != 0 || summary.Complete {
		t.Fatalf("exclusion must not shrink denominator: %+v", summary)
	}
	if len(summary.UncoveredRequired) != 1 || summary.UncoveredRequired[0] != "obj-1" {
		t.Fatalf("uncovered required objects = %v", summary.UncoveredRequired)
	}
	// 删除条目应被拒绝。
	manifest.Entries = manifest.Entries[:1]
	if err := manifest.ValidateAgainst(scope); err == nil {
		t.Fatal("missing frozen object was accepted")
	}
}

func TestCoverageManifestRejectsIllegalTransitions(t *testing.T) {
	scope := testCoverageScope(t)
	manifest, err := NewCoverageManifest(scope)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	if _, err := manifest.Apply([]CoverageUpdate{{ObjectID: "obj-1", Status: CoverageCompleted, EvidenceRef: testEvidenceRef(), SubjectVersion: scope.Objects[0].Version, VerifierID: "v", RulesetVersion: "v1"}}); err == nil {
		t.Fatal("selected -> completed without pending/running was accepted")
	}
	if _, err := manifest.Apply([]CoverageUpdate{{ObjectID: "missing", Status: CoveragePending}}); err == nil {
		t.Fatal("update for unknown object was accepted")
	}
}

func TestCoverageManifestInvalidateRunningKeepsReason(t *testing.T) {
	scope := testCoverageScope(t)
	manifest, err := NewCoverageManifest(scope)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	manifest, err = manifest.Apply([]CoverageUpdate{
		{ObjectID: "obj-1", Status: CoveragePending},
		{ObjectID: "obj-1", Status: CoverageRunning},
	})
	if err != nil {
		t.Fatalf("to running: %v", err)
	}
	manifest, err = manifest.Invalidate([]string{"obj-1"}, "产物在执行中变化，保守重新验证")
	if err != nil {
		t.Fatalf("invalidate running object: %v", err)
	}
	for _, entry := range manifest.Entries {
		if entry.ObjectID == "obj-1" {
			if entry.Status != CoveragePending {
				t.Fatalf("status = %s, want pending", entry.Status)
			}
			if entry.Reason == "" {
				t.Fatal("invalidation reason was discarded")
			}
		}
	}
}

func TestCoverageManifestFailedAndWaivedStates(t *testing.T) {
	scope := testCoverageScope(t)
	manifest, err := NewCoverageManifest(scope)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	manifest, err = manifest.Apply([]CoverageUpdate{
		{ObjectID: "obj-1", Status: CoveragePending},
		{ObjectID: "obj-1", Status: CoverageRunning},
	})
	if err != nil {
		t.Fatalf("to running: %v", err)
	}
	// Failed 表示执行失败，必须带理由；不等于发现业务缺陷。
	manifest, err = manifest.Apply([]CoverageUpdate{{ObjectID: "obj-1", Status: CoverageFailed, Reason: "工具超时"}})
	if err != nil {
		t.Fatalf("to failed: %v", err)
	}
	summary, err := manifest.Summary(scope)
	if err != nil {
		t.Fatalf("summary after failed: %v", err)
	}
	if summary.Covered != 0 || summary.Complete {
		t.Fatalf("failed object must not count as covered: %+v", summary)
	}
	// 失败可退回 Pending 后按合法授权豁免。
	manifest, err = manifest.Apply([]CoverageUpdate{{ObjectID: "obj-1", Status: CoveragePending}})
	if err != nil {
		t.Fatalf("failed to pending: %v", err)
	}
	if _, err := manifest.Apply([]CoverageUpdate{{ObjectID: "obj-1", Status: CoverageWaived, Reason: "高风险"}}); err == nil {
		t.Fatal("waived without authorization was accepted")
	}
	manifest, err = manifest.Apply([]CoverageUpdate{{ObjectID: "obj-1", Status: CoverageWaived, Reason: "高风险", Authorization: "user-approved"}})
	if err != nil {
		t.Fatalf("waived with authorization: %v", err)
	}
	summary, err = manifest.Summary(scope)
	if err != nil {
		t.Fatalf("summary after waived: %v", err)
	}
	if summary.AuthorizedExceptions != 1 || summary.Covered != 0 || summary.Complete {
		t.Fatalf("waived object must not count as covered: %+v", summary)
	}
}

func TestCoverageManifestInvalidationAndReuse(t *testing.T) {
	scope := testCoverageScope(t)
	manifest, err := NewCoverageManifest(scope)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	manifest, err = manifest.Apply([]CoverageUpdate{
		{ObjectID: "obj-1", Status: CoveragePending},
		{ObjectID: "obj-1", Status: CoverageRunning},
		{ObjectID: "obj-1", Status: CoverageCompleted, EvidenceRef: testEvidenceRef(), SubjectVersion: scope.Objects[0].Version, VerifierID: "verifier-1", RulesetVersion: "v1"},
	})
	if err != nil {
		t.Fatalf("complete object: %v", err)
	}
	validCandidate := ReuseCandidate{
		ObjectID: "obj-1", SubjectVersion: scope.Objects[0].Version,
		AcceptanceDigest: scope.AcceptanceDigest, Ruleset: "coding", RulesetVersion: "v1",
		VerifierID: "verifier-1", VerifierPolicy: "read-only", EvidenceRef: *testEvidenceRef(),
		Conclusion: ConclusionPassed, CoverageComplete: true,
	}
	if err := CanReuse(scope, validCandidate); err != nil {
		t.Fatalf("valid reuse candidate rejected: %v", err)
	}
	stale := validCandidate
	stale.SubjectVersion = "sha256:stale"
	if err := CanReuse(scope, stale); err == nil {
		t.Fatal("stale subject version was accepted for reuse")
	}
	invalidated := validCandidate
	invalidated.Invalidated = true
	if err := CanReuse(scope, invalidated); err == nil {
		t.Fatal("invalidated candidate was accepted for reuse")
	}
	notPassed := validCandidate
	notPassed.Conclusion = ConclusionIncomplete
	if err := CanReuse(scope, notPassed); err == nil {
		t.Fatal("incomplete conclusion was accepted for reuse")
	}
	notCovered := validCandidate
	notCovered.CoverageComplete = false
	if err := CanReuse(scope, notCovered); err == nil {
		t.Fatal("incomplete coverage was accepted for reuse")
	}
	fakeDigest := validCandidate
	fakeDigest.EvidenceRef.SHA256 = "x"
	if err := CanReuse(scope, fakeDigest); err == nil {
		t.Fatal("fake evidence digest was accepted for reuse")
	}
	emptyVerifier := validCandidate
	emptyVerifier.VerifierID = ""
	if err := CanReuse(scope, emptyVerifier); err == nil {
		t.Fatal("empty verifier identity was accepted for reuse")
	}
	wrongContract := validCandidate
	wrongContract.AcceptanceDigest = "sha256:other"
	if err := CanReuse(scope, wrongContract); err == nil {
		t.Fatal("mismatched acceptance digest was accepted for reuse")
	}
	// 通用 Apply 不得绕过有效性核验进入 Reused（从 Pending 也不行）。
	pending, err := manifest.Invalidate([]string{"obj-1"}, "先退回 Pending 再尝试复用")
	if err != nil {
		t.Fatalf("invalidate to pending: %v", err)
	}
	if _, err := pending.Apply([]CoverageUpdate{{
		ObjectID: "obj-1", Status: CoverageReused, EvidenceRef: testEvidenceRef(),
		SubjectVersion: scope.Objects[0].Version, VerifierID: "verifier-1", RulesetVersion: "v1",
	}}); err == nil {
		t.Fatal("unvalidated reused status was accepted through Apply")
	}
	// MarkReused 通过全部核验后应可进入 Reused。
	marked, err := pending.MarkReused(scope, validCandidate)
	if err != nil || len(marked.Entries) == 0 {
		t.Fatalf("MarkReused failed: %v", err)
	}
	for _, entry := range marked.Entries {
		if entry.ObjectID == "obj-1" && entry.Status != CoverageReused {
			t.Fatalf("MarkReused did not set reused status: %+v", entry)
		}
	}
	manifest, err = manifest.Invalidate([]string{"obj-1"}, "产物变化，保守重新验证")
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	for _, entry := range manifest.Entries {
		if entry.ObjectID == "obj-1" {
			if entry.Status != CoveragePending || entry.EvidenceRef != nil {
				t.Fatalf("invalidated entry was not reset: %+v", entry)
			}
		}
	}
}
