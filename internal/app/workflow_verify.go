/*
 * [INPUT]: 依赖 Core 脚本化执行、固定命令合同与内容寻址制品。
 * [OUTPUT]: 提供冻结脚本与验证 Run 的启动、识别和续跑。
 * [POS]: 应用桥接。沿用 Run 的审批、预算与独立验证。
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
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/core/agent"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
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
	first, err := boot.Journal.Append(ctx, 0, domain.EventDraft{
		RunID: runID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunCreated,
		Payload: contract.MustEncodeRedactedJSON(createdPayload),
	})
	if err != nil {
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
	return service.driveWorkflowScriptRun(ctx, boot, first, ResumeOptions{
		StateDir: execution.stateDir, Config: execution.configuration,
	}, nil)
}

const (
	workflowScriptPlanVersion   = workflowVerifyPlanVersion
	workflowScriptPlanKind      = "workflow-script"
	workflowScriptPlanMediaType = "application/vnd.cairn.workflow-script-plan+json"
	workflowScriptPlanMaxBytes  = 1 << 20
	workflowScriptMaxArguments  = 64
	workflowScriptMaxArgBytes   = 4 << 10
	workflowScriptExecTool      = "exec_command"
	workflowScriptCallID        = "workflow_script_1"
)

// workflowScriptPlan 固定一次工作流 Run 的执行方式、节点身份和实参。
type workflowScriptPlan struct {
	Version        int                          `json:"version"`
	Kind           string                       `json:"kind"`
	WorkflowID     string                       `json:"workflow_id"`
	StepID         string                       `json:"step_id"`
	Argv           []string                     `json:"argv,omitempty"`
	Acceptance     *contract.AcceptanceContract `json:"acceptance,omitempty"`
	SubjectVersion string                       `json:"subject_version,omitempty"`
}

// runWithWorkflowScript 通过普通运行装配创建脚本或验证 Run。
// 创建绑定先落盘。脚本随后由既有策略和审批链驱动。
func (service Service) runWithWorkflowScript(ctx context.Context, options RunOptions, bind func(*Boot, *domain.EventDraft) error, plan workflowScriptPlan) (RunResult, error) {
	if bind == nil {
		return RunResult{}, errors.New("workflow scripted run requires a creation binding")
	}
	if options.Config.Preset == config.PresetMinimal || options.ParentRunID != "" {
		return RunResult{}, errors.New("workflow scripted runs require a workflow-enabled root run")
	}
	if plan.Kind == workflowVerifyPlanKind && len(options.Config.Verification.Commands) == 0 {
		return RunResult{}, errors.New("workflow verification requires configured commands")
	}
	if plan.Kind == workflowScriptPlanKind && options.Config.Preset == config.PresetReadOnly {
		return RunResult{}, errors.New("workflow scripts are unavailable in the read-only preset")
	}
	plan.Argv = slices.Clone(plan.Argv)
	if plan.Acceptance != nil {
		acceptance := plan.Acceptance.Clone()
		plan.Acceptance, options.Acceptance = &acceptance, &acceptance
	}
	options.workflowScript = &plan
	return service.run(ctx, options, bind)
}

// calls 将冻结 argv 交给 exec_command 的共享参数合同。
// Verify 使用空调用表，仅驱动冻结验证命令。
func (plan workflowScriptPlan) calls() ([]ports.ToolCall, error) {
	if plan.Version != workflowScriptPlanVersion || strings.TrimSpace(plan.WorkflowID) == "" || strings.TrimSpace(plan.StepID) == "" {
		return nil, errors.New("invalid workflow scripted plan version or identity")
	}
	for _, id := range []string{plan.WorkflowID, plan.StepID} {
		if !utf8.ValidString(id) || string(contract.Redact([]byte(id))) != id {
			return nil, errors.New("invalid workflow scripted plan identity")
		}
	}
	switch plan.Kind {
	case workflowVerifyPlanKind:
		if len(plan.Argv) != 0 {
			return nil, errors.New("workflow verification plan cannot contain a scripted command")
		}
		if plan.Acceptance != nil {
			if err := plan.Acceptance.Validate(); err != nil {
				return nil, err
			}
			if plan.SubjectVersion == "" {
				return nil, errors.New("workflow acceptance requires an observed subject version")
			}
		} else if plan.SubjectVersion != "" {
			return nil, errors.New("workflow verification subject requires an acceptance contract")
		}
		return nil, nil
	case workflowScriptPlanKind:
		if plan.Acceptance != nil || plan.SubjectVersion != "" {
			return nil, errors.New("workflow script cannot carry an acceptance contract")
		}
		if len(plan.Argv) == 0 || len(plan.Argv) > workflowScriptMaxArguments || strings.TrimSpace(plan.Argv[0]) == "" {
			return nil, errors.New("workflow script requires a bounded argv")
		}
	default:
		return nil, errors.New("unknown workflow scripted plan kind")
	}
	for _, value := range plan.Argv {
		if len(value) > workflowScriptMaxArgBytes || !utf8.ValidString(value) || strings.ContainsFunc(value, func(r rune) bool {
			return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
		}) {
			return nil, errors.New("workflow script contains an invalid argument")
		}
		flag, _, _ := strings.Cut(value, "=")
		if string(contract.Redact([]byte(value))) != value || (strings.HasPrefix(flag, "-") && contract.SensitiveKey(strings.TrimLeft(flag, "-"))) {
			return nil, errors.New("workflow script contains a sensitive argument")
		}
	}
	raw, err := json.Marshal(struct {
		Argv []string `json:"argv"`
	}{Argv: plan.Argv})
	if err != nil {
		return nil, err
	}
	if _, err := contract.ParseExecArguments(raw); err != nil {
		return nil, err
	}
	return []ports.ToolCall{{ID: workflowScriptCallID, Name: workflowScriptExecTool, Arguments: raw}}, nil
}

// storeWorkflowScriptPlan 保存已核验的计划。事件只携带制品引用，制品缺失时
// 仍能从媒体类型识别脚本计划并拒绝模型回退。
func storeWorkflowScriptPlan(boot *Boot, plan workflowScriptPlan) (contract.Ref, error) {
	if plan.Kind == workflowScriptPlanKind && !slices.Contains(boot.Registry.Names(), workflowScriptExecTool) {
		return contract.Ref{}, errors.New("workflow script requires the exec_command tool")
	}
	if plan.SubjectVersion != "" {
		if err := requireWorkflowVerificationVersion(boot, plan.SubjectVersion); err != nil {
			return contract.Ref{}, err
		}
	}
	content, err := json.Marshal(plan)
	if err != nil {
		return contract.Ref{}, err
	}
	if len(content) > workflowScriptPlanMaxBytes {
		return contract.Ref{}, errors.New("workflow scripted plan exceeds its size limit")
	}
	ref, err := boot.Artifacts.Put(content, "application/json")
	if err != nil {
		return contract.Ref{}, fmt.Errorf("store workflow scripted plan: %w", err)
	}
	ref.MediaType = workflowScriptPlanMediaType
	ref.TruncatedPreview = ""
	return ref, nil
}

// driveWorkflowScriptRun 通过已持久化的创建时刻复用原预算、审批和对账规则。
func (service Service) driveWorkflowScriptRun(ctx context.Context, boot *Boot, first domain.Event, options ResumeOptions, calls []ports.ToolCall) (RunResult, error) {
	runID := first.RunID
	runContext, cancelRun, err := runBudgetContext(ctx, first.OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: runID}, err
	}
	defer cancelRun()
	input := agent.RunInput{RunID: runID, WorkspaceHash: boot.WorkspaceHash}
	result, err := boot.Loop.RunScriptedToolPlan(runContext, input, options.Approval, calls)
	if wallBudgetExpired(runContext) && !runReachedTerminal(boot.Journal, runID) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = boot.Loop.RunScriptedToolPlan(expiredContext, input, ports.Approval{}, calls)
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, runID, result.Response, err)
}

// workflowScriptCallsOfRun 解引用冻结计划。通用引用兼容旧 CLI 验证制品。
// 已绑定制品缺失或可识别计划格式错误时禁止模型续跑。
func workflowScriptCallsOfRun(first domain.Event, boot *Boot, checkSubject bool) ([]ports.ToolCall, bool, error) {
	var payload struct {
		TaskRef *contract.Ref `json:"task_ref"`
	}
	if err := json.Unmarshal(first.Payload, &payload); err != nil {
		return nil, false, fmt.Errorf("%w: decode run task reference: %w", ErrStateCorrupt, err)
	}
	if payload.TaskRef == nil {
		return nil, false, nil
	}
	tagged := payload.TaskRef.MediaType == workflowScriptPlanMediaType
	var content []byte
	var err error
	if tagged {
		content, err = boot.Artifacts.OpenBounded(*payload.TaskRef, workflowScriptPlanMaxBytes)
	} else {
		content, err = boot.Artifacts.Open(*payload.TaskRef)
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: read run task artifact: %w", ErrStateCorrupt, err)
	}
	var marker struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(content, &marker); err != nil && tagged {
		return nil, false, fmt.Errorf("%w: decode workflow scripted plan marker: %w", ErrStateCorrupt, err)
	}
	if !tagged && marker.Kind != workflowVerifyPlanKind {
		return nil, false, nil
	}
	var plan workflowScriptPlan
	if err := contract.DecodeToolArguments(content, &plan); err != nil {
		return nil, false, fmt.Errorf("%w: decode workflow scripted plan: %w", ErrStateCorrupt, err)
	}
	calls, err := plan.calls()
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrStateCorrupt, err)
	}
	if checkSubject && plan.SubjectVersion != "" {
		if err := requireWorkflowVerificationVersion(boot, plan.SubjectVersion); err != nil {
			return nil, false, err
		}
	}
	if plan.Acceptance != nil {
		boot.Loop.Acceptance = plan.Acceptance
	}
	return calls, true, nil
}
