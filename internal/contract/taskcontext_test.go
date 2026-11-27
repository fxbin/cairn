// taskcontext_test.go 回归任务上下文只读投影：逐字段落状态的触发条件、封闭的锚点与
// 原因码、摘要的确定性，以及交接摘要的结构与体积上限。
//
// Author: fxbin
package contract

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

// taskContextAcceptance 返回一份合法的验收合同。
func taskContextAcceptance(t *testing.T) AcceptanceContract {
	t.Helper()
	accepted, err := NewAcceptanceContract([]AcceptanceCriterion{
		{ID: "double", Statement: "翻倍正确", Required: true, Checks: []string{"test"}},
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	return accepted
}

// trustedTaskContextInput 返回全部来源可信的输入：已挂接的工作流、三个已收束的运行、
// 在位的制品与一份验证报告。
func trustedTaskContextInput(t *testing.T) TaskContextInput {
	t.Helper()
	accepted := taskContextAcceptance(t)
	return TaskContextInput{
		SchemaVersion: TaskContextSchemaVersion, TaskID: "task-1", WorkspaceHash: "sha256:workspace",
		Goal: "让翻倍正确", State: "active", Revision: 3, Remaining: []string{"验收"},
		Acceptance:    &accepted,
		WorkflowBound: true,
		Workflow: &TaskWorkflowInput{
			WorkflowID: "wf-1", ConfigFingerprint: "sha256:config", AuthorizationSource: "user:task-workflow",
			PlanSHA256: "sha256:plan", AuthorizedRevision: 2, Attached: true, LastSeq: 7, Phase: "ready",
			Attempts: []TaskAttemptInput{
				{Step: "worker", Number: 1, RunID: "run-worker", State: "succeeded", Terminal: &TaskRunAnchor{Sequence: 9, EventHash: "hash-9"}},
				{Step: "program", Number: 1, RunID: "run-program", State: "succeeded", Terminal: &TaskRunAnchor{Sequence: 12, EventHash: "hash-12"}, OutputSHA256: "sha256:program-output"},
				{Step: "review", Number: 1, RunID: "run-review", State: "succeeded", Terminal: &TaskRunAnchor{Sequence: 15, EventHash: "hash-15"}, OutputSHA256: "sha256:review-output"},
			},
		},
		Runs: []TaskRunInput{
			{RunID: "run-worker", Role: TaskRoleWorker, SessionID: "session-1", State: "completed", LastSeq: 9, HeadHash: "hash-9", Terminal: true,
				StartedOperations: 1, SettledOperations: 1,
				ApprovalDecisions: []TaskDecision{{Kind: "approval_granted", RunID: "run-worker", OperationID: "op-1", Fingerprint: "sha256:fingerprint", EventSeq: 4, EventHash: "hash-4"}}},
			{RunID: "run-program", Role: "program", State: "completed", LastSeq: 12, HeadHash: "hash-12", Terminal: true},
			{RunID: "run-review", Role: "review", State: "completed", LastSeq: 15, HeadHash: "hash-15", Terminal: true},
		},
		Artifacts: []TaskArtifactInput{
			{SHA256: "sha256:definition", Role: TaskRoleDefinition},
			{SHA256: "sha256:program-output", Role: TaskRoleAttemptOutput},
			{SHA256: "sha256:review-output", Role: TaskRoleAttemptOutput},
		},
		Reference: &TaskReferenceInput{Sequence: 7, Report: &TaskReportInput{
			WorkerRunID: "run-worker", ProgramRunID: "run-program", ReviewRunID: "run-review",
			Verdict: "passed", ScopeDigest: "sha256:scope", CoverageDigest: "sha256:coverage",
			Criteria: []CriterionVerdict{{CriterionID: "double", Required: true, Verdict: VerdictPassed}},
			Findings: []TaskFindingFact{{ID: "finding-1", TargetID: "run-worker", Category: "style", Severity: "low", Status: "inconclusive"}},
		}},
	}
}

// manualTaskContextInput 返回没有绑定工作流的输入：一个手工分配的运行，运行已通过验证。
func manualTaskContextInput(t *testing.T) TaskContextInput {
	t.Helper()
	input := trustedTaskContextInput(t)
	input.WorkflowBound, input.Workflow, input.Reference = false, nil, nil
	input.Runs = []TaskRunInput{{RunID: "run-assignment", Role: TaskRoleAssignment, State: "completed", LastSeq: 6, HeadHash: "hash-6",
		Terminal: true, VerificationSeq: 5, VerificationEventHash: "hash-5"}}
	input.Artifacts = nil
	return input
}

// taskContextFacts 按固定顺序列出十一个字段的校验视角，供逐字段断言。
func taskContextFacts(context TaskContextV1) []struct {
	name string
	fact taskFactView
} {
	return []struct {
		name string
		fact taskFactView
	}{
		{"goal", taskFactViewOf(context.Goal)},
		{"frozen_acceptance", taskFactViewOf(context.FrozenAcceptance)},
		{"progress", taskFactViewOf(context.Progress)},
		{"decisions", taskFactViewOf(context.Decisions)},
		{"runs", taskFactViewOf(context.Runs)},
		{"artifacts", taskFactViewOf(context.Artifacts)},
		{"evidence", taskFactViewOf(context.Evidence)},
		{"findings", taskFactViewOf(context.Findings)},
		{"remaining", taskFactViewOf(context.Remaining)},
		{"permissions", taskFactViewOf(context.Permissions)},
		{"unresolved_side_effects", taskFactViewOf(context.UnresolvedSideEffects)},
	}
}

// composeForTest 投影并要求投影通过自洽校验。
func composeForTest(t *testing.T, input TaskContextInput) TaskContextV1 {
	t.Helper()
	context, err := ComposeTaskContext(input)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if err := context.Validate(); err != nil {
		t.Fatalf("composed context is not self-consistent: %v", err)
	}
	return context
}

// assertTaskFact 断言指定字段的状态与原因码。
func assertTaskFact(t *testing.T, context TaskContextV1, field string, status TaskFactStatus, reason TaskContextReason) {
	t.Helper()
	for _, item := range taskContextFacts(context) {
		if item.name != field {
			continue
		}
		if item.fact.status != status || item.fact.reason != reason {
			t.Fatalf("%s = %s (%s), want %s (%s)", field, item.fact.status, item.fact.reason, status, reason)
		}
		return
	}
	t.Fatalf("unknown field %q", field)
}

// TestComposeTaskContextProjectsEveryFieldWhenSourcesAreTrusted 锁定全部来源可信时
// 十一个字段都是 known，且投影不随输入里切片的顺序变化。
func TestComposeTaskContextProjectsEveryFieldWhenSourcesAreTrusted(t *testing.T) {
	context := composeForTest(t, trustedTaskContextInput(t))
	for _, item := range taskContextFacts(context) {
		if item.fact.status != TaskFactKnown {
			t.Fatalf("%s = %s (%s), want known", item.name, item.fact.status, item.fact.reason)
		}
	}
	if context.Runs.Value == nil || len(*context.Runs.Value) != 3 || (*context.Runs.Value)[0].RunID != "run-program" {
		t.Fatalf("runs must be sorted by run id: %+v", context.Runs.Value)
	}
	if context.Decisions.Value == nil || len(*context.Decisions.Value) != 1 || (*context.Decisions.Value)[0].EventSeq != 4 {
		t.Fatalf("approval decision must come from the journal: %+v", context.Decisions.Value)
	}
	if context.UnresolvedSideEffects.Value == nil || (*context.UnresolvedSideEffects.Value).StartedOperations != 1 || (*context.UnresolvedSideEffects.Value).Settled != 1 {
		t.Fatalf("side effect summary = %+v", context.UnresolvedSideEffects.Value)
	}

	reversed := trustedTaskContextInput(t)
	reversed.Runs = []TaskRunInput{reversed.Runs[2], reversed.Runs[0], reversed.Runs[1]}
	reversed.Artifacts = []TaskArtifactInput{reversed.Artifacts[2], reversed.Artifacts[1], reversed.Artifacts[0]}
	reversed.Workflow.Attempts = []TaskAttemptInput{reversed.Workflow.Attempts[2], reversed.Workflow.Attempts[0], reversed.Workflow.Attempts[1]}
	other := composeForTest(t, reversed)
	first, err := canonicaljson.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	second, err := canonicaljson.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("input order must not change the projection")
	}
}

// TestComposeTaskContextFallsToUnknownWhenFactsAreAbsent 锁定尚无事实时逐字段落 unknown，
// 并写明原因码；任务来源的字段不受影响。
func TestComposeTaskContextFallsToUnknownWhenFactsAreAbsent(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TaskContextInput)
		field  string
		reason TaskContextReason
	}{
		{"legacy task without structured acceptance", func(input *TaskContextInput) {
			input.Acceptance, input.AcceptanceFailure = nil, &TaskSourceFailure{Status: TaskFactUnknown, Reason: TaskReasonNoStructuredAcceptance}
		}, "frozen_acceptance", TaskReasonNoStructuredAcceptance},
		{"task not bound to a workflow", func(input *TaskContextInput) {
			*input = manualTaskContextInput(t)
		}, "permissions", TaskReasonNotBoundToWorkflow},
		{"binding not attached", func(input *TaskContextInput) {
			input.Workflow, input.WorkflowFailure = nil, &TaskSourceFailure{Status: TaskFactUnknown, Reason: TaskReasonBindingNotAttached}
		}, "runs", TaskReasonBindingNotAttached},
		{"verification report not generated", func(input *TaskContextInput) {
			input.Reference = nil
		}, "evidence", TaskReasonReportNotGenerated},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := trustedTaskContextInput(t)
			testCase.mutate(&input)
			context := composeForTest(t, input)
			assertTaskFact(t, context, testCase.field, TaskFactUnknown, testCase.reason)
			assertTaskFact(t, context, "goal", TaskFactKnown, "")
		})
	}
}

