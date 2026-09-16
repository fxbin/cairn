// rewind.go 提供工作区回退用例：从会话链各 run 的已生效补丁事件逆序推导反
// 放计划，在新 run 中以脚本化工具计划逐项走完整审批链执行。执行前会全量预
// 校验；执行阶段逐操作持久化，每个单文件操作原子，逻辑补丁及整批均非原子。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// rewindPlanKind 与 rewindPlanVersion 标识回退 run 创建事件 task_ref 指向的
// 计划制品：它只承载被反放的 run 清单，反放参数始终由事件重推导。
const rewindPlanKind = "workspace-rewind"

// rewindPlanVersion 是回退计划制品的当前版本。
const rewindPlanVersion = 1

// rewindPlanArtifact 是持久化于制品库的回退计划；runs 为链序目标 run 清单。
type rewindPlanArtifact struct {
	Version int            `json:"version"`
	Kind    string         `json:"kind"`
	Runs    []domain.RunID `json:"runs"`
}

// rewindPatch 记录链上一个确定生效的 apply_patch 事实：原模式由前后摘要推
// 断（before=absent 为 create，after=absent 为 delete，否则 update）。
type rewindPatch struct {
	RunID      domain.RunID
	Seq        uint64
	Path       string
	Mode       string
	BeforeHash string
	AfterHash  string
	BeforeRef  *artifact.Ref
}

// RewindOptions 描述一次工作区回退的输入：Runs 是要反放的链上运行（链序）；
// Approval 仅在续跑等待审批的既有回退 run 时生效。
type RewindOptions struct {
	Workspace string
	StateDir  string
	Config    config.Config
	Runs      []domain.RunID
	Approval  agent.Approval
	OnEvent   EventSink
}

// Rewind 反放选定链节点之后的所有已生效补丁：全新回退创建脚本化 run 执行；
// 同计划的未收口回退 run 可在审批中断后幂等续跑，悬挂副作用须先对账。每个
// 反放补丁独立走 提议 → 策略 → 必要时审批 → operation_started → 结果 事件链。
func (service Service) Rewind(ctx context.Context, options RewindOptions) (outcome RunResult, err error) {
	if len(options.Runs) == 0 {
		return RunResult{}, errors.New("rewind requires at least one run")
	}
	build, err := currentBuildIdentity()
	if err != nil {
		return RunResult{}, err
	}
	boot, err := service.assembleBoot(options.Workspace, options.StateDir, options.Config, options.OnEvent)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	active, err := boot.Journal.NonTerminalRuns(ctx, boot.WorkspaceHash)
	if err != nil {
		return RunResult{}, err
	}
	if len(active) != 0 {
		plan, isRewind, planErr := rewindPlanOfRun(ctx, boot.Journal, boot.Artifacts, active[0].ID)
		if planErr != nil {
			return RunResult{ID: active[0].ID}, planErr
		}
		phase := active[0].Phase()
		if isRewind && sameRunList(plan.Runs, options.Runs) && (phase == "running" || phase == "waiting_approval") {
			events, loadErr := boot.Journal.Load(ctx, active[0].ID)
			if loadErr != nil {
				return RunResult{ID: active[0].ID}, loadErr
			}
			if driftErr := requireMatchingConfig(events, options.Config); driftErr != nil {
				return RunResult{ID: active[0].ID}, driftErr
			}
			if referenceErr := checkRunArtifactReferences(events, options.StateDir, boot.WorkspaceHash); referenceErr != nil {
				return RunResult{ID: active[0].ID}, fmt.Errorf("%w: %v", ErrStateCorrupt, referenceErr)
			}
			if contractErr := checkFrozenRunContract(events, options.StateDir, boot.WorkspaceHash); contractErr != nil {
				return RunResult{ID: active[0].ID}, fmt.Errorf("%w: %v", ErrStateCorrupt, contractErr)
			}
			if admissionErr := enforceAdmissionGate(options.StateDir, boot.WorkspaceHash, options.Config); admissionErr != nil {
				return RunResult{ID: active[0].ID}, admissionErr
			}
			return service.driveRewindRun(ctx, boot, active[0].ID, plan.Runs, options)
		}
		return RunResult{}, activeRunError(active[0])
	}
	if err := enforceAdmissionGate(options.StateDir, boot.WorkspaceHash, options.Config); err != nil {
		return RunResult{}, err
	}
	calls, err := service.rewindToolCalls(ctx, boot, options.Runs, nil)
	if err != nil {
		return RunResult{}, err
	}
	configFingerprint := options.Config.Fingerprint()
	frozenConfig, err := marshalFrozenRunConfig(options.Config, configFingerprint)
	if err != nil {
		return RunResult{}, fmt.Errorf("build frozen run config: %w", err)
	}
	configRef, err := boot.Artifacts.Put(frozenConfig, "application/json")
	if err != nil {
		return RunResult{}, fmt.Errorf("store frozen run config: %w", err)
	}
	planRef, err := boot.Artifacts.Put(encode(rewindPlanArtifact{Version: rewindPlanVersion, Kind: rewindPlanKind, Runs: options.Runs}), "application/json")
	if err != nil {
		return RunResult{}, fmt.Errorf("store rewind plan artifact: %w", err)
	}
	task := fmt.Sprintf("工作区回退：反放 %d 个已生效补丁", len(calls))
	runID := domain.NewRunID()
	first, err := boot.Journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunCreated, Payload: encode(struct {
		domain.RunCreatedPayload
		ConfigRef artifact.Ref `json:"config_ref"`
		TaskRef   artifact.Ref `json:"task_ref"`
	}{RunCreatedPayload: domain.RunCreatedPayload{Task: task, Workspace: boot.Workspace.Root, ConfigFingerprint: configFingerprint, Build: build}, ConfigRef: configRef, TaskRef: planRef})})
	if err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(first)
	}
	second, err := boot.Journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunStarted, Payload: encode(struct {
		Invocation string `json:"invocation"`
	}{Invocation: domain.NewID("invoke")})})
	if err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(second)
	}
	return service.driveRewindRun(ctx, boot, runID, options.Runs, options)
}

