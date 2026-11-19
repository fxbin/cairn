// [INPUT]: 依赖 App 模板修订门面、旧偏好快照与桌面配置装载。
// [OUTPUT]: 提供模板保存、明确迁移、预演和启动绑定。
// [POS]: 桌面适配层把模板草案交给 App。历史偏好只供读取。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件投影宿主保存的工作流模板。
// Author: fxbin
package desktop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fxbin/cairn/internal/app"
)

// WorkflowStep 传输节点文案、静态参数、前序引用和返工策略。
type WorkflowStep struct {
	ID          string   `json:"id,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	Task        string   `json:"task"`
	Argv        []string `json:"argv,omitempty"`
	Model       string   `json:"model,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	MaxAttempts int      `json:"max_attempts,omitempty"`
	Inputs      []string `json:"inputs,omitempty"`
	RetryFrom   string   `json:"retry_from,omitempty"`
}

// WorkflowDefinition 提供兼容历史编辑器的模板传输格式。
type WorkflowDefinition struct {
	ID        string         `json:"id"`
	Workspace string         `json:"workspace"`
	StateDir  string         `json:"state_dir,omitempty"`
	Name      string         `json:"name"`
	Revision  uint64         `json:"revision,omitempty"`
	Legacy    bool           `json:"legacy,omitempty"`
	Steps     []WorkflowStep `json:"steps"`
	MaxSteps  int            `json:"max_steps,omitempty"`
}

// WorkflowSaveAck 返回已核验且保存的具体修订。
type WorkflowSaveAck struct {
	OK         bool               `json:"ok"`
	Definition WorkflowDefinition `json:"definition"`
}

// WorkflowStartRequest 选中精确版本。修订零仅表示明确选中的历史偏好模板。
type WorkflowStartRequest struct {
	Workspace    string `json:"workspace"`
	StateDir     string `json:"state_dir,omitempty"`
	DefinitionID string `json:"definition_id"`
	Revision     uint64 `json:"revision"`
	DryRun       bool   `json:"dry_run,omitempty"`
}

// ListWorkflows 组合 App 已存修订与尚未显式导入的历史模板。
func (api *API) ListWorkflows(ctx context.Context, workspace string) ([]WorkflowDefinition, error) {
	scope, origin, err := desktopWorkflowScope(workspace, "")
	if err != nil {
		return nil, err
	}
	stored, err := api.service.ListWorkflowDefinitions(ctx, scope)
	if err != nil {
		return nil, err
	}
	items := make([]WorkflowDefinition, 0, len(stored))
	seen := make(map[string]bool, len(stored))
	for _, definition := range stored {
		item := workflowDefinitionView(origin, definition)
		items = append(items, item)
		seen[item.ID] = true
	}
	for _, legacy := range api.prefs.ListWorkflows(scope.Workspace) {
		if !seen[legacy.ID] {
			legacy.Legacy, legacy.Revision = true, 0
			legacy.Workspace = origin
			items = append(items, legacy)
			seen[legacy.ID] = true
		}
	}
	return items, nil
}

// SaveWorkflow 显式保存模板并返回宿主核验的修订。草案本身不得触发执行。
func (api *API) SaveWorkflow(ctx context.Context, definition WorkflowDefinition) (WorkflowSaveAck, error) {
	scope, origin, err := desktopWorkflowScope(definition.Workspace, definition.StateDir)
	if err != nil {
		return WorkflowSaveAck{}, err
	}
	draft, err := workflowDraft(definition)
	if err != nil {
		return WorkflowSaveAck{}, err
	}
	if err := api.service.CheckWorkflowDraft(ctx, scope, draft); err != nil {
		return WorkflowSaveAck{}, err
	}
	saved, err := api.service.SaveWorkflowDefinition(ctx, scope, draft, definition.Revision)
	if err != nil {
		return WorkflowSaveAck{}, err
	}
	return WorkflowSaveAck{OK: true, Definition: workflowDefinitionView(origin, saved)}, nil
}

// StartWorkflowInstance 预演指定版本，或保存实例后由 App 异步推进。
func (api *API) StartWorkflowInstance(ctx context.Context, request WorkflowStartRequest) (WorkflowRunAck, error) {
	return api.controller.startWorkflow(ctx, request, false)
}

// ListWorkflowInstances 只投影本工作区中已经持久化的工作流实例。
func (api *API) ListWorkflowInstances(ctx context.Context, request WorkspaceRequest) ([]app.WorkflowStatusView, error) {
	scope, _, err := desktopWorkflowScope(request.Workspace, request.StateDir)
	if err != nil {
		return nil, err
	}
	return api.service.WorkflowHistory(ctx, scope)
}