// TestComposeTaskContextBlocksUntrustedDerivedFacts 锁定完整性失败时逐字段落 blocked，
// 并写明来源与原因码；没有一处把缺失或不可信表达成通过。
func TestComposeTaskContextBlocksUntrustedDerivedFacts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TaskContextInput)
		field  string
		reason TaskContextReason
	}{
		{"acceptance contract mismatch", func(input *TaskContextInput) {
			input.Acceptance, input.AcceptanceFailure = nil, &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonAcceptanceContractMismatch}
		}, "frozen_acceptance", TaskReasonAcceptanceContractMismatch},
		{"workflow chain invalid", func(input *TaskContextInput) {
			input.Workflow, input.WorkflowFailure = nil, &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonWorkflowChainInvalid}
		}, "permissions", TaskReasonWorkflowChainInvalid},
		{"journal chain invalid", func(input *TaskContextInput) {
			input.Runs, input.RunsFailure = nil, &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonJournalChainInvalid}
		}, "runs", TaskReasonJournalChainInvalid},
		{"terminal anchor behind the journal head", func(input *TaskContextInput) {
			input.Runs[0].LastSeq = 8
		}, "runs", TaskReasonTerminalAnchorMismatch},
		{"terminal anchor names a missing run", func(input *TaskContextInput) {
			input.Runs = input.Runs[1:]
		}, "runs", TaskReasonTerminalAnchorMismatch},
		{"artifact missing", func(input *TaskContextInput) {
			input.Artifacts[1].Missing = true
		}, "artifacts", TaskReasonArtifactMissing},
		{"artifact tampered", func(input *TaskContextInput) {
			input.Artifacts[2].Tampered = true
		}, "artifacts", TaskReasonArtifactTampered},
		{"completion acceptance digest differs from the frozen one", func(input *TaskContextInput) {
			input.State, input.Remaining = "completed", nil
			input.Completion = &TaskCompletionInput{RunID: "run-review", Sequence: 15, EventHash: "hash-15", AcceptanceDigest: "sha256:other"}
		}, "decisions", TaskReasonCompletionAnchorMismatch},
		{"completion anchors an earlier event", func(input *TaskContextInput) {
			input.State, input.Remaining = "completed", nil
			input.Completion = &TaskCompletionInput{RunID: "run-review", Sequence: 14, EventHash: "hash-15"}
		}, "decisions", TaskReasonCompletionAnchorMismatch},
		{"completed task without a completion record", func(input *TaskContextInput) {
			input.State, input.Remaining = "completed", nil
		}, "decisions", TaskReasonCompletionAnchorMismatch},
		{"reference view behind the authoritative projection", func(input *TaskContextInput) {
			input.Reference.Sequence = 6
		}, "evidence", TaskReasonReferenceViewStale},
		{"report refers to a run outside the task", func(input *TaskContextInput) {
			input.Reference.Report.ReviewRunID = "run-unknown"
		}, "evidence", TaskReasonReportRunOutOfScope},
		{"report refers to a run that is not terminal", func(input *TaskContextInput) {
			input.Runs[2].Terminal = false
		}, "evidence", TaskReasonReportRunOutOfScope},
		{"reference view cannot be verified", func(input *TaskContextInput) {
			input.Reference, input.ReferenceFailure = nil, &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonReferenceViewInvalid}
		}, "findings", TaskReasonReferenceViewInvalid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := trustedTaskContextInput(t)
			testCase.mutate(&input)
			context := composeForTest(t, input)
			assertTaskFact(t, context, testCase.field, TaskFactBlocked, testCase.reason)
			assertTaskFact(t, context, "goal", TaskFactKnown, "")
		})
	}
}