// driveRewindRun 重推导反放调用并驱动脚本化执行器收束回退 run；墙钟预算
// 耗尽与普通运行一致地补写终态后重试一次。
func (service Service) driveRewindRun(ctx context.Context, boot *Boot, runID domain.RunID, runs []domain.RunID, options RewindOptions) (RunResult, error) {
	events, err := boot.Journal.Load(ctx, runID)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	calls, err := service.rewindToolCalls(ctx, boot, runs, proposedRewindCallIDs(events))
	if err != nil {
		return RunResult{ID: runID}, err
	}
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	defer cancelRun()
	input := agent.RunInput{RunID: runID, WorkspaceHash: boot.WorkspaceHash}
	result, err := boot.Loop.RunScriptedToolPlan(runContext, input, options.Approval, calls)
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = boot.Loop.RunScriptedToolPlan(expiredContext, input, agent.Approval{}, calls)
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, runID, result.Response, err)
}

// rewindToolCalls 由链上事件推导反放调用清单：done 是已有终态或对账结论、
// 无需再次提议的调用标识集合；not_applied 对账结论仍会重新提议。
func (service Service) rewindToolCalls(ctx context.Context, boot *Boot, runs []domain.RunID, done map[string]bool) ([]ports.ToolCall, error) {
	patches, err := collectRewindPatches(ctx, boot.Journal, runs)
	if err != nil {
		return nil, err
	}
	if len(patches) == 0 {
		return nil, errors.New("选定节点之后没有确定生效的补丁，工作区无需回退")
	}
	return deriveRewindCalls(patches, boot.Workspace, boot.Artifacts, done)
}