// RunWorkflow 兼容旧绑定，启动和预演均调用统一门面。
func (api *API) RunWorkflow(ctx context.Context, workspace, workflowID string, dryRun bool) (WorkflowRunAck, error) {
	return api.controller.RunWorkflow(ctx, workspace, workflowID, dryRun)
}

// desktopWorkflowScope 规范化实际授权路径，并保留原选择路径用于界面事件路由。
func desktopWorkflowScope(workspace, stateDir string) (app.WorkflowScope, string, error) {
	origin, err := absWorkspace(workspace)
	if err != nil {
		return app.WorkflowScope{}, "", err
	}
	canonical, err := filepath.EvalSymlinks(origin)
	if err != nil {
		return app.WorkflowScope{}, "", err
	}
	state, err := filepath.Abs(resolveStateDir(stateDir))
	if err != nil {
		return app.WorkflowScope{}, "", err
	}
	configuration, err := loadConfiguration(canonical, state)
	if err != nil {
		return app.WorkflowScope{}, "", err
	}
	return app.WorkflowScope{Workspace: canonical, StateDir: state, Config: configuration}, origin, nil
}

// workflowDraft 映射已知旧编辑器字段。实际节点边界由 App 验证。
func workflowDraft(item WorkflowDefinition) (app.WorkflowDefinitionDraft, error) {
	draft := app.WorkflowDefinitionDraft{ID: app.WorkflowDefinitionID(item.ID), Name: item.Name, Nodes: make([]app.WorkflowNode, len(item.Steps))}
	for index, step := range item.Steps {
		if err := validateMode(step.Mode); err != nil {
			return app.WorkflowDefinitionDraft{}, err
		}
		id := step.ID
		if id == "" {
			id = fmt.Sprintf("step-%d", index+1)
		}
		kind := step.Kind
		if kind == "" {
			kind = "execute"
		}
		draft.Nodes[index] = app.WorkflowNode{
			ID: id, Kind: kind, Prompt: step.Task, Argv: step.Argv, Model: step.Model,
			ReadOnly: strings.TrimSpace(step.Mode) == "read-only", MaxAttempts: step.MaxAttempts, Inputs: step.Inputs, RetryFrom: step.RetryFrom,
		}
	}
	return draft, nil
}

// workflowDefinitionView 从 App 修订恢复旧编辑器兼容字段。
func workflowDefinitionView(workspace string, definition app.WorkflowDefinition) WorkflowDefinition {
	item := WorkflowDefinition{Workspace: workspace, ID: string(definition.ID), Name: definition.Name, Revision: definition.Revision, Steps: make([]WorkflowStep, len(definition.Nodes))}
	for index, node := range definition.Nodes {
		mode := ""
		if node.ReadOnly {
			mode = "read-only"
		}
		item.Steps[index] = WorkflowStep{
			ID: node.ID, Kind: node.Kind, Task: node.Prompt, Argv: node.Argv, Model: node.Model,
			Mode: mode, MaxAttempts: node.MaxAttempts, Inputs: node.Inputs, RetryFrom: node.RetryFrom,
		}
	}
	return item
}

// selectedWorkflowDefinition 检查指定修订。历史导入只发生在明确请求启动时。
func (controller *RunController) selectedWorkflowDefinition(ctx context.Context, scope app.WorkflowScope, request WorkflowStartRequest) (app.WorkflowDefinition, error) {
	if strings.TrimSpace(request.DefinitionID) == "" {
		return app.WorkflowDefinition{}, errInvalid("definition_id is required")
	}
	id := app.WorkflowDefinitionID(request.DefinitionID)
	if request.Revision != 0 {
		return controller.service.ReadWorkflowDefinition(ctx, scope, id, request.Revision)
	}
	if _, err := controller.service.ReadWorkflowDefinition(ctx, scope, id, 0); !errors.Is(err, app.ErrWorkflowDefinitionNotFound) {
		if err != nil {
			return app.WorkflowDefinition{}, err
		}
		return app.WorkflowDefinition{}, errInvalid("workflow start requires an explicitly selected definition revision")
	}
	draft, err := controller.legacyWorkflowDraft(scope.Workspace, request.DefinitionID)
	if err != nil {
		return app.WorkflowDefinition{}, err
	}
	if err := controller.service.CheckWorkflowDraft(ctx, scope, draft); err != nil {
		return app.WorkflowDefinition{}, err
	}
	if request.DryRun {
		return app.WorkflowDefinition{ID: id}, nil
	}
	return controller.service.SaveWorkflowDefinition(ctx, scope, draft, 0)
}