// TestComposeTaskContextBlocksTheWholeRunSetWhenTheWorkflowIsUntrusted 锁定工作流链
// 不可信时运行集合整体 blocked：不拿分配或尝试绑定的个别运行冒充全集。
func TestComposeTaskContextBlocksTheWholeRunSetWhenTheWorkflowIsUntrusted(t *testing.T) {
	input := trustedTaskContextInput(t)
	input.Workflow, input.WorkflowFailure = nil, &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonBindingMismatch}
	context := composeForTest(t, input)
	for _, field := range []string{"permissions", "decisions", "runs", "unresolved_side_effects", "artifacts", "evidence", "findings"} {
		assertTaskFact(t, context, field, TaskFactBlocked, TaskReasonBindingMismatch)
	}
	for _, field := range []string{"goal", "frozen_acceptance", "progress", "remaining"} {
		assertTaskFact(t, context, field, TaskFactKnown, "")
	}
	if context.Progress.Value == nil || context.Progress.Value.WorkflowPhase != "" {
		t.Fatalf("phase must stay empty when the workflow is untrusted: %+v", context.Progress.Value)
	}
}

// TestComposeTaskContextVerifiesCompletionOnBothPaths 锁定完成记录的核对：自主工作流
// 路径锚定审查运行的末事件，手工分配路径锚定分配运行的最后一次验证事件。
func TestComposeTaskContextVerifiesCompletionOnBothPaths(t *testing.T) {
	accepted := taskContextAcceptance(t)

	workflowPath := trustedTaskContextInput(t)
	workflowPath.State, workflowPath.Remaining = "completed", nil
	workflowPath.Completion = &TaskCompletionInput{RunID: "run-review", Sequence: 15, EventHash: "hash-15", AcceptanceDigest: accepted.Digest}
	context := composeForTest(t, workflowPath)
	assertTaskFact(t, context, "decisions", TaskFactKnown, "")
	var accept *TaskDecision
	for index := range *context.Decisions.Value {
		if (*context.Decisions.Value)[index].Kind == TaskDecisionAccept {
			accept = &(*context.Decisions.Value)[index]
		}
	}
	if accept == nil || accept.RunID != "run-review" || accept.EventSeq != 15 || accept.EventHash != "hash-15" {
		t.Fatalf("accept decision must anchor the review run head: %+v", context.Decisions.Value)
	}

	manualPath := manualTaskContextInput(t)
	manualPath.State, manualPath.Remaining = "completed", nil
	manualPath.Completion = &TaskCompletionInput{RunID: "run-assignment", Sequence: 5, EventHash: "hash-5", AcceptanceDigest: accepted.Digest}
	assertTaskFact(t, composeForTest(t, manualPath), "decisions", TaskFactKnown, "")

	manualPath.Completion.Sequence, manualPath.Completion.EventHash = 6, "hash-6"
	assertTaskFact(t, composeForTest(t, manualPath), "decisions", TaskFactBlocked, TaskReasonCompletionAnchorMismatch)
}

