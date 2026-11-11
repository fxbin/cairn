// authorizer.go 以纯合同暴露授权门禁的补丁规划与工作区观察能力。
// Core 无需访问 Gate 内部字段；策略求值仍由 Gate 的既有实现负责。
// Author: fxbin
package policy

import (
	"encoding/json"

	"github.com/fxbin/cairn/internal/contract"
)

// PlanPatch 复用确定性单文件规划器，保留路径、前置摘要校验及原始错误类型。
// 返回视图不携带写入能力；实际写入仍须经授权、Journal 与工具执行链。
func (gate *Gate) PlanPatch(raw json.RawMessage) (contract.PatchPlan, error) {
	plan, err := gate.Patch.Plan(raw)
	if err != nil {
		return contract.PatchPlan{}, err
	}
	return contract.PatchPlan{
		Path: plan.Path, Mode: plan.Mode, Before: plan.Before,
		BeforeHash: plan.BeforeHash, AfterHash: plan.AfterHash, Exists: plan.Exists,
	}, nil
}

// WorkspaceObservation 沿用工具视图的观察摘要；verifier 的完整观察由独立视图提供。
func (gate *Gate) WorkspaceObservation() (string, error) {
	return gate.Patch.WorkspaceObservation()
}