// legacyWorkflowDraft 只读取旧格式字段。旧偏好不能声明新的命令节点。
func (controller *RunController) legacyWorkflowDraft(workspace, id string) (app.WorkflowDefinitionDraft, error) {
	for _, legacy := range controller.prefs.ListWorkflows(workspace) {
		if legacy.ID != id {
			continue
		}
		if err := validateLegacyWorkflow(legacy); err != nil {
			return app.WorkflowDefinitionDraft{}, err
		}
		steps := make([]WorkflowStep, len(legacy.Steps))
		for index, step := range legacy.Steps {
			steps[index] = WorkflowStep{Task: step.Task, Model: step.Model, Mode: step.Mode}
		}
		legacy.Steps = steps
		return workflowDraft(legacy)
	}
	return app.WorkflowDefinitionDraft{}, app.ErrWorkflowDefinitionNotFound
}

// previewUnconfiguredLegacyWorkflow 保留旧入口的无 Provider 结构预演。
func (controller *RunController) previewUnconfiguredLegacyWorkflow(ctx context.Context, workspace, id string) (WorkflowRunAck, error) {
	absolute, err := absWorkspace(workspace)
	if err != nil {
		return WorkflowRunAck{}, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return WorkflowRunAck{}, err
	}
	draft, err := controller.legacyWorkflowDraft(canonical, id)
	if err != nil {
		return WorkflowRunAck{}, err
	}
	if err := controller.service.CheckWorkflowDraftStructure(ctx, canonical, draft); err != nil {
		return WorkflowRunAck{}, err
	}
	return WorkflowRunAck{WorkflowID: id, State: "dry_run_ok"}, nil
}

// validateLegacyWorkflow 维持旧偏好反序列化及显式夹具写入的字段边界。
func validateLegacyWorkflow(definition WorkflowDefinition) error {
	const maxLegacySteps = 8
	if strings.TrimSpace(definition.ID) == "" || strings.TrimSpace(definition.Workspace) == "" || strings.TrimSpace(definition.Name) == "" {
		return errInvalid("workflow id/workspace/name required")
	}
	if len(definition.Steps) == 0 || definition.MaxSteps < 0 || definition.MaxSteps > maxLegacySteps ||
		len(definition.Steps) > maxLegacySteps || definition.MaxSteps > 0 && len(definition.Steps) > definition.MaxSteps {
		return errInvalid("workflow steps exceed limit")
	}
	for _, step := range definition.Steps {
		if strings.TrimSpace(step.Task) == "" {
			return errInvalid("workflow step task required")
		}
		if err := validateMode(step.Mode); err != nil {
			return err
		}
	}
	return nil
}

// cloneWorkflowSteps 隔离旧偏好读写的内存引用。
func cloneWorkflowSteps(steps []WorkflowStep) []WorkflowStep {
	cloned := slices.Clone(steps)
	for index := range cloned {
		cloned[index].Argv = slices.Clone(cloned[index].Argv)
		cloned[index].Inputs = slices.Clone(cloned[index].Inputs)
	}
	return cloned
}

// SaveWorkflow 仅维持旧偏好 Go 调用合同。Wails 使用 App 模板修订存储。
func (store *PrefsStore) SaveWorkflow(definition WorkflowDefinition) error {
	if err := validateLegacyWorkflow(definition); err != nil {
		return err
	}
	definition.Steps = cloneWorkflowSteps(definition.Steps)
	store.mu.Lock()
	defer store.mu.Unlock()
	for index := range store.data.Workflows {
		if store.data.Workflows[index].ID == definition.ID && store.data.Workflows[index].Workspace == definition.Workspace {
			store.data.Workflows[index] = definition
			return store.save()
		}
	}
	store.data.Workflows = append(store.data.Workflows, definition)
	return store.save()
}

// ListWorkflows 读取旧模板快照。工作区别名归一后才参与匹配。
func (store *PrefsStore) ListWorkflows(workspace string) []WorkflowDefinition {
	if store == nil {
		return nil
	}
	store.mu.RLock()
	snapshot := slices.Clone(store.data.Workflows)
	store.mu.RUnlock()
	out := make([]WorkflowDefinition, 0, len(snapshot))
	for _, definition := range snapshot {
		path, err := filepath.EvalSymlinks(definition.Workspace)
		if err == nil && path == workspace {
			definition.Steps = cloneWorkflowSteps(definition.Steps)
			out = append(out, definition)
		}
	}
	return out
}
