/*
 * [INPUT]: 依赖Core 脚本化执行、Journal 与冻结制品。
 * [OUTPUT]: 提供工作流验证 Run 的启动、识别和续跑。
 * [POS]: 应用桥接；沿用 Run 审批、预算和验证可信语义。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 */
// Package app 连接工作流能力与既有运行入口。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/core/agent"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/artifact"
)

// startWorkflowVerifyRun 装配 Boot、手工创建验证步骤 run（task_ref 绑定验
// 证计划制品）并以零调用脚本化计划强制执行冻结验证；Boot 生命周期由本函数
// 持有，等待审批时如实返回供步骤保持运行中。
func (service Service) startWorkflowVerifyRun(ctx context.Context, execution workflowExecution, runID domain.RunID, id workflow.ID, stepID string) (outcome RunResult, err error) {
	boot, err := service.assembleBoot(execution.workspacePath, execution.stateDir, execution.configuration, execution.onEvent)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	active, err := boot.Journal.NonTerminalRuns(ctx, boot.WorkspaceHash)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	if len(active) != 0 {
		return RunResult{ID: runID}, activeRunError(active[0])
	}
	if err := enforceAdmissionGate(execution.stateDir, boot.WorkspaceHash, execution.configuration); err != nil {
		return RunResult{ID: runID}, err
	}
	build, err := currentBuildIdentity()
	if err != nil {
		return RunResult{ID: runID}, err
	}
	frozenConfig, err := marshalFrozenRunConfig(execution.configuration, execution.configuration.Fingerprint())
	if err != nil {
		return RunResult{ID: runID}, err
	}
	configRef, err := boot.Artifacts.Put(frozenConfig, "application/json")
	if err != nil {
		return RunResult{ID: runID}, err
	}
	planRef, err := boot.Artifacts.Put(contract.MustEncodeRedactedJSON(workflowVerifyPlanArtifact{
		Version: workflowVerifyPlanVersion, Kind: workflowVerifyPlanKind, WorkflowID: string(id), StepID: stepID,
	}), "application/json")
	if err != nil {
		return RunResult{ID: runID}, err
	}
	task := fmt.Sprintf("工作流验证门：%s 步骤 %s", id, stepID)
	createdPayload := struct {
		domain.RunCreatedPayload
		ConfigRef contract.Ref `json:"config_ref"`
		TaskRef   contract.Ref `json:"task_ref"`
	}{RunCreatedPayload: domain.RunCreatedPayload{
		Task: task, Workspace: boot.Workspace.Root, ConfigFingerprint: execution.configuration.Fingerprint(), Build: build,
	}, ConfigRef: configRef, TaskRef: planRef}
	if _, err := boot.Journal.Append(ctx, 0, domain.EventDraft{
		RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunCreated,
		Payload: contract.MustEncodeRedactedJSON(createdPayload),
	}); err != nil {
		return RunResult{ID: runID}, err
	}
	if _, err := boot.Journal.Append(ctx, 1, domain.EventDraft{
		RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunStarted,
		Payload: contract.MustEncodeRedactedJSON(struct {
			Invocation string `json:"invocation"`
		}{Invocation: domain.NewID("invoke")}),
	}); err != nil {
		return RunResult{ID: runID}, err
	}
	return service.driveWorkflowVerifyRun(ctx, boot, runID, ResumeOptions{
		Workspace: execution.workspacePath, StateDir: execution.stateDir,
		Config: execution.configuration, OnEvent: execution.onEvent,
	})
}

// driveWorkflowVerifyRun 以零调用脚本化计划驱动既有验证 run 收束冻结验证；
// 它不触碰模型，审批与对账暂停原样上抛，由 run 级 Resume/Reconcile 续跑。
func (service Service) driveWorkflowVerifyRun(ctx context.Context, boot *Boot, runID domain.RunID, options ResumeOptions) (RunResult, error) {
	events, err := boot.Journal.Load(ctx, runID)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	if len(events) == 0 {
		return RunResult{ID: runID}, fmt.Errorf("workflow verify run %s not found", runID)
	}
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	defer cancelRun()
	input := agent.RunInput{RunID: runID, WorkspaceHash: boot.WorkspaceHash}
	result, err := boot.Loop.RunScriptedToolPlan(runContext, input, options.Approval, nil)
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = boot.Loop.RunScriptedToolPlan(expiredContext, input, ports.Approval{}, nil)
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, runID, result.Response, err)
}

// workflowVerifyRunOfRun 判定 run 是否为工作流验证步骤的脚本化 run：以
// task_ref 制品的 kind 标记识别，已识别制品损坏时失败关闭。
func workflowVerifyRunOfRun(ctx context.Context, journal *sqlite.Journal, artifacts *artifact.Store, runID domain.RunID) (bool, error) {
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return false, err
	}
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return false, fmt.Errorf("run %s is missing run_created event", runID)
	}
	var payload struct {
		TaskRef *contract.Ref `json:"task_ref"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		return false, err
	}
	if payload.TaskRef == nil {
		return false, nil
	}
	content, err := artifacts.Open(*payload.TaskRef)
	if err != nil {
		var previewMarker struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(payload.TaskRef.TruncatedPreview), &previewMarker) == nil && previewMarker.Kind == workflowVerifyPlanKind {
			return false, fmt.Errorf("open workflow verify plan artifact: %w", err)
		}
		return false, nil
	}
	var marker struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(content, &marker) != nil || marker.Kind != workflowVerifyPlanKind {
		return false, nil
	}
	return true, nil
}