// collectRewindPatches 按链序加载各 run 事件并提取确定生效的补丁：
// operation_succeeded 或 operation_reconciled(applied_success)；未启动、未
// 生效或结果不确定的补丁跳过，其偏差由推导阶段的哈希校验兜底。
func collectRewindPatches(ctx context.Context, journal *sqlite.Journal, runs []domain.RunID) ([]rewindPatch, error) {
	patches := make([]rewindPatch, 0)
	for _, runID := range runs {
		events, err := journal.Load(ctx, runID)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			return nil, fmt.Errorf("run %s not found", runID)
		}
		projection, err := domain.Replay(events)
		if err != nil {
			return nil, fmt.Errorf("run %s: %w", runID, err)
		}
		if !projection.Terminal() {
			return nil, fmt.Errorf("run %s 尚未收口（%s），需先终态后再回退", runID, projection.Phase())
		}
		started := make(map[domain.OperationID]*rewindPatch)
		for _, event := range events {
			if event.OperationID == nil {
				continue
			}
			switch event.Type {
			case domain.EventOperationStarted:
				var payload struct {
					ToolName         string        `json:"tool_name"`
					SideEffectClass  string        `json:"side_effect_class"`
					PlannedPath      string        `json:"planned_path"`
					PlannedBefore    string        `json:"planned_before"`
					PlannedAfter     string        `json:"planned_after"`
					PlannedBeforeRef *artifact.Ref `json:"planned_before_ref"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return nil, fmt.Errorf("run %s event %d: decode operation_started: %w", runID, event.Seq, err)
				}
				if payload.ToolName != "apply_patch" || payload.SideEffectClass != "workspace" || payload.PlannedPath == "" {
					continue
				}
				mode := "update"
				if payload.PlannedBefore == "absent" {
					mode = "create"
				} else if payload.PlannedAfter == "absent" {
					mode = "delete"
				}
				started[*event.OperationID] = &rewindPatch{RunID: runID, Seq: event.Seq, Path: payload.PlannedPath, Mode: mode, BeforeHash: payload.PlannedBefore, AfterHash: payload.PlannedAfter, BeforeRef: payload.PlannedBeforeRef}
			case domain.EventOperationSucceeded:
				if patch, exists := started[*event.OperationID]; exists {
					patches = append(patches, *patch)
				}
			case domain.EventOperationReconciled:
				var payload struct {
					Verdict string `json:"verdict"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err == nil && payload.Verdict == "applied_success" {
					if patch, exists := started[*event.OperationID]; exists {
						patches = append(patches, *patch)
					}
				}
			}
		}
	}
	return patches, nil
}

// simulatedFile 是逆序模拟中的单文件状态；hash 为 absent 表示文件不存在。
type simulatedFile struct {
	content []byte
	hash    string
}

