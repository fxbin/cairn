// 本文件回归任务上下文的只读读取入口：确定性、只读、逐字段落状态与完成记录核对。
package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/canonicaljson"
	tasksqlite "github.com/fxbin/cairn/internal/capabilities/task/sqlite"
	"github.com/fxbin/cairn/internal/contract"
)

// taskContextFixture 是一个“通过但未验收”的自主任务，作用域已解析。
type taskContextFixture struct {
	service Service
	scope   TaskScope
	hash    string
	id      TaskID
	view    TaskView
}

// newTaskContextFixture 建立夹具：工作流的三个运行都已收束，验证报告判定为通过。
func newTaskContextFixture(t *testing.T) taskContextFixture {
	t.Helper()
	service, scope, current := newTaskWorkflowFixture(t)
	view, err := service.StartTaskWorkflow(context.Background(), scope, current.ID, TaskWorkflowOptions{Revision: current.Revision, ReworkAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	resolved, hash, err := scope.resolve()
	if err != nil {
		t.Fatal(err)
	}
	return taskContextFixture{service: service, scope: resolved, hash: hash, id: current.ID, view: view}
}

// read 以全新的服务实例冷读任务上下文，并要求投影通过自洽校验。
func (fixture taskContextFixture) read(t *testing.T) contract.TaskContextV1 {
	t.Helper()
	context, err := (Service{}).TaskContext(context.Background(), fixture.scope, fixture.id)
	if err != nil {
		t.Fatalf("task context: %v", err)
	}
	if err := context.Validate(); err != nil {
		t.Fatalf("task context is not self-consistent: %v", err)
	}
	return context
}

// journalPath 返回夹具工作区的运行日志库路径。
func (fixture taskContextFixture) journalPath() string {
	return filepath.Join(fixture.scope.StateDir, "workspaces", safeWorkspaceDir(fixture.hash), "runs.db")
}

// workflowPath 返回夹具工作区的工作流库路径。
func (fixture taskContextFixture) workflowPath() string {
	return workflowDBPath(fixture.scope.StateDir, fixture.hash)
}

// artifactPath 返回指定摘要在内容寻址库里的文件路径。
func (fixture taskContextFixture) artifactPath(digest string) string {
	trimmed := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(manifestArtifactRoot(fixture.scope.StateDir, fixture.hash), "sha256", trimmed[:2], trimmed)
}

// taskContextFact 是单个字段的状态与原因码。
type taskContextFact struct {
	status contract.TaskFactStatus
	reason contract.TaskContextReason
}

// taskContextFacts 按固定顺序列出十一个字段。
func taskContextFacts(context contract.TaskContextV1) []struct {
	name string
	fact taskContextFact
} {
	return []struct {
		name string
		fact taskContextFact
	}{
		{"goal", taskContextFact{context.Goal.Status, context.Goal.Reason}},
		{"frozen_acceptance", taskContextFact{context.FrozenAcceptance.Status, context.FrozenAcceptance.Reason}},
		{"progress", taskContextFact{context.Progress.Status, context.Progress.Reason}},
		{"decisions", taskContextFact{context.Decisions.Status, context.Decisions.Reason}},
		{"runs", taskContextFact{context.Runs.Status, context.Runs.Reason}},
		{"artifacts", taskContextFact{context.Artifacts.Status, context.Artifacts.Reason}},
		{"evidence", taskContextFact{context.Evidence.Status, context.Evidence.Reason}},
		{"findings", taskContextFact{context.Findings.Status, context.Findings.Reason}},
		{"remaining", taskContextFact{context.Remaining.Status, context.Remaining.Reason}},
		{"permissions", taskContextFact{context.Permissions.Status, context.Permissions.Reason}},
		{"unresolved_side_effects", taskContextFact{context.UnresolvedSideEffects.Status, context.UnresolvedSideEffects.Reason}},
	}
}

// assertTaskContextFacts 断言十一个字段的状态与原因码逐一符合预期。
func assertTaskContextFacts(t *testing.T, context contract.TaskContextV1, expected map[string]taskContextFact) {
	t.Helper()
	if len(expected) != 11 {
		t.Fatalf("expected map must cover all 11 fields, got %d", len(expected))
	}
	for _, item := range taskContextFacts(context) {
		want, found := expected[item.name]
		if !found {
			t.Fatalf("field %q missing from expectations", item.name)
		}
		if item.fact.status != want.status || item.fact.reason != want.reason {
			t.Fatalf("%s = %s (%s), want %s (%s)", item.name, item.fact.status, item.fact.reason, want.status, want.reason)
		}
	}
}

// knownTaskContextFields 返回当前为 known 的字段名集合。
func knownTaskContextFields(context contract.TaskContextV1) map[string]bool {
	known := make(map[string]bool)
	for _, item := range taskContextFacts(context) {
		known[item.name] = item.fact.status == contract.TaskFactKnown
	}
	return known
}

// expectKnown 返回一个 known 的预期。
func expectKnown() taskContextFact {
	return taskContextFact{status: contract.TaskFactKnown}
}

// expectBlocked 返回指定原因码的 blocked 预期。
func expectBlocked(reason contract.TaskContextReason) taskContextFact {
	return taskContextFact{status: contract.TaskFactBlocked, reason: reason}
}

// expectUnknown 返回指定原因码的 unknown 预期。
func expectUnknown(reason contract.TaskContextReason) taskContextFact {
	return taskContextFact{status: contract.TaskFactUnknown, reason: reason}
}

// withTaskContextFacts 合并多组预期，后者覆盖前者。
func withTaskContextFacts(groups ...map[string]taskContextFact) map[string]taskContextFact {
	merged := make(map[string]taskContextFact)
	for _, group := range groups {
		for name, fact := range group {
			merged[name] = fact
		}
	}
	return merged
}

// taskContextTaskFacts 返回四个任务来源字段都 known 的预期。
func taskContextTaskFacts() map[string]taskContextFact {
	return map[string]taskContextFact{
		"goal": expectKnown(), "frozen_acceptance": expectKnown(), "progress": expectKnown(), "remaining": expectKnown(),
	}
}

// taskContextStoredFacts 返回权限与制品两个独立来源的预期。
func taskContextStoredFacts(fact taskContextFact) map[string]taskContextFact {
	return map[string]taskContextFact{"permissions": fact, "artifacts": fact}
}

// taskContextRunFacts 返回由运行 Journal 派生的五个字段的预期。
func taskContextRunFacts(fact taskContextFact) map[string]taskContextFact {
	return map[string]taskContextFact{
		"runs": fact, "decisions": fact, "unresolved_side_effects": fact, "evidence": fact, "findings": fact,
	}
}

// taskContextWorkflowFacts 返回由工作流派生的七个字段的预期。
func taskContextWorkflowFacts(fact taskContextFact) map[string]taskContextFact {
	return withTaskContextFacts(taskContextStoredFacts(fact), taskContextRunFacts(fact))
}

// stateDirDigest 计算状态目录的逐文件摘要：相对路径与内容哈希排序后整体取哈希。
func stateDirDigest(t *testing.T, root string) string {
	t.Helper()
	type entry struct {
		path   string
		digest string
	}
	entries := make([]entry, 0)
	if err := filepath.WalkDir(root, func(path string, item fs.DirEntry, err error) error {
		if err != nil || item.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		entries = append(entries, entry{path: relative, digest: hex.EncodeToString(sum[:])})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].path < entries[right].path })
	hasher := sha256.New()
	for _, item := range entries {
		hasher.Write([]byte(item.path))
		hasher.Write([]byte{0})
		hasher.Write([]byte(item.digest))
		hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// execTaskContextSQL 以可写连接执行一条语句，用于制造损坏。
func execTaskContextSQL(t *testing.T, path, query string, arguments ...any) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(query, arguments...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// queryTaskContextSQL 返回一条标量查询的结果。
func queryTaskContextSQL(t *testing.T, path, query string, arguments ...any) int64 {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var value int64
	if err := database.QueryRow(query, arguments...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// TestTaskContextIsDeterministicAndReadOnly 锁定同一存储快照的两次读取逐字节相同，
// 且读取不改变状态目录里的任何文件；通过但未验收时十一个字段都是 known。
func TestTaskContextIsDeterministicAndReadOnly(t *testing.T) {
	fixture := newTaskContextFixture(t)
	before := stateDirDigest(t, fixture.scope.StateDir)
	first := fixture.read(t)
	after := stateDirDigest(t, fixture.scope.StateDir)
	second := fixture.read(t)
	if before != after {
		t.Fatal("read-only projection changed the state directory")
	}
	firstJSON, err := canonicaljson.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := canonicaljson.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("two reads of the same snapshot must be byte-identical")
	}
	for _, item := range taskContextFacts(first) {
		if item.fact.status != contract.TaskFactKnown {
			t.Fatalf("%s = %s (%s), want known", item.name, item.fact.status, item.fact.reason)
		}
	}
	report := fixture.view.Workflow.Verification
	if len(*first.Runs.Value) != 3 {
		t.Fatalf("runs = %+v", *first.Runs.Value)
	}
	progress := first.Progress.Value
	if progress.State != string(fixture.view.State) || progress.Revision != fixture.view.Revision || progress.WorkflowPhase != fixture.view.Workflow.Phase {
		t.Fatalf("progress = %+v, view state %s revision %d phase %s", progress, fixture.view.State, fixture.view.Revision, fixture.view.Workflow.Phase)
	}
	if evidence := first.Evidence.Value; evidence.Verdict != string(report.Evaluation.Verdict) || evidence.WorkerRunID != string(report.WorkerRunID) ||
		evidence.ProgramRunID != string(report.ProgramRunID) || evidence.ReviewRunID != string(report.ReviewRunID) ||
		evidence.ScopeDigest != report.Scope.Digest || evidence.CoverageDigest != report.Coverage.Digest {
		t.Fatalf("evidence = %+v, report %+v", evidence, report)
	}
	if permissions := first.Permissions.Value; permissions.ConfigFingerprint != fixture.view.Workflow.ConfigFingerprint ||
		permissions.WorkflowID != fixture.view.WorkflowBinding.WorkflowID || !permissions.Attached {
		t.Fatalf("permissions = %+v", permissions)
	}
	if acceptance := first.FrozenAcceptance.Value; acceptance.AcceptanceDigest != fixture.view.Definition.AcceptanceDigest {
		t.Fatalf("frozen acceptance = %+v", acceptance)
	}
}

// TestTaskContextKnowledgeNeverRegressesAcrossTaskProgress 锁定任务推进时已知的字段
// 不会退回 unknown：创建后、通过后、显式验收后各读一次，已知字段只增不减；验收后
// 完成记录锚定审查运行的链头，验收摘要等于冻结摘要。
func TestTaskContextKnowledgeNeverRegressesAcrossTaskProgress(t *testing.T) {
	ctx := context.Background()
	service, scope, current := newTaskWorkflowFixture(t)
	created, err := (Service{}).TaskContext(ctx, scope, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 尚未绑定工作流：没有权限摘要与验证报告；运行与制品是已知且为空的清单。
	assertTaskContextFacts(t, created, withTaskContextFacts(taskContextTaskFacts(), map[string]taskContextFact{
		"runs": expectKnown(), "decisions": expectKnown(), "unresolved_side_effects": expectKnown(), "artifacts": expectKnown(),
		"permissions": expectUnknown(contract.TaskReasonNotBoundToWorkflow),
		"evidence":    expectUnknown(contract.TaskReasonNotBoundToWorkflow),
		"findings":    expectUnknown(contract.TaskReasonNotBoundToWorkflow),
	}))

	view, err := service.StartTaskWorkflow(ctx, scope, current.ID, TaskWorkflowOptions{Revision: current.Revision, ReworkAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	fixture := taskContextFixture{service: service, scope: scope, id: current.ID, view: view}
	passed := fixture.read(t)

	report := view.Workflow.Verification
	completed, err := service.CompleteTask(ctx, scope, current.ID, view.Revision, report.ReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	accepted := fixture.read(t)

	for _, step := range []struct {
		name   string
		before contract.TaskContextV1
		after  contract.TaskContextV1
	}{{"passed", created, passed}, {"accepted", passed, accepted}} {
		before, after := knownTaskContextFields(step.before), knownTaskContextFields(step.after)
		for field := range before {
			if before[field] && !after[field] {
				t.Fatalf("known field %q regressed after the task was %s", field, step.name)
			}
		}
	}
	var accept *contract.TaskDecision
	for index := range *accepted.Decisions.Value {
		if (*accepted.Decisions.Value)[index].Kind == contract.TaskDecisionAccept {
			accept = &(*accepted.Decisions.Value)[index]
		}
	}
	reviewSeq, reviewHash := uint64(0), ""
	for _, run := range *accepted.Runs.Value {
		if run.RunID == string(report.ReviewRunID) {
			reviewSeq, reviewHash = run.LastSeq, run.HeadHash
		}
	}
	if accept == nil || accept.RunID != string(report.ReviewRunID) || accept.EventSeq != reviewSeq || accept.EventHash != reviewHash ||
		accept.EventSeq != completed.Completion.Sequence || accept.EventHash != completed.Completion.EventHash {
		t.Fatalf("accept decision = %+v, completion %+v, review head %d/%s", accept, completed.Completion, reviewSeq, reviewHash)
	}
	if completed.Completion.AcceptanceDigest != accepted.FrozenAcceptance.Value.AcceptanceDigest {
		t.Fatal("completion digest must equal the frozen acceptance digest")
	}
	if accepted.Progress.Value.State != string(completed.State) {
		t.Fatalf("progress state = %q, want %s", accepted.Progress.Value.State, completed.State)
	}
}

// TestTaskContextReportsUntrustedSourcesPerField 锁定各种损坏下逐字段的状态：由不可信
// 来源派生的字段落 blocked 并带原因码，任务来源的字段保持 known。
func TestTaskContextReportsUntrustedSourcesPerField(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(*testing.T, taskContextFixture)
		expect map[string]taskContextFact
	}{
		{"journal payload rewritten", func(t *testing.T, fixture taskContextFixture) {
			execTaskContextSQL(t, fixture.journalPath(), "UPDATE events SET payload_json = ? WHERE run_id = ? AND seq = 1",
				[]byte(`{"tampered":true}`), string(fixture.view.Workflow.Verification.WorkerRunID))
		}, withTaskContextFacts(taskContextTaskFacts(), taskContextStoredFacts(expectKnown()),
			taskContextRunFacts(expectBlocked(contract.TaskReasonJournalChainInvalid)))},
		{"run events missing", func(t *testing.T, fixture taskContextFixture) {
			execTaskContextSQL(t, fixture.journalPath(), "DELETE FROM events WHERE run_id = ?", string(fixture.view.Workflow.Verification.ReviewRunID))
		}, withTaskContextFacts(taskContextTaskFacts(), taskContextStoredFacts(expectKnown()),
			taskContextRunFacts(expectBlocked(contract.TaskReasonRunMissing)))},
		{"journal head truncated behind the declared terminal anchor", func(t *testing.T, fixture taskContextFixture) {
			review := string(fixture.view.Workflow.Verification.ReviewRunID)
			last := queryTaskContextSQL(t, fixture.journalPath(), "SELECT MAX(seq) FROM events WHERE run_id = ?", review)
			execTaskContextSQL(t, fixture.journalPath(), "DELETE FROM events WHERE run_id = ? AND seq >= ?", review, last-1)
		}, withTaskContextFacts(taskContextTaskFacts(), taskContextStoredFacts(expectKnown()),
			taskContextRunFacts(expectBlocked(contract.TaskReasonTerminalAnchorMismatch)))},
		{"journal unavailable", func(t *testing.T, fixture taskContextFixture) {
			if err := os.Rename(fixture.journalPath(), fixture.journalPath()+".moved"); err != nil {
				t.Fatal(err)
			}
		}, withTaskContextFacts(taskContextTaskFacts(), taskContextStoredFacts(expectKnown()),
			taskContextRunFacts(expectBlocked(contract.TaskReasonJournalUnavailable)))},
		{"workflow event rewritten", func(t *testing.T, fixture taskContextFixture) {
			execTaskContextSQL(t, fixture.workflowPath(), "UPDATE workflow_events SET event_hash = '0' WHERE workflow_id = ? AND seq = 1",
				fixture.view.WorkflowBinding.WorkflowID)
		}, withTaskContextFacts(taskContextTaskFacts(), taskContextWorkflowFacts(expectBlocked(contract.TaskReasonWorkflowChainInvalid)))},
		{"workflow store missing", func(t *testing.T, fixture taskContextFixture) {
			if err := os.Rename(fixture.workflowPath(), fixture.workflowPath()+".moved"); err != nil {
				t.Fatal(err)
			}
		}, withTaskContextFacts(taskContextTaskFacts(), taskContextWorkflowFacts(expectBlocked(contract.TaskReasonWorkflowMissing)))},
		{"definition artifact missing", func(t *testing.T, fixture taskContextFixture) {
			if err := os.Remove(fixture.artifactPath(fixture.view.WorkflowBinding.DefinitionArtifact.SHA256)); err != nil {
				t.Fatal(err)
			}
		}, withTaskContextFacts(taskContextTaskFacts(), taskContextWorkflowFacts(expectBlocked(contract.TaskReasonDefinitionMismatch)))},
		{"attempt output artifact rewritten", func(t *testing.T, fixture taskContextFixture) {
			first := fixture.read(t)
			for _, artifact := range *first.Artifacts.Value {
				if artifact.Role != contract.TaskRoleAttemptOutput {
					continue
				}
				if err := os.WriteFile(fixture.artifactPath(artifact.SHA256), []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
				break
			}
		}, withTaskContextFacts(taskContextTaskFacts(), map[string]taskContextFact{
			"permissions": expectKnown(), "artifacts": expectBlocked(contract.TaskReasonArtifactTampered),
			"runs": expectKnown(), "decisions": expectKnown(), "unresolved_side_effects": expectKnown(),
			"evidence": expectBlocked(contract.TaskReasonReferenceViewInvalid), "findings": expectBlocked(contract.TaskReasonReferenceViewInvalid),
		})},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTaskContextFixture(t)
			testCase.tamper(t, fixture)
			assertTaskContextFacts(t, fixture.read(t), testCase.expect)
		})
	}
}

// TestTaskContextDescribesTasksWithoutARunningWorkflow 锁定没有可读工作流事实时的状态：
// 已绑定未挂接、手工分配已验收及其完成证据缺失或被改写，以及任务不存在。
func TestTaskContextDescribesTasksWithoutARunningWorkflow(t *testing.T) {
	ctx := context.Background()

	t.Run("binding not attached", func(t *testing.T) {
		_, scope, current := newTaskWorkflowFixture(t)
		resolved, hash, err := scope.resolve()
		if err != nil {
			t.Fatal(err)
		}
		binding, err := freezeTaskWorkflow(resolved, current, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := current.PrepareWorkflow(binding); err != nil {
			t.Fatal(err)
		}
		store, err := tasksqlite.Open(ctx, resolved.databasePath(hash), false)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitTaskWorkflowBinding(ctx, store, &current); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		context, err := (Service{}).TaskContext(ctx, resolved, current.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskContextFacts(t, context, withTaskContextFacts(taskContextTaskFacts(),
			taskContextWorkflowFacts(expectUnknown(contract.TaskReasonBindingNotAttached))))
	})

	t.Run("manual assignment", func(t *testing.T) {
		service, scope, current := newTaskWorkflowFixture(t)
		view, err := service.RunTask(ctx, scope, current.ID, TaskRunOptions{Revision: current.Revision, AssignmentID: "manual-assignment", SessionID: "manual-session"})
		if err != nil || len(view.Assignments) != 1 || !view.Assignments[0].settled() {
			t.Fatalf("manual assignment failed: %v %+v", err, view)
		}
		resolved, hash, err := scope.resolve()
		if err != nil {
			t.Fatal(err)
		}
		fixture := taskContextFixture{service: service, scope: resolved, hash: hash, id: current.ID, view: view}
		assigned := fixture.read(t)
		assertTaskContextFacts(t, assigned, withTaskContextFacts(taskContextTaskFacts(), taskContextRunFacts(expectKnown()),
			map[string]taskContextFact{
				"permissions": expectUnknown(contract.TaskReasonNotBoundToWorkflow),
				"evidence":    expectUnknown(contract.TaskReasonNotBoundToWorkflow),
				"findings":    expectUnknown(contract.TaskReasonNotBoundToWorkflow),
				"artifacts":   expectKnown(),
			}))
		if runs := *assigned.Runs.Value; len(runs) != 1 || runs[0].Role != contract.TaskRoleAssignment {
			t.Fatalf("runs = %+v", runs)
		}
		// 手工分配路径的显式验收需要标准级证据与评估齐备，仓库里没有它的夹具；
		// 该路径的完成记录核对只在合同层的单元测试里覆盖。
	})

	t.Run("unknown task", func(t *testing.T) {
		_, scope, _ := newTaskWorkflowFixture(t)
		if _, err := (Service{}).TaskContext(ctx, scope, "missing-task"); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("unknown task = %v", err)
		}
	})
}

// TestTaskContextClassifiesArtifactState 锁定制品在位状态的三种判定：内容与摘要一致、
// 文件缺失与内容被改写。
func TestTaskContextClassifiesArtifactState(t *testing.T) {
	_, scope, _ := newTaskWorkflowFixture(t)
	resolved, hash, err := scope.resolve()
	if err != nil {
		t.Fatal(err)
	}
	fixture := taskContextFixture{scope: resolved, hash: hash}
	content := []byte("task context artifact state probe")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	path := fixture.artifactPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	referenced := []contract.TaskArtifactInput{{SHA256: digest, Role: contract.TaskRoleCompletionEvidence}}
	state := func() contract.TaskArtifactInput {
		t.Helper()
		checked := taskContextArtifacts(resolved.StateDir, hash, referenced)
		if len(checked) != 1 {
			t.Fatalf("checked artifacts = %+v", checked)
		}
		return checked[0]
	}
	if present := state(); present.Missing || present.Tampered {
		t.Fatalf("intact artifact = %+v", present)
	}
	if err := os.WriteFile(path, []byte("rewritten"), 0600); err != nil {
		t.Fatal(err)
	}
	if tampered := state(); !tampered.Tampered || tampered.Missing {
		t.Fatalf("rewritten artifact = %+v", tampered)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if missing := state(); !missing.Missing || missing.Tampered {
		t.Fatalf("removed artifact = %+v", missing)
	}
}