// TestComposeTaskContextRejectsContradictoryInput 锁定输入自洽性：每个来源要么有数据
// 要么有失败说明，失败说明的状态与原因码必须对应。
func TestComposeTaskContextRejectsContradictoryInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TaskContextInput)
	}{
		{"unsupported schema version", func(input *TaskContextInput) { input.SchemaVersion = 2 }},
		{"missing task id", func(input *TaskContextInput) { input.TaskID = "" }},
		{"neither acceptance nor failure", func(input *TaskContextInput) { input.Acceptance = nil }},
		{"both acceptance and failure", func(input *TaskContextInput) {
			input.AcceptanceFailure = &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonAcceptanceContractMismatch}
		}},
		{"bound without workflow data or failure", func(input *TaskContextInput) { input.Workflow = nil }},
		{"unbound carrying workflow data", func(input *TaskContextInput) { input.WorkflowBound = false }},
		{"both runs and a runs failure", func(input *TaskContextInput) {
			input.RunsFailure = &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonJournalChainInvalid}
		}},
		{"failure reason inconsistent with status", func(input *TaskContextInput) {
			input.Workflow, input.WorkflowFailure = nil, &TaskSourceFailure{Status: TaskFactUnknown, Reason: TaskReasonWorkflowChainInvalid}
		}},
		{"both reference view and reference failure", func(input *TaskContextInput) {
			input.ReferenceFailure = &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonReferenceViewInvalid}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := trustedTaskContextInput(t)
			testCase.mutate(&input)
			if _, err := ComposeTaskContext(input); err == nil {
				t.Fatal("contradictory input must be rejected")
			}
		})
	}
}