// deriveRewindCalls 逆序反放已生效补丁并全量预校验：从当前工作区状态出发，
// 逐项校验文件摘要与链上 planned_after 一致、before 制品在档可读。任一项
// 不可恢复即在计划生成阶段拒绝；该保证不表示后续逐项执行具有整批原子性。
func deriveRewindCalls(patches []rewindPatch, workspace tools.Workspace, artifacts *artifact.Store, done map[string]bool) ([]ports.ToolCall, error) {
	simulated := make(map[string]*simulatedFile)
	state := func(path string) (*simulatedFile, error) {
		if existing, exists := simulated[path]; exists {
			return existing, nil
		}
		absolute, _, err := workspace.Resolve(path)
		if err != nil {
			return nil, err
		}
		content, err := os.ReadFile(absolute)
		if errors.Is(err, os.ErrNotExist) {
			file := &simulatedFile{hash: "absent"}
			simulated[path] = file
			return file, nil
		}
		if err != nil {
			return nil, err
		}
		file := &simulatedFile{content: content, hash: contentReference(content)}
		simulated[path] = file
		return file, nil
	}
	calls := make([]ports.ToolCall, 0, len(patches))
	appendCall := func(callID string, arguments tools.PatchArguments) error {
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return err
		}
		calls = append(calls, ports.ToolCall{ID: callID, Name: "apply_patch", Arguments: encoded})
		return nil
	}
	for index := len(patches) - 1; index >= 0; index-- {
		patch := patches[index]
		restoreID := rewindCallID(patch.RunID, patch.Seq)
		if done[restoreID] {
			continue
		}
		file, err := state(patch.Path)
		if err != nil {
			return nil, fmt.Errorf("%s（run %s 事件 %d）: %w", patch.Path, patch.RunID, patch.Seq, err)
		}
		switch patch.Mode {
		case "create":
			// 原 create 反放为 delete：现文件必须仍是创建出的内容。
			if file.hash != patch.AfterHash {
				return nil, driftError(patch, file)
			}
			if err := appendCall(restoreID, tools.PatchArguments{Path: patch.Path, Mode: "delete", ExpectedSHA256: patch.AfterHash}); err != nil {
				return nil, err
			}
			file.content, file.hash = nil, "absent"
		case "delete":
			// 原 delete 反放为 create：现文件必须仍不存在。
			if file.hash != patch.AfterHash {
				return nil, driftError(patch, file)
			}
			before, err := restoreBefore(artifacts, patch)
			if err != nil {
				return nil, err
			}
			content := string(before)
			if err := appendCall(restoreID, tools.PatchArguments{Path: patch.Path, Mode: "create", Content: &content}); err != nil {
				return nil, err
			}
			file.content, file.hash = before, patch.BeforeHash
		default:
			// 原 update 反放为全文替换：old 是当前全文、new 是 before 制品
			// 内容；改写结果为空文件时无法用替换表达，退化为先删后建。
			before, err := restoreBefore(artifacts, patch)
			if err != nil {
				return nil, err
			}
			switch {
			case file.hash == patch.AfterHash && len(file.content) > 0:
				if err := appendCall(restoreID, tools.PatchArguments{Path: patch.Path, Mode: "update", ExpectedSHA256: patch.AfterHash, Replacements: []tools.Replacement{{Old: string(file.content), New: string(before)}}}); err != nil {
					return nil, err
				}
			case file.hash == patch.AfterHash:
				if err := appendCall(restoreID+"_clear", tools.PatchArguments{Path: patch.Path, Mode: "delete", ExpectedSHA256: patch.AfterHash}); err != nil {
					return nil, err
				}
				content := string(before)
				if err := appendCall(restoreID, tools.PatchArguments{Path: patch.Path, Mode: "create", Content: &content}); err != nil {
					return nil, err
				}
			case file.hash == "absent":
				// 仅出现在"先删后建"的删除步骤已执行、创建步骤被中断的续
				// 跑窗口：只补创建步骤。
				content := string(before)
				if err := appendCall(restoreID, tools.PatchArguments{Path: patch.Path, Mode: "create", Content: &content}); err != nil {
					return nil, err
				}
			default:
				return nil, driftError(patch, file)
			}
			file.content, file.hash = before, patch.BeforeHash
		}
	}
	return calls, nil
}

// restoreBefore 读回并校验 before 快照制品；缺失或不可读即如实拒绝。
func restoreBefore(artifacts *artifact.Store, patch rewindPatch) ([]byte, error) {
	if patch.BeforeRef == nil {
		return nil, fmt.Errorf("%s（run %s 事件 %d）缺少 before 快照制品，该次改写不可回退", patch.Path, patch.RunID, patch.Seq)
	}
	content, err := artifacts.Open(*patch.BeforeRef)
	if err != nil {
		return nil, fmt.Errorf("%s（run %s 事件 %d）before 快照制品不可读: %w", patch.Path, patch.RunID, patch.Seq, err)
	}
	return content, nil
}

// driftError 报告工作区状态与链上摘要不符，回退失败关闭。
func driftError(patch rewindPatch, file *simulatedFile) error {
	return fmt.Errorf("%s（run %s 事件 %d）工作区当前摘要 %s 与链上摘要 %s 不符，回退已拒绝", patch.Path, patch.RunID, patch.Seq, shortHash(file.hash), shortHash(patch.AfterHash))
}

// rewindCallID 由补丁来源 run 与事件序号构造确定性反放调用标识。
func rewindCallID(runID domain.RunID, seq uint64) string {
	return fmt.Sprintf("rewind_%s_%d", runID, seq)
}

