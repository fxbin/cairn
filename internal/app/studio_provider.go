// studio_provider.go 为 CLI/TUI 提供模型目录与 Provider 准入的 Application
// 门面；Studio 层不得直接依赖 internal/modelcatalog 与 internal/provider。
// Author: fxbin
package app

import (
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/runtime/modelcatalog"
	"github.com/fxbin/cairn/internal/runtime/provider"
)

// 模型目录合同类型别名：CLI/TUI 仅作建议展示。
type (
	// ModelCatalog 是已装载的模型目录。
	ModelCatalog = modelcatalog.Catalog
	// ModelCatalogOptions 是目录装载选项。
	ModelCatalogOptions = modelcatalog.Options
	// ModelEntry 是单条模型建议。
	ModelEntry = modelcatalog.ModelEntry
	// ProviderInfo 是目录中的 Provider 身份信息。
	ProviderInfo = modelcatalog.ProviderInfo
)

// Provider 准入合同类型别名。
type (
	// ProviderAdmissionDecision 是一次准入决定。
	ProviderAdmissionDecision = provider.ProviderAdmissionDecision
	// ProviderAdmissionBinding 是决定绑定的指纹对。
	ProviderAdmissionBinding = provider.AdmissionBinding
	// ProviderDecisionStore 是准入决定存储。
	ProviderDecisionStore = provider.DecisionStore
	// ProviderEvidenceStore 是探测证据存储。
	ProviderEvidenceStore = provider.EvidenceStore
	// ProviderAdmissionOutcome 是准入门评估结果。
	ProviderAdmissionOutcome = provider.AdmissionOutcome
	// ProviderResolvedFacts 是解析 Provider 身份的输入事实。
	ProviderResolvedFacts = provider.ResolvedFacts
	// ProviderSelectionFacts 是显式选择的 Provider 事实。
	ProviderSelectionFacts = provider.SelectionFacts
)

// Provider 准入常量别名。
const (
	ProviderDecisionAdmitted    = provider.DecisionAdmitted
	ProviderDecisionRejected    = provider.DecisionRejected
	ProviderAuthorityMaintainer = provider.AuthorityMaintainer
)

// ModelCatalogDefaultCachePath 返回模型目录默认缓存路径。
func ModelCatalogDefaultCachePath() (string, error) { return modelcatalog.DefaultCachePath() }

// ModelCatalogLoad 装载模型目录（缓存优先）。
func ModelCatalogLoad(options modelcatalog.Options) (*modelcatalog.Catalog, error) {
	return modelcatalog.Load(options)
}

// ModelCatalogRefresh 强制刷新模型目录。
func ModelCatalogRefresh(options modelcatalog.Options) (*modelcatalog.Catalog, error) {
	return modelcatalog.Refresh(options)
}

// ProviderOpenDecisionStore 打开准入决定存储。
func ProviderOpenDecisionStore(stateDir, workspaceHash string) (*provider.DecisionStore, error) {
	return provider.OpenDecisionStore(stateDir, workspaceHash)
}

// ProviderOpenEvidenceStore 打开探测证据存储。
func ProviderOpenEvidenceStore(stateDir, workspaceHash string) (*provider.EvidenceStore, error) {
	return provider.OpenEvidenceStore(stateDir, workspaceHash)
}

// ProviderResolveChainHead 解析准入决定链头。
func ProviderResolveChainHead(partition []provider.ProviderAdmissionDecision) (*provider.ProviderAdmissionDecision, error) {
	return provider.ResolveChainHead(partition)
}

// ProviderEvaluateAdmissionGate 评估当前准入门。
func ProviderEvaluateAdmissionGate(decisions *provider.DecisionStore, evidence *provider.EvidenceStore, profile contract.ResolvedProviderProfile, admission string) (provider.AdmissionOutcome, error) {
	return provider.EvaluateAdmissionGate(decisions, evidence, profile, admission)
}

// ProviderResolveResolvedProfile 从事实解析 Provider 请求画像。
func ProviderResolveResolvedProfile(facts provider.ResolvedFacts) (contract.ResolvedProviderProfile, error) {
	return provider.ResolveResolvedProfile(facts)
}
