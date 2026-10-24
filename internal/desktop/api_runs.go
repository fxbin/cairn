// 运行启停、对账与检查接口。
// Author: fxbin
package desktop

import (
	"context"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

func (api *API) InspectRun(ctx context.Context, request InspectRequest) (RunDetail, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return RunDetail{}, err
	}
	if strings.TrimSpace(request.RunID) == "" {
		return RunDetail{}, errInvalid("run_id is required")
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return RunDetail{}, err
	}
	inspected, err := api.service.Inspect(ctx, workspace, resolveStateDir(request.StateDir), configuration, domain.RunID(request.RunID), request.IncludeEvents)
	if err != nil {
		return RunDetail{}, err
	}
	detail := RunDetail{
		RunID:     request.RunID,
		SessionID: inspected.Projection.SessionID,
		State:     inspected.Projection.State,
		Phase:     inspected.Projection.Phase(),
		Task:      inspected.Projection.Task,
		Summary:   inspected.Summary,
		Manifest:  inspected.Manifest,
		Events:    inspected.Events,
	}
	return detail, nil
}

// EvidenceFor 返回指定运行的完成证据。
func (api *API) EvidenceFor(ctx context.Context, request InspectRequest) (contract.Result, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return contract.Result{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return contract.Result{}, err
	}
	return api.service.Evidence(ctx, workspace, resolveStateDir(request.StateDir), configuration, domain.RunID(request.RunID))
}

// StartRun 启动任务（异步）。
func (api *API) StartRun(ctx context.Context, request StartRunRequest) (StartRunAck, error) {
	return api.controller.StartRun(ctx, request)
}

// ResumeRun 恢复/审批（异步）。
func (api *API) ResumeRun(ctx context.Context, request ResumeRequest) (StartRunAck, error) {
	return api.controller.ResumeRun(ctx, request)
}

// AbortRun 收尾运行。
func (api *API) AbortRun(ctx context.Context, request AbortRequest) (RunStatusView, error) {
	return api.controller.AbortRun(ctx, request)
}

// ResolveReconcile 对账悬而未决副作用。
func (api *API) ResolveReconcile(ctx context.Context, request ResolveRequest) (RunStatusView, error) {
	return api.controller.ResolveReconcile(ctx, request)
}

// CancelRun 取消运行。
func (api *API) CancelRun(_ context.Context, request CancelRequest) (OKResult, error) {
	if err := api.controller.Cancel(request.RunID, request.Reason); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// Doctor 返回诊断摘要。
