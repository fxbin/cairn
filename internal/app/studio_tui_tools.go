// studio_tui_tools.go 为 TUI 提供补丁预览、引用展开与工作区信任状态的
// Application 门面；Studio 层不得直接依赖 internal/tools。
// Author: fxbin
package app

import (
	"encoding/json"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/tools"
)

// StudioWorkspace 是受限工作区视图的窄别名，终端层只消费不构造内部状态。
type StudioWorkspace = tools.Workspace

// 补丁预览数据合同别名：与合同层单文件改写合同保持同一形状。
type (
	// ToolPatchArguments 是模型请求的单文件改写参数。
	ToolPatchArguments = tools.PatchArguments
	// ToolReplacement 描述一次文本替换。
	ToolReplacement = tools.Replacement
	// ToolPatchPlan 是改写前后的确定性预览。
	ToolPatchPlan = contract.PatchPlan
	// ToolModelSearchEntry 是可检索的模型目录条目（非敏感字段）。
	ToolModelSearchEntry = tools.ModelSearchEntry
)

// PlanApplyPatch 在不写入文件的前提下校验参数并计算改写预览。
func PlanApplyPatch(workspace StudioWorkspace, raw json.RawMessage) (ToolPatchPlan, error) {
	return tools.ApplyPatch{Workspace: workspace}.Plan(raw)
}

// SearchToolModels 按大小写不敏感子串过滤模型目录条目，结果有界且稳定。
func SearchToolModels(query string, entries []ToolModelSearchEntry) []ToolModelSearchEntry {
	return tools.SearchModels(query, entries)
}

// 工作区信任状态门面：只读判定与显式信任变更，路径边界仍在 Runtime 内。

// IsWorkspaceTrusted 报告工作区根是否已显式信任。
func IsWorkspaceTrusted(root string) bool { return tools.IsTrusted(root) }

// IsWorkspaceExplicitlyUntrusted 报告工作区根是否被显式标记为不受信任。
func IsWorkspaceExplicitlyUntrusted(root string) bool { return tools.IsExplicitlyUntrusted(root) }

// TrustWorkspace 将工作区根标记为显式信任。
func TrustWorkspace(root string) error { return tools.TrustWorkspace(root) }

// UntrustWorkspace 撤销工作区根的显式信任。
func UntrustWorkspace(root string) error { return tools.UntrustWorkspace(root) }

// WriteUntrustedMarker 为工作区根写入显式不受信任标记。
func WriteUntrustedMarker(root string) error { return tools.WriteUntrustedMarker(root) }

// RemoveUntrustedMarker 删除工作区根的显式不受信任标记。
func RemoveUntrustedMarker(root string) error { return tools.RemoveUntrustedMarker(root) }

// GuardTrustedWrite 要求工作区根已显式信任才允许本机写操作。
func GuardTrustedWrite(root, operation string) error { return tools.GuardTrustedWrite(root, operation) }