// TestTaskContextReasonsAreClosed 锁定原因码的封闭性：每个取值都对应唯一的状态，
// 未知取值不对应任何状态。
func TestTaskContextReasonsAreClosed(t *testing.T) {
	unknown := []TaskContextReason{TaskReasonNoStructuredAcceptance, TaskReasonNotBoundToWorkflow, TaskReasonBindingNotAttached, TaskReasonReportNotGenerated}
	blocked := []TaskContextReason{
		TaskReasonAcceptanceContractMismatch, TaskReasonWorkflowChainInvalid, TaskReasonWorkflowMissing, TaskReasonBindingMismatch,
		TaskReasonDefinitionMismatch, TaskReasonJournalUnavailable, TaskReasonJournalChainInvalid, TaskReasonRunMissing,
		TaskReasonTerminalAnchorMismatch, TaskReasonArtifactMissing, TaskReasonArtifactTampered, TaskReasonCompletionAnchorMismatch,
		TaskReasonReferenceViewInvalid, TaskReasonReferenceViewStale, TaskReasonReportRunOutOfScope,
	}
	seen := make(map[TaskContextReason]bool, len(unknown)+len(blocked))
	for _, group := range []struct {
		status  TaskFactStatus
		reasons []TaskContextReason
	}{{TaskFactUnknown, unknown}, {TaskFactBlocked, blocked}} {
		for _, reason := range group.reasons {
			if seen[reason] || strings.Contains(string(reason), "/") {
				t.Fatalf("reason %q must be unique and path-free", reason)
			}
			seen[reason] = true
			if got := taskReasonStatus(reason); got != group.status {
				t.Fatalf("reason %q maps to %q, want %q", reason, got, group.status)
			}
		}
	}
	if taskReasonStatus("not_a_reason") != "" {
		t.Fatal("unknown reasons must not map to a status")
	}
}

