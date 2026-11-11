// Hook、工作流、记忆、Provider 扩展接口。
// Author: fxbin
package desktop

import (
	"context"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

func (api *API) Shutdown(timeout time.Duration) error {
	return api.controller.Shutdown(timeout)
}

// SubmitSteering 向运行中的任务投递立即补充输入。
func (api *API) SubmitSteering(_ context.Context, request SteeringRequest) (OKResult, error) {
	if err := api.controller.SubmitSteering(request.RunID, request.InputID, request.Content); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ListHooks 列出工作区钩子规则（有限动作，不授予权限）。
func (api *API) ListHooks(_ context.Context, workspace string) ([]HookRule, error) {
	if api.prefs == nil {
		return nil, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	return api.prefs.ListHooks(abs), nil
}

// UpsertHook 创建或更新钩子规则；仅允许 queue_followup。
func (api *API) UpsertHook(_ context.Context, rule HookRule) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(rule.Workspace)
	if err != nil {
		return OKResult{}, err
	}
	rule.Workspace = abs
	if rule.Event == "" {
		rule.Event = "run_finished"
	}
	if rule.ID == "" {
		rule.ID = domain.NewID("hook")
	}
	if err := api.prefs.UpsertHook(rule); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// DisableHook 停用钩子。
func (api *API) DisableHook(_ context.Context, id string) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	if err := api.prefs.DisableHook(id); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ProjectInstructionsView 是规约发现的只读投影。
type ProjectInstructionsView struct {
	Documents []ProjectInstructionDoc `json:"documents"`
}

// ProjectInstructionDoc 单条规约来源与摘要。
type ProjectInstructionDoc struct {
	Source  string `json:"source,omitempty"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Bytes   int    `json:"bytes,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// ListProjectInstructions 发现工作区/用户 AGENTS.md，不解释正文命令。
