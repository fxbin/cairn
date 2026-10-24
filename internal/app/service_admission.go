// 准入门禁检查与摘要。
// Author: fxbin
package app

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/provider"
)

// 不拦截，也验证现有 admitted 决定及其证据，供 Run 与 Doctor 共享同一事实。
func checkAdmissionGate(stateDir, workspaceHash string, configuration config.Config) (AdmissionHealth, error) {
	status := AdmissionHealth{Mode: configuration.Provider.Admission}
	profile, err := provider.ResolveResolvedProfile(provider.ResolvedFacts{
		Selection:     provider.SelectionFacts{ProviderID: configuration.Provider.ID, Model: configuration.Provider.Model, BaseURL: configuration.Provider.BaseURL, Protocol: configuration.Provider.Protocol},
		Capability:    config.ProviderCapabilityDeclaration(configuration),
		Generation:    config.ProviderGenerationDeclaration(configuration),
		CredentialRef: configuration.Provider.APIKeyEnv,
	})
	if err != nil {
		status.Error = admissionErrorSummary(err)
		return status, err
	}
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	decisions, err := provider.OpenDecisionStoreReadOnly(stateDir, workspaceHash)
	if err != nil {
		status.Error = admissionErrorSummary(err)
		return status, err
	}
	evidence, err := provider.OpenEvidenceStoreReadOnly(stateDir, workspaceHash)
	if err != nil {
		status.Error = admissionErrorSummary(err)
		return status, err
	}
	outcome, err := provider.EvaluateAdmissionGate(decisions, evidence, profile, config.ProviderAdmissionRequired)
	if err != nil {
		status.Error = admissionErrorSummary(err)
		return status, err
	}
	status.Admitted = outcome.Head != nil && outcome.Head.Decision == provider.DecisionAdmitted
	if configuration.Provider.Admission == config.ProviderAdmissionRequired {
		status.Admitted = outcome.Allowed
	}
	return status, nil
}

// admissionErrorSummary 把准入失败归一为不含路径和凭据的稳定诊断原因。
// 只识别类型化哨兵错误，不再对错误文本做脆弱匹配。
func admissionErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, provider.ErrNoAdmissionDecision):
		return "no admission decision on file for this binding"
	case errors.Is(err, provider.ErrAdmissionRejected):
		return "provider admission is rejected for this binding"
	case errors.Is(err, provider.ErrAdmissionSelectionMismatch):
		return "admission evidence does not match the resolved provider"
	default:
		return "admission evidence or decision state is invalid"
	}
}

// enforceAdmissionGate 执行 Provider 准入门（D4 优先级链的最后一环）：
// optional 只展示检查结果而不拦截；required 要求唯一 admitted 链头、
// 在档证据与解析身份一致。
func enforceAdmissionGate(stateDir, workspaceHash string, configuration config.Config) error {
	status, err := checkAdmissionGate(stateDir, workspaceHash, configuration)
	if configuration.Provider.Admission == config.ProviderAdmissionOptional {
		return nil
	}
	if err != nil {
		return fmt.Errorf("admission gate: %w", err)
	}
	if !status.Admitted {
		return errors.New("admission gate: provider is not admitted for this binding")
	}
	return nil
}