// TestTaskContextContentMirrorsTheProjection 锁定摘要覆盖投影的每个字段：摘要内容与投影
// 的字段逐一对应，只少摘要字段自身。给投影加字段而忘记加进摘要内容时，这里会失败。
func TestTaskContextContentMirrorsTheProjection(t *testing.T) {
	projection := reflect.TypeOf(TaskContextV1{})
	content := reflect.TypeOf(taskContextContent{})
	if projection.NumField() != content.NumField()+1 {
		t.Fatalf("projection has %d fields, digest content has %d", projection.NumField(), content.NumField())
	}
	for index := 0; index < content.NumField(); index++ {
		projected, digested := projection.Field(index), content.Field(index)
		if projected.Name != digested.Name || projected.Tag.Get("json") != digested.Tag.Get("json") {
			t.Fatalf("field %d differs: projection %q, digest content %q", index, projected.Name, digested.Name)
		}
	}
	last := projection.Field(projection.NumField() - 1)
	if last.Name != "Digest" || last.Tag.Get("json") != "digest" {
		t.Fatalf("the only field outside the digest must be Digest, got %q", last.Name)
	}
}

// TestTaskContextValidateRejectsTampering 锁定投影自洽校验：改动内容后摘要不符即拒绝，
// 状态与来源、锚点、原因码不对应也拒绝。
func TestTaskContextValidateRejectsTampering(t *testing.T) {
	context := composeForTest(t, trustedTaskContextInput(t))

	tampered := context
	tampered.Goal = knownTaskFact("别的目标", TaskFactSourceTask, *context.Goal.Anchor)
	if tampered.Validate() == nil {
		t.Fatal("changed content must invalidate the digest")
	}

	// 摘要重算后只剩结构问题：状态与来源、锚点、原因码不对应同样必须拒绝。
	unknownWithoutReason := context
	unknownWithoutReason.Runs = TaskFact[[]TaskRunFact]{Status: TaskFactUnknown}
	if redigested := redigestTaskContext(t, unknownWithoutReason); redigested.Validate() == nil {
		t.Fatal("unknown fact without a reason must be rejected")
	}

	unknownAnchor := context
	unknownAnchor.Permissions = knownTaskFact(*context.Permissions.Value, TaskFactSourceWorkflow, TaskAnchor{Kind: "run_count", Value: "3"})
	if redigested := redigestTaskContext(t, unknownAnchor); redigested.Validate() == nil {
		t.Fatal("anchor outside the closed vocabulary must be rejected")
	}

	unknownSource := context
	unknownSource.Runs = knownTaskFact(*context.Runs.Value, "database", *context.Runs.Anchor)
	if redigested := redigestTaskContext(t, unknownSource); redigested.Validate() == nil {
		t.Fatal("source outside the closed vocabulary must be rejected")
	}
}

// redigestTaskContext 重算摘要，让校验只面对结构问题。
func redigestTaskContext(t *testing.T, context TaskContextV1) TaskContextV1 {
	t.Helper()
	digest, err := taskContextDigest(context)
	if err != nil {
		t.Fatal(err)
	}
	context.Digest = digest
	return context
}

// TestRenderTaskHandoffListsKnownFactsOnly 锁定交接摘要只列出 known 的事实并带来源与
// 锚点，全可信时没有任何 unknown 或 blocked 标记，末尾是固定的免责说明。
func TestRenderTaskHandoffListsKnownFactsOnly(t *testing.T) {
	handoff := RenderTaskHandoff(composeForTest(t, trustedTaskContextInput(t)))
	for _, required := range []string{
		"task task-1\n", "goal 让翻倍正确\n", "acceptance digest=", "progress state=active revision=3 phase=ready sequence=7 anchor=task_revision:3",
		"permissions workflow=wf-1 config=", "evidence verdict=passed scope=scope coverage=coverage worker=run-worker program=run-program review=run-review",
		"  decision approval_granted run=run-worker operation=op-1 fingerprint=fingerprint seq=4 hash=hash-4\n",
		"  run run-program role=program state=completed terminal=true last_seq=12 head=hash-12\n",
		"  finding finding-1 status=inconclusive severity=low blocking=false\n",
		"  artifact definition role=definition present=true\n",
		"unresolved side effects started=1 settled=1 unsettled=none pending_reconcile=false pending_approval=false",
		TaskHandoffDisclaimer + "\n",
	} {
		if !strings.Contains(handoff, required) {
			t.Fatalf("handoff missing %q:\n%s", required, handoff)
		}
	}
	if strings.Contains(handoff, string(TaskFactUnknown)) || strings.Contains(handoff, string(TaskFactBlocked)) {
		t.Fatalf("fully trusted context must not carry status markers:\n%s", handoff)
	}
	if len(handoff) > TaskHandoffMaxBytes {
		t.Fatalf("handoff is %d bytes, limit %d", len(handoff), TaskHandoffMaxBytes)
	}
}

