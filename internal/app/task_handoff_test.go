// 本文件回归任务交接摘要的自动注入：显式开启后新会话的手工分配与返工 Worker 收到
// 交接摘要，默认关闭时行为与此前逐字相同，上下文不可信时拒绝启动。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// taskHandoffRecordingProvider 只回报文本并记录每次请求的消息，供断言注入内容。
type taskHandoffRecordingProvider struct {
	requests [][]ports.Message
}

// Stream 记录请求并给出最小完成流。
func (provider *taskHandoffRecordingProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.requests = append(provider.requests, request.Messages)
	return emitTestCompletion(emit)
}

// taskHandoffReworkProvider 在既有返工夹具的外面记录每次请求的消息。
type taskHandoffReworkProvider struct {
	taskReworkProvider
	requests [][]ports.Message
}

// Stream 先记录请求，再按返工夹具的脚本回应。
func (provider *taskHandoffReworkProvider) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.requests = append(provider.requests, request.Messages)
	return provider.taskReworkProvider.Stream(ctx, request, emit)
}

// taskHandoffUserText 拼接一次请求里全部 user 消息的正文。
func taskHandoffUserText(messages []ports.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		if message.Role == "user" {
			builder.WriteString(message.Content)
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

// taskHandoffRequestsBy 找出 user 正文包含指定标记的请求下标。
func taskHandoffRequestsBy(requests [][]ports.Message, marker string) []int {
	matched := make([]int, 0)
	for index, messages := range requests {
		if strings.Contains(taskHandoffUserText(messages), marker) {
			matched = append(matched, index)
		}
	}
	return matched
}

// taskHandoffPriorContext 读取某运行的创建事件引用的前情制品正文；没有引用时返回空串。
func taskHandoffPriorContext(t *testing.T, scope TaskScope, runID domain.RunID) string {
	t.Helper()
	_, hash, err := scope.resolve()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openReadOnlyJournalContext(context.Background(), scope.StateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	events, err := journal.Load(context.Background(), runID)
	if err != nil || len(events) == 0 {
		t.Fatalf("load run %s: %v", runID, err)
	}
	var created domain.RunCreatedPayload
	if err := json.Unmarshal(events[0].Payload, &created); err != nil {
		t.Fatal(err)
	}
	if len(created.PriorContextRef) == 0 {
		return ""
	}
	var reference contract.Ref
	if err := json.Unmarshal(created.PriorContextRef, &reference); err != nil {
		t.Fatal(err)
	}
	content, err := readArtifactContent(manifestArtifactRoot(scope.StateDir, hash), reference)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// TestTaskHandoffPromptRefusesBlockedContextAndPassesUnknownThrough 锁定生成交接文本
// 的两条规则：任一字段 blocked 就拒绝并只给原因码；unknown 照常注入，标题与免责说明成对。
func TestTaskHandoffPromptRefusesBlockedContextAndPassesUnknownThrough(t *testing.T) {
	input := contract.TaskContextInput{
		SchemaVersion: contract.TaskContextSchemaVersion, TaskID: "task-1", Goal: "让翻倍正确", State: "active", Revision: 1,
		AcceptanceFailure: &contract.TaskSourceFailure{Status: contract.TaskFactUnknown, Reason: contract.TaskReasonNoStructuredAcceptance},
	}
	unknown, err := contract.ComposeTaskContext(input)
	if err != nil {
		t.Fatal(err)
	}
	text, err := taskHandoffPrompt(unknown)
	if err != nil {
		t.Fatalf("unknown fields must not block the handoff: %v", err)
	}
	if !strings.HasPrefix(text, contract.TaskHandoffHeading) || !strings.HasSuffix(text, contract.TaskHandoffDisclaimer+"\n") {
		t.Fatalf("handoff must pair its heading with the disclaimer:\n%s", text)
	}
	if !strings.Contains(text, "acceptance unknown - no_structured_acceptance") {
		t.Fatalf("unknown facts must stay explicit:\n%s", text)
	}

	input.RunsFailure = &contract.TaskSourceFailure{Status: contract.TaskFactBlocked, Reason: contract.TaskReasonJournalChainInvalid}
	blocked, err := contract.ComposeTaskContext(input)
	if err != nil {
		t.Fatal(err)
	}
	text, err = taskHandoffPrompt(blocked)
	if !errors.Is(err, ErrTaskHandoffUntrusted) || text != "" {
		t.Fatalf("blocked context = %q, %v; want a refusal", text, err)
	}
	if !strings.Contains(err.Error(), string(contract.TaskReasonJournalChainInvalid)) || strings.Contains(err.Error(), "/") {
		t.Fatalf("refusal must carry the reason code and no path: %v", err)
	}
}

// TestRunTaskHandsOffTaskContextToAssignmentInNewSession 锁定手工分配路径：显式开启
// assignment 后，新会话里的第二次分配收到交接摘要，创建事件引用的前情制品与模型看到的
// 正文一致；默认关闭、任务没有历史、调用方自带前驱运行这三种情形都不注入。
func TestRunTaskHandsOffTaskContextToAssignmentInNewSession(t *testing.T) {
	cases := []struct {
		name        string
		handoff     *config.TaskHandoffConfig
		second      TaskRunOptions
		wantHandoff bool
	}{
		{"enabled for assignments", &config.TaskHandoffConfig{Mode: config.TaskHandoffAssignment},
			TaskRunOptions{AssignmentID: "second-assignment", SessionID: "fresh-session"}, true},
		{"enabled as all", &config.TaskHandoffConfig{Mode: config.TaskHandoffAll},
			TaskRunOptions{AssignmentID: "second-assignment", SessionID: "fresh-session"}, true},
		{"disabled by default", nil,
			TaskRunOptions{AssignmentID: "second-assignment", SessionID: "fresh-session"}, false},
		{"enabled only for rework", &config.TaskHandoffConfig{Mode: config.TaskHandoffRework},
			TaskRunOptions{AssignmentID: "second-assignment", SessionID: "fresh-session"}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			configuration := workflowExecConfig(nil)
			configuration.Verification.Commands = []config.VerificationCommand{{ID: "check", Argv: []string{"/usr/bin/true"}}}
			configuration.Context.TaskHandoff = testCase.handoff
			scope := TaskScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: configuration}
			provider := &taskHandoffRecordingProvider{}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
			current, err := service.CreateTask(ctx, scope, TaskDefinition{
				Goal:     "创建一个带有已知标记的文件",
				Criteria: []contract.AcceptanceCriterion{{ID: "build", Statement: "验证已创建的文件", Required: true, Checks: []string{"check"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			first, err := service.RunTask(ctx, scope, current.ID, TaskRunOptions{Revision: current.Revision, AssignmentID: "first-assignment", SessionID: "first-session"})
			if err != nil || len(first.Assignments) != 1 || !first.Assignments[0].settled() {
				t.Fatalf("first assignment failed: %v %+v", err, first.Assignments)
			}
			// 全新任务没有可交接的东西：第一次分配在任何模式下都不带交接摘要。
			if got := taskHandoffRequestsBy(provider.requests, contract.TaskHandoffHeading); len(got) != 0 {
				t.Fatalf("the first assignment of a new task must not receive a handoff: %v", got)
			}
			if got := taskHandoffPriorContext(t, scope, first.Assignments[0].RunID); got != "" {
				t.Fatalf("the first assignment must not carry prior context: %q", got)
			}

			testCase.second.Revision = first.Revision
			second, err := service.RunTask(ctx, scope, current.ID, testCase.second)
			if err != nil || len(second.Assignments) != 2 || !second.Assignments[1].settled() {
				t.Fatalf("second assignment failed: %v %+v", err, second.Assignments)
			}
			requests := provider.requests
			// 每次运行只有一次模型请求：前一次属于第一次分配，后一次属于第二次分配。
			if len(requests) != 2 {
				t.Fatalf("expected one request per assignment, got %d", len(requests))
			}
			stored := taskHandoffPriorContext(t, scope, second.Assignments[1].RunID)
			secondText := taskHandoffUserText(requests[1])
			if !testCase.wantHandoff {
				if strings.Contains(secondText, contract.TaskHandoffHeading) || strings.HasPrefix(stored, contract.TaskHandoffHeading) {
					t.Fatalf("handoff must not be injected in this configuration:\n%s", secondText)
				}
				if testCase.second.PreviousRunID == "" && stored != "" {
					t.Fatalf("a new session without handoff must not carry prior context: %q", stored)
				}
				return
			}
			for _, required := range []string{contract.TaskHandoffHeading, "run " + string(first.Assignments[0].RunID) + " role=assignment", contract.TaskHandoffDisclaimer} {
				if !strings.Contains(secondText, required) || !strings.Contains(stored, required) {
					t.Fatalf("second assignment must receive %q in the model input and the stored prior context:\n%s", required, secondText)
				}
			}
			// 模型看到的是一条独立的 user 消息：会话前情的固定标题在前，之后正是存下来的交接文本。
			seen := false
			for _, message := range requests[1] {
				if message.Role == "user" && strings.HasSuffix(message.Content, stored) {
					seen = true
				}
			}
			if !seen {
				t.Fatalf("the model must see exactly the stored handoff text in its own message:\n%s", secondText)
			}
		})
	}

	t.Run("caller provides a previous run", func(t *testing.T) {
		ctx := context.Background()
		configuration := workflowExecConfig(nil)
		configuration.Verification.Commands = []config.VerificationCommand{{ID: "check", Argv: []string{"/usr/bin/true"}}}
		configuration.Context.TaskHandoff = &config.TaskHandoffConfig{Mode: config.TaskHandoffAll}
		scope := TaskScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: configuration}
		provider := &taskHandoffRecordingProvider{}
		service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
		current, err := service.CreateTask(ctx, scope, TaskDefinition{Goal: "创建一个带有已知标记的文件", Acceptance: []string{"the file exists"}})
		if err != nil {
			t.Fatal(err)
		}
		first, err := service.RunTask(ctx, scope, current.ID, TaskRunOptions{Revision: current.Revision, AssignmentID: "first-assignment", SessionID: "first-session"})
		if err != nil || len(first.Assignments) != 1 {
			t.Fatalf("first assignment failed: %v", err)
		}
		second, err := service.RunTask(ctx, scope, current.ID, TaskRunOptions{
			Revision: first.Revision, AssignmentID: "second-assignment", SessionID: "first-session", PreviousRunID: first.Assignments[0].RunID,
		})
		if err != nil || len(second.Assignments) != 2 {
			t.Fatalf("second assignment failed: %v", err)
		}
		// 调用方明确选择了会话续链：沿用自动生成的前情卡，不重复注入交接摘要。
		text := taskHandoffUserText(provider.requests[len(provider.requests)-1])
		stored := taskHandoffPriorContext(t, scope, second.Assignments[1].RunID)
		if strings.Contains(text, contract.TaskHandoffHeading) || !strings.HasPrefix(stored, "任务: ") {
			t.Fatalf("a caller-provided previous run must keep the session card:\n%s", text)
		}
	})
}

// TestTaskWorkflowReworkWorkerReceivesTaskHandoff 锁定返工路径：显式开启 rework 后，
// 返工 Worker 收到交接摘要与“前次固定程序检查”一段，不再收到带最终回复的前情卡，
// 前驱运行与会话链不变；默认关闭时行为与此前逐字相同。
func TestTaskWorkflowReworkWorkerReceivesTaskHandoff(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		handoff *config.TaskHandoffConfig
	}{
		{"handoff enabled", &config.TaskHandoffConfig{Mode: config.TaskHandoffRework}},
		{"default off", nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			configuration := workflowExecConfig(nil)
			configuration.Verification.Commands = []config.VerificationCommand{{ID: "check", Argv: []string{"/usr/bin/cmp", "-s", "created.txt", "expected.txt"}}}
			configuration.Context.TaskHandoff = testCase.handoff
			scope := TaskScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: configuration}
			if err := os.WriteFile(filepath.Join(scope.Workspace, "expected.txt"), []byte("expected"), 0600); err != nil {
				t.Fatal(err)
			}
			worker := &taskHandoffReworkProvider{taskReworkProvider: taskReworkProvider{repair: true}}
			reviewer := &workflowReviewFixture{output: `{"findings":[]}`}
			service := Service{NewProvider: func(cfg config.Config) (ports.Provider, error) {
				if cfg.Tools.VerificationFilesOnly {
					return reviewer, nil
				}
				return worker, nil
			}}
			current, err := service.CreateTask(ctx, scope, TaskDefinition{
				Goal: "创建与参照文件一致的产物", Criteria: []contract.AcceptanceCriterion{
					{ID: "content", Statement: "内容与参照文件相同", Required: true, Checks: []string{"check"}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.StartTaskWorkflow(ctx, scope, current.ID, TaskWorkflowOptions{Revision: current.Revision, ReworkAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			instance := view.Workflow
			if instance == nil || !instance.Finished || instance.Verification == nil || !worker.rework ||
				instance.Verification.Evaluation.Verdict != contract.VerdictPassed {
				t.Fatalf("rework did not repair within budget: %+v", instance)
			}
			steps := instance.Steps
			if len(steps[0].Attempts) != 2 || len(steps[1].Attempts) != 2 || len(steps[2].Attempts) != 1 {
				t.Fatalf("unexpected attempt history: %+v", steps)
			}
			reworkRequests := taskHandoffRequestsBy(worker.requests, "前次固定程序检查。")
			if len(reworkRequests) == 0 {
				t.Fatal("the rework worker made no request")
			}
			handoffRequests := taskHandoffRequestsBy(worker.requests, contract.TaskHandoffHeading)
			reworkText := taskHandoffUserText(worker.requests[reworkRequests[0]])
			// 返工 Worker 仍链在程序检查运行之后：交接只替换前情，不改动会话链。
			if created := taskHandoffRunCreated(t, scope, domain.RunID(steps[0].Attempts[1].RunID)); created.PrevRunID == "" {
				t.Fatal("the rework worker must keep its previous run in the session chain")
			}
			if testCase.handoff == nil {
				if len(handoffRequests) != 0 || !strings.Contains(reworkText, "最终回复:") {
					t.Fatalf("default off must keep the generated session card and inject no handoff:\n%s", reworkText)
				}
				return
			}
			if len(handoffRequests) != len(reworkRequests) {
				t.Fatalf("handoff requests = %v, rework requests = %v", handoffRequests, reworkRequests)
			}
			if strings.Contains(reworkText, "最终回复:") {
				t.Fatalf("the rework worker must no longer receive the previous run's final response:\n%s", reworkText)
			}
			for _, required := range []string{contract.TaskHandoffHeading, "role=worker", "verdict=failed", contract.TaskHandoffDisclaimer} {
				if !strings.Contains(reworkText, required) {
					t.Fatalf("rework request missing %q:\n%s", required, reworkText)
				}
			}
			for index := range worker.requests {
				_, isRework := slices.BinarySearch(reworkRequests, index)
				_, hasHandoff := slices.BinarySearch(handoffRequests, index)
				// 交接摘要只出现在返工 Worker 的请求里；返工 Worker 的每一次请求都带它。
				if isRework != hasHandoff {
					t.Fatalf("request %d: rework=%t handoff=%t", index, isRework, hasHandoff)
				}
			}
			for _, run := range []domain.RunID{domain.RunID(steps[0].Attempts[0].RunID), domain.RunID(steps[1].Attempts[0].RunID)} {
				if stored := taskHandoffPriorContext(t, scope, run); strings.HasPrefix(stored, contract.TaskHandoffHeading) {
					t.Fatalf("run %s is not a rework worker and must not carry a handoff", run)
				}
			}
		})
	}
}

// taskHandoffRunCreated 读取某运行的创建事件载荷。
func taskHandoffRunCreated(t *testing.T, scope TaskScope, runID domain.RunID) domain.RunCreatedPayload {
	t.Helper()
	_, hash, err := scope.resolve()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openReadOnlyJournalContext(context.Background(), scope.StateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	events, err := journal.Load(context.Background(), runID)
	if err != nil || len(events) == 0 {
		t.Fatalf("load run %s: %v", runID, err)
	}
	var created domain.RunCreatedPayload
	if err := json.Unmarshal(events[0].Payload, &created); err != nil {
		t.Fatal(err)
	}
	return created
}
