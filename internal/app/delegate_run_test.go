package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// delegateThenFinalProvider 按调用序编排父委派与子/父收尾：第 1 次父提议
// delegate_task，第 2 次子收尾，其后父收尾。
type delegateThenFinalProvider struct{ calls int }

func (provider *delegateThenFinalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	switch provider.calls {
	case 1:
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "delegate-1", Name: "delegate_task", Arguments: json.RawMessage(`{"task":"child work"}`)}}); err != nil {
			return err
		}
	case 2:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "child done"}); err != nil {
			return err
		}
	default:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "parent done"}); err != nil {
			return err
		}
	}
	return emitTestCompletion(emit)
}

// delegatePauseProvider 编排审批暂停路径：父委派 → 子提议写入触发审批暂停 →
// 父继续收尾 → 子经恢复后收尾。
type delegatePauseProvider struct{ calls int }

func (provider *delegatePauseProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	switch provider.calls {
	case 1:
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "delegate-1", Name: "delegate_task", Arguments: json.RawMessage(`{"task":"patch child","max_turns":5}`)}}); err != nil {
			return err
		}
	case 2:
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"child.txt","mode":"create","content":"child"}`)}}); err != nil {
			return err
		}
	case 3:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "parent continues"}); err != nil {
			return err
		}
	default:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "child finished"}); err != nil {
			return err
		}
	}
	return emitTestCompletion(emit)
}

// delegateTestConfig 返回委派测试通用配置：exec 放行、含通过式验证命令。
func delegateTestConfig() config.Config {
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	return configuration
}

// onlyChildRun 返回父投影中的唯一子运行标识。
func onlyChildRun(t *testing.T, projection domain.RunProjection) string {
	t.Helper()
	if len(projection.Children) != 1 {
		t.Fatalf("expected exactly one delegated child, got %d", len(projection.Children))
	}
	for childID := range projection.Children {
		return childID
	}
	return ""
}

// TestDelegateTaskRunsChildRunToCompletion 端到端验证模型侧委派：父经
// delegate_task 受控启动子 run，子完成后父 Journal 同时含 delegated 与
// completed 事实，父自身正常收尾。
func TestDelegateTaskRunsChildRunToCompletion(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := delegateTestConfig()
	provider := &delegateThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	childID := onlyChildRun(t, inspected.Projection)
	record := inspected.Projection.Children[childID]
	if record.State != "completed" || record.Outcome != "ok" {
		t.Fatalf("child record = %+v", record)
	}
	delegated, completed := 0, 0
	for _, event := range inspected.Events {
		switch event.Type {
		case domain.EventSubagentDelegated:
			delegated++
		case domain.EventSubagentCompleted:
			completed++
		}
	}
	if delegated != 1 || completed != 1 {
		t.Fatalf("delegated=%d completed=%d", delegated, completed)
	}
}

// TestDelegateTaskSurfacesChildApprovalPauseAndResumeCoversNotification 验证
// 子审批暂停不是父错误：父继续收尾；子经恢复完成后，完成事实仍回写已终态
// 的父 Journal。
func TestDelegateTaskSurfacesChildApprovalPauseAndResumeCoversNotification(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := delegateTestConfig()
	provider := &delegatePauseProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	childID := onlyChildRun(t, inspected.Projection)
	if record := inspected.Projection.Children[childID]; record.State != "delegated" {
		t.Fatalf("paused child record = %+v", record)
	}
	// 子运行冻结的是裁剪后预算（max_turns=5），后续检查与恢复须用同构配置。
	childConfiguration := configuration
	childConfiguration.Budget.MaxTurns = 5
	childInspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, childConfiguration, domain.RunID(childID), true)
	if err != nil {
		t.Fatal(err)
	}
	pending := childInspected.Projection.PendingApproval
	if pending == nil {
		t.Fatalf("child is not waiting for approval: %+v", childInspected.Projection)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: domain.RunID(childID), Workspace: workspace, StateDir: stateDir, Config: childConfiguration, Approval: ports.Approval{Fingerprint: pending.Fingerprint, Approve: true}}); err != nil {
		t.Fatal(err)
	}
	assertLatestChildState(t, workspace, stateDir, configuration, result.ID, domain.RunID(childID), "completed", "ok")
}

// delegateThenMessageProvider 编排绑定恢复路径：父委派 → 子收尾 → 父向预置
// 目标 run 投递跟进消息 → 父收尾。
type delegateThenMessageProvider struct {
	calls  int
	target string
}

func (provider *delegateThenMessageProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	switch provider.calls {
	case 1:
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "delegate-1", Name: "delegate_task", Arguments: json.RawMessage(`{"task":"child work"}`)}}); err != nil {
			return err
		}
	case 2:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "child done"}); err != nil {
			return err
		}
	case 3:
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "message-1", Name: "send_message", Arguments: json.RawMessage(`{"to_run_id":"` + provider.target + `","content":"followup after delegation"}`)}}); err != nil {
			return err
		}
	default:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "parent done"}); err != nil {
			return err
		}
	}
	return emitTestCompletion(emit)
}

// TestDelegateTaskRestoresGlobalBindingsForParentMessaging 验证嵌套子 Run
// 返回后进程级投递绑定恢复为父 Boot：父在委派之后调用 send_message 仍能
// 把 steering 事实写入目标 run Journal，而不是落到已关闭的子 Journal。
func TestDelegateTaskRestoresGlobalBindingsForParentMessaging(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := delegateTestConfig()
	// 预置一个已终态的目标 run 作为投递对象。
	seed := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	seeded, err := seed.Run(context.Background(), RunOptions{Task: "seed", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	provider := &delegateThenMessageProvider{target: string(seeded.ID)}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	targetEvents, err := journal.Load(context.Background(), seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	delivered := false
	for _, event := range targetEvents {
		if event.Type == domain.EventSteeringInputSubmitted {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("send_message after delegation was not delivered to %s", seeded.ID)
	}
}

// TestTolerateFamilyActiveRunsAllowsAncestorsAndSiblingsOnly 锁定家族豁免
// 判定：父与同父兄弟放行，无关活跃运行拒绝。
func TestTolerateFamilyActiveRunsAllowsAncestorsAndSiblingsOnly(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	parent, err := service.Run(context.Background(), RunOptions{Task: "parent", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	sibling := domain.NewRunID()
	events, err := journal.Load(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSubagentDelegated(context.Background(), journal, parent.ID, hash, DelegateOptions{ParentRunID: parent.ID, ChildRunID: sibling, Task: "sibling"}, events[len(events)-1].OccurredAt.Add(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	family := []domain.RunProjection{{ID: parent.ID}, {ID: sibling}}
	if err := tolerateFamilyActiveRuns(context.Background(), journal, hash, family, parent.ID); err != nil {
		t.Fatalf("family runs must be tolerated: %v", err)
	}
	unrelated := []domain.RunProjection{{ID: domain.NewRunID()}}
	if err := tolerateFamilyActiveRuns(context.Background(), journal, hash, unrelated, parent.ID); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("unrelated active run must be rejected: %v", err)
	}
	// 深链祖先上溯：手工构造祖父与父的创建事件，豁免必须沿 parent_run_id 命中祖父。
	grandparent, midParent := domain.NewRunID(), domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: grandparent, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"gp","workspace":"/gp"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: midParent, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"mid","workspace":"/mid","parent_run_id":"` + string(grandparent) + `"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := tolerateFamilyActiveRuns(context.Background(), journal, hash, []domain.RunProjection{{ID: grandparent}}, midParent); err != nil {
		t.Fatalf("grandparent must be tolerated via parent_run_id walk: %v", err)
	}
}

// conflictOnceJournal 在前 N 次 Append 注入 CAS 序号冲突，模拟完成回写与
// 父 Loop 并发追加的竞争窗口。
type conflictOnceJournal struct {
	ports.Journal
	conflicts int
}

func (journal *conflictOnceJournal) Append(ctx context.Context, expectedSeq uint64, draft domain.EventDraft) (domain.Event, error) {
	if journal.conflicts > 0 {
		journal.conflicts--
		return domain.Event{}, sqlite.ErrConflict
	}
	return journal.Journal.Append(ctx, expectedSeq, draft)
}

// TestRecordSubagentCompletedRetriesOnceOnSequenceConflict 锁定有界重试：
// 单次 CAS 冲突后重载重试成功落事实；连续冲突仍失败关闭。
func TestRecordSubagentCompletedRetriesOnceOnSequenceConflict(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	parent, err := service.Run(context.Background(), RunOptions{Task: "parent", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	retryChild, stuckChild := domain.NewRunID(), domain.NewRunID()
	events, err := journal.Load(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := events[len(events)-1].OccurredAt
	for _, child := range []domain.RunID{retryChild, stuckChild} {
		if _, err := RecordSubagentDelegated(context.Background(), journal, parent.ID, hash, DelegateOptions{ParentRunID: parent.ID, ChildRunID: child, Task: "retry probe"}, at.Add(time.Nanosecond)); err != nil {
			t.Fatal(err)
		}
	}
	// 单次冲突：重载后重试成功，完成事实恰好一条。
	wrapped := &conflictOnceJournal{Journal: journal, conflicts: 1}
	if _, err := RecordSubagentCompleted(context.Background(), wrapped, parent.ID, hash, retryChild, "ok", nil, at.Add(2*time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	reloaded, err := journal.Load(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, event := range reloaded {
		if event.Type == domain.EventSubagentCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("completed facts = %d, want 1", completed)
	}
	// 连续冲突：重试一次后仍失败关闭，不静默丢事实也不死循环。
	exhausted := &conflictOnceJournal{Journal: journal, conflicts: 2}
	if _, err := RecordSubagentCompleted(context.Background(), exhausted, parent.ID, hash, stuckChild, "ok", nil, at.Add(3*time.Nanosecond)); !errors.Is(err, sqlite.ErrConflict) {
		t.Fatalf("persistent conflict must fail closed: %v", err)
	}
}

// concurrentWinnerJournal 模拟并发写者：首次追加先真实落盘（并发方胜出）
// 再返回冲突；重试重载后应走幂等分支返回既有事实而不双写。
type concurrentWinnerJournal struct {
	ports.Journal
	injected bool
}

func (journal *concurrentWinnerJournal) Append(ctx context.Context, expectedSeq uint64, draft domain.EventDraft) (domain.Event, error) {
	if !journal.injected {
		journal.injected = true
		if _, err := journal.Journal.Append(ctx, expectedSeq, draft); err != nil {
			return domain.Event{}, err
		}
		return domain.Event{}, sqlite.ErrConflict
	}
	return journal.Journal.Append(ctx, expectedSeq, draft)
}

// TestRecordSubagentCompletedRetrySeesConcurrentWinnerIdempotently 锁定重试
// 重载遇并发完成事实时幂等返回：不双写、不报错。
func TestRecordSubagentCompletedRetrySeesConcurrentWinnerIdempotently(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	parent, err := service.Run(context.Background(), RunOptions{Task: "parent", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	child := domain.NewRunID()
	events, err := journal.Load(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := events[len(events)-1].OccurredAt
	if _, err := RecordSubagentDelegated(context.Background(), journal, parent.ID, hash, DelegateOptions{ParentRunID: parent.ID, ChildRunID: child, Task: "winner probe"}, at.Add(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	wrapped := &concurrentWinnerJournal{Journal: journal}
	event, err := RecordSubagentCompleted(context.Background(), wrapped, parent.ID, hash, child, "ok", nil, at.Add(2*time.Nanosecond))
	if err != nil || event.Type != domain.EventSubagentCompleted {
		t.Fatalf("idempotent retry result = %+v err=%v", event, err)
	}
	reloaded, err := journal.Load(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, item := range reloaded {
		if item.Type == domain.EventSubagentCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("completed facts = %d, want exactly 1", completed)
	}
}

// TestChildBootExcludesDelegateToolSurface 锁定子运行工具面：根 Boot 注册
// delegate_task；子 Boot（家族共享锁路径）不注册，且在根持锁时仍可装配。
func TestChildBootExcludesDelegateToolSurface(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	root, err := service.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	child, err := service.assembleBootWithLock(workspace, stateDir, configuration, nil, true)
	if err != nil {
		t.Fatalf("child boot under family lock failed: %v", err)
	}
	defer child.Close()
	hasDelegate := func(names []string) bool {
		for _, name := range names {
			if name == "delegate_task" {
				return true
			}
		}
		return false
	}
	if !hasDelegate(root.Registry.Names()) {
		t.Fatalf("root surface must include delegate_task: %v", root.Registry.Names())
	}
	if hasDelegate(child.Registry.Names()) {
		t.Fatalf("child surface must exclude delegate_task: %v", child.Registry.Names())
	}
}

// TestDelegateTaskWorksWhenSkillsEnabled 锁定 R3 修复：技能启用时父工具面
// 含 skill 工具，显式传递后子收窄不再被基础面重建误拒，委派照常完成。
func TestDelegateTaskWorksWhenSkillsEnabled(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := delegateTestConfig()
	configuration.Skills = &config.SkillsConfig{Enabled: true}
	provider := &delegateThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	childID := onlyChildRun(t, inspected.Projection)
	if record := inspected.Projection.Children[childID]; record.State != "completed" || record.Outcome != "ok" {
		t.Fatalf("skills-enabled delegation child record = %+v", record)
	}
}

// TestDelegateTaskClampsChildTurnsToParentRemaining 锁定子额度钳制：父剩余
// 轮数为零时失败关闭不产生子运行（父随后按既有预算语义收束）；剩余一轮时
// 子额度被钳到剩余值并照常完成。
func TestDelegateTaskClampsChildTurnsToParentRemaining(t *testing.T) {
	for _, setting := range []struct {
		parentMaxTurns int
		expectChild    bool
	}{
		{parentMaxTurns: 1, expectChild: false},
		{parentMaxTurns: 2, expectChild: true},
	} {
		workspace, stateDir := t.TempDir(), t.TempDir()
		configuration := delegateTestConfig()
		configuration.Budget.MaxTurns = setting.parentMaxTurns
		provider := &delegateThenFinalProvider{}
		service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
		result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
		if setting.expectChild {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, ports.ErrTurnBudgetExceeded) {
			t.Fatalf("exhausted-parent err = %v, want turn budget stop", err)
		}
		inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if setting.expectChild && inspected.Projection.State != domain.RunCompleted {
			t.Fatalf("parent state = %s", inspected.Projection.State)
		}
		children := len(inspected.Projection.Children)
		if (setting.expectChild && children != 1) || (!setting.expectChild && children != 0) {
			t.Fatalf("max_turns=%d children = %d, want child=%t", setting.parentMaxTurns, children, setting.expectChild)
		}
	}
}