// TestRenderTaskHandoffMarksUnknownAndBlockedExplicitly 锁定 unknown 与 blocked 在交接
// 摘要里作为显式标记出现并带原因码，接收方不会把它们读成已完成或已通过。
func TestRenderTaskHandoffMarksUnknownAndBlockedExplicitly(t *testing.T) {
	input := trustedTaskContextInput(t)
	input.Acceptance, input.AcceptanceFailure = nil, &TaskSourceFailure{Status: TaskFactUnknown, Reason: TaskReasonNoStructuredAcceptance}
	input.Reference.Sequence = 6
	handoff := RenderTaskHandoff(composeForTest(t, input))
	for _, required := range []string{
		"acceptance unknown - no_structured_acceptance\n",
		"evidence blocked - reference_view_stale\n",
		"  findings blocked - reference_view_stale\n",
	} {
		if !strings.Contains(handoff, required) {
			t.Fatalf("handoff missing %q:\n%s", required, handoff)
		}
	}
	if strings.Contains(handoff, "verdict=passed") {
		t.Fatalf("blocked evidence must not be rendered as a verdict:\n%s", handoff)
	}
}

// TestRenderTaskHandoffStaysWithinTheSizeLimit 锁定交接摘要的体积上限：大量运行与超长
// 目标下仍不超过上限，并显式标出省略与截断。
func TestRenderTaskHandoffStaysWithinTheSizeLimit(t *testing.T) {
	input := manualTaskContextInput(t)
	input.Goal = strings.Repeat("很长", 50000)
	input.Remaining = make([]string, 2000)
	runs := make([]TaskRunInput, 0, 3000)
	for index := 0; index < 3000; index++ {
		runs = append(runs, TaskRunInput{RunID: fmt.Sprintf("run-%06d-0123456789abcdef0123456789abcdef", index), Role: TaskRoleAssignment, State: "completed", Terminal: true})
	}
	input.Runs = runs
	handoff := RenderTaskHandoff(composeForTest(t, input))
	if len(handoff) > TaskHandoffMaxBytes {
		t.Fatalf("handoff is %d bytes, limit %d", len(handoff), TaskHandoffMaxBytes)
	}
	for _, required := range []string{"...(truncated)", "omitted", TaskHandoffDisclaimer + "\n"} {
		if !strings.Contains(handoff, required) {
			t.Fatalf("handoff missing %q", required)
		}
	}
	if !strings.HasSuffix(handoff, TaskHandoffDisclaimer+"\n") {
		t.Fatalf("handoff must end with the disclaimer:\n%s", handoff[len(handoff)-200:])
	}
}