// contentReference 计算内容的 sha256 引用形态，与链上摘要口径一致。
func contentReference(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// shortHash 截断摘要便于错误信息阅读。
func shortHash(value string) string {
	if len(value) > 19 {
		return value[:19] + "…"
	}
	return value
}

// rewindPlanOfRun 解码指定 run 的回退计划制品；found 为假表示普通 run。
// task_ref 是通用字段，只有制品明确声明 workspace-rewind 才按回退处理；已
// 识别的回退制品若版本或内容损坏则失败关闭。
func rewindPlanOfRun(ctx context.Context, journal *sqlite.Journal, artifacts *artifact.Store, runID domain.RunID) (rewindPlanArtifact, bool, error) {
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return rewindPlanArtifact{}, false, err
	}
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return rewindPlanArtifact{}, false, fmt.Errorf("run %s is missing run_created event", runID)
	}
	var payload struct {
		TaskRef *artifact.Ref `json:"task_ref"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		return rewindPlanArtifact{}, false, err
	}
	if payload.TaskRef == nil {
		return rewindPlanArtifact{}, false, nil
	}
	content, err := artifacts.Open(*payload.TaskRef)
	if err != nil {
		var previewMarker struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(payload.TaskRef.TruncatedPreview), &previewMarker) == nil && previewMarker.Kind == rewindPlanKind {
			return rewindPlanArtifact{}, false, fmt.Errorf("open rewind plan artifact: %w", err)
		}
		return rewindPlanArtifact{}, false, nil
	}
	var marker struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(content, &marker) != nil || marker.Kind != rewindPlanKind {
		return rewindPlanArtifact{}, false, nil
	}
	var plan rewindPlanArtifact
	if err := json.Unmarshal(content, &plan); err != nil {
		return rewindPlanArtifact{}, false, fmt.Errorf("decode rewind plan artifact: %w", err)
	}
	if plan.Version != rewindPlanVersion || len(plan.Runs) == 0 {
		return rewindPlanArtifact{}, false, fmt.Errorf("invalid rewind plan artifact version %d or empty run list", plan.Version)
	}
	return plan, true, nil
}

// proposedRewindCallIDs 提取回退 run 中无需再次执行的调用标识：普通终态与
// 对账确认已生效或已失败的操作均跳过；not_applied 明确表示副作用未发生，
// 必须保留该调用供脚本化执行器重新执行。等待审批的提议也不算完成。
func proposedRewindCallIDs(events []domain.Event) map[string]bool {
	terminal := make(map[domain.OperationID]bool)
	for _, event := range events {
		if event.OperationID == nil {
			continue
		}
		switch event.Type {
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled:
			terminal[*event.OperationID] = true
		case domain.EventOperationReconciled:
			var payload struct {
				Verdict string `json:"verdict"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Verdict != "not_applied" {
				terminal[*event.OperationID] = true
			}
		}
	}
	done := make(map[string]bool)
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed || event.OperationID == nil || !terminal[*event.OperationID] {
			continue
		}
		var payload struct {
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" {
			done[payload.CallID] = true
		}
	}
	return done
}

// sameRunList 判断两个 run 清单是否元素与顺序完全一致。
func sameRunList(left, right []domain.RunID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// activeRunError 把非终态 run 转换为与普通 Run 一致的失败信息，保留各自的
// 恢复指引。
func activeRunError(active domain.RunProjection) error {
	switch active.Phase() {
	case "waiting_approval":
		if pending := active.PendingApproval; pending != nil {
			return fmt.Errorf("%w: %w", ErrActiveRun, &agent.WaitingApprovalError{RunID: active.ID, OperationID: pending.OperationID, Fingerprint: pending.Fingerprint, Summary: pending.Summary})
		}
	case "waiting_reconcile":
		return fmt.Errorf("%w: %w", ErrActiveRun, ErrWaitingReconcile)
	case "created", "cancelling":
		return fmt.Errorf("%w: run %s is %s after an interrupted lifecycle; finish it with resume %s --abort-run --reason <text>", ErrActiveRun, active.ID, active.Phase(), active.ID)
	}
	return fmt.Errorf("%w: resume %s", ErrActiveRun, active.ID)
}