// TestRenderTaskHandoffOnlyUsesKnownLinePrefixes 锁定交接摘要的结构：每一行都以固定的
// 前缀开始，没有任何携带模型输出或工具结果正文的行。
func TestRenderTaskHandoffOnlyUsesKnownLinePrefixes(t *testing.T) {
	input := trustedTaskContextInput(t)
	input.Workflow, input.WorkflowFailure = nil, &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonWorkflowChainInvalid}
	input.AcceptanceFailure = &TaskSourceFailure{Status: TaskFactBlocked, Reason: TaskReasonAcceptanceContractMismatch}
	input.Acceptance = nil
	handoff := RenderTaskHandoff(composeForTest(t, input))
	prefixes := []string{
		"task ", "workspace ", "digest ", "goal ", "acceptance ", "progress ", "permissions ", "evidence ", "unresolved side effects ",
		"  remaining ", "  decision ", "  decisions ", "  run ", "  runs ", "  finding ", "  findings ", "  artifact ", "  artifacts ", "... ", TaskHandoffDisclaimer,
	}
	for _, line := range strings.Split(strings.TrimSuffix(handoff, "\n"), "\n") {
		matched := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(line, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("unexpected handoff line %q", line)
		}
	}
}

// TestRenderTaskHandoffCapsUnsettledOperationList 锁定未了结操作清单有上限：这一行是必需
// 内容，只列前若干个并标出省略数量，不会单凭它就把必需行整体顶过体积上限。
func TestRenderTaskHandoffCapsUnsettledOperationList(t *testing.T) {
	unsettled := make([]string, 0, 2000)
	for index := 0; index < 2000; index++ {
		unsettled = append(unsettled, fmt.Sprintf("op-%06d-0123456789abcdef0123456789abcdef", index))
	}
	context := composeForTest(t, manualTaskContextInput(t))
	context.UnresolvedSideEffects = knownTaskFact(TaskSideEffectSummary{Unsettled: unsettled, PendingReconcile: true},
		TaskFactSourceJournal, *context.UnresolvedSideEffects.Anchor)
	handoff := RenderTaskHandoff(context)
	if len(handoff) > TaskHandoffMaxBytes {
		t.Fatalf("handoff is %d bytes, limit %d", len(handoff), TaskHandoffMaxBytes)
	}
	if !strings.Contains(handoff, unsettled[TaskHandoffUnsettledPreview-1]+",...(+1980 more) pending_reconcile=true") {
		t.Fatalf("unresolved side effects must list the first items and the omitted count:\n%s", handoff)
	}
	for _, hidden := range []string{unsettled[TaskHandoffUnsettledPreview], unsettled[len(unsettled)-1]} {
		if strings.Contains(handoff, hidden+",") || strings.Contains(handoff, hidden+" ") {
			t.Fatalf("unresolved side effects leaked an omitted operation %q", hidden)
		}
	}
	if strings.Contains(handoff, "\n... handoff truncated\n") {
		t.Fatal("bounded mandatory lines must not need the last-resort truncation")
	}
	if !strings.HasSuffix(handoff, TaskHandoffDisclaimer+"\n") {
		t.Fatalf("handoff must end with the disclaimer:\n%s", handoff[len(handoff)-200:])
	}
}

// TestTaskHandoffWriterKeepsTheDisclaimerWhenMandatoryLinesOverflow 锁定最后的保护性截断
// 不会截掉免责说明：必需行本身超限时截断的是正文，免责说明完整地留在末尾，且不输出
// 残缺字符。正文里的省略计数会随正文一起被截掉，“被截断”这个标记已经足够说明摘要不完整。
func TestTaskHandoffWriterKeepsTheDisclaimerWhenMandatoryLinesOverflow(t *testing.T) {
	writer := taskHandoffWriter{}
	writer.required("goal %s", strings.Repeat("长", 9000))
	writer.optional("  run %s", "run-1")
	handoff := writer.render()
	if len(handoff) > TaskHandoffMaxBytes {
		t.Fatalf("handoff is %d bytes, limit %d", len(handoff), TaskHandoffMaxBytes)
	}
	if !utf8.ValidString(handoff) {
		t.Fatal("truncation split a multi-byte character")
	}
	if !strings.HasSuffix(handoff, "... handoff truncated\n"+TaskHandoffDisclaimer+"\n") {
		t.Fatalf("handoff must keep the disclaimer after the truncated body:\n%s", handoff[len(handoff)-200:])
	}
	if strings.Contains(handoff, "run-1") {
		t.Fatal("list lines must be dropped before the mandatory line is truncated")
	}
}
