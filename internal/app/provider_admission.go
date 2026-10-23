// 本文件提供桌面交互式 Provider 准入，复用原生证据、决定存储与运行准入门。
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/provider"
)

// ConfirmProviderAdmissionOptions 绑定用户所见的配置、请求画像和成功探测，
// 仅允许确认准入；替换已有决定必须明确携带用户所见的链头指纹。
type ConfirmProviderAdmissionOptions struct {
	Workspace                 string
	StateDir                  string
	Config                    config.Config
	ConfigFingerprint         string
	RequestProfileFingerprint string
	EvidenceFingerprint       string
	Supersedes                string
	Reason                    string
}

// admissionProfile 解析与运行门相同的请求画像及指纹，禁止由 UI 拼装身份。
func admissionProfile(configuration config.Config) (contract.ResolvedProviderProfile, string, error) {
	profile, err := provider.ResolveResolvedProfile(provider.ResolvedFacts{
		Selection:     provider.SelectionFacts{ProviderID: configuration.Provider.ID, Model: configuration.Provider.Model, BaseURL: configuration.Provider.BaseURL, Protocol: configuration.Provider.Protocol},
		Capability:    config.ProviderCapabilityDeclaration(configuration),
		Generation:    config.ProviderGenerationDeclaration(configuration),
		CredentialRef: configuration.Provider.APIKeyEnv,
	})
	if err != nil {
		return contract.ResolvedProviderProfile{}, "", err
	}
	fingerprint, err := profile.Fingerprint()
	return profile, fingerprint, err
}

// ConfirmProviderAdmission 在工作区排他锁内验证当前绑定、成功的两程证据和
// 既有决定链，然后追加交互式决定；不发起网络请求，不隐式改变配置。
func (service Service) ConfirmProviderAdmission(ctx context.Context, options ConfirmProviderAdmissionOptions) (fingerprint string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := config.Validate(options.Config); err != nil {
		return "", err
	}
	profile, requestFingerprint, err := admissionProfile(options.Config)
	if err != nil {
		return "", err
	}
	if options.ConfigFingerprint == "" || options.ConfigFingerprint != options.Config.Fingerprint() || options.RequestProfileFingerprint != requestFingerprint {
		return "", errors.New("provider configuration changed; probe and review it again")
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return "", err
	}
	stateDir := options.StateDir
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	workspaceLock, err := acquireWorkspaceLock(stateDir, workspaceHash)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, workspaceLock.Release()) }()
	evidenceStore, err := provider.OpenEvidenceStoreReadOnly(stateDir, workspaceHash)
	if err != nil {
		return "", err
	}
	evidence, err := evidenceStore.Get(options.EvidenceFingerprint)
	if err != nil {
		return "", err
	}
	if evidence.Selection != profile.Selection {
		return "", provider.ErrAdmissionSelectionMismatch
	}
	if evidence.Outcome != provider.EvidenceOutcomeCompleted || evidence.Observations.ToolRoundtrip != provider.RoundtripOK || evidence.Observations.UsageStreamed != provider.ObservationObservedTrue {
		return "", errors.New("interactive admission requires a successful tool roundtrip and streaming usage probe")
	}
	decisions, err := provider.OpenDecisionStore(stateDir, workspaceHash)
	if err != nil {
		return "", err
	}
	partition, err := decisions.ListPartition(requestFingerprint)
	if err != nil {
		return "", err
	}
	if len(partition) == 0 {
		if options.Supersedes != "" {
			return "", errors.New("admission decision changed; review it again")
		}
	} else {
		head, err := provider.ResolveChainHead(partition)
		if err != nil {
			return "", err
		}
		if options.Supersedes != head.DecisionFingerprint {
			return "", errors.New("admission decision changed; review it again")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	decision := provider.ProviderAdmissionDecision{
		SchemaVersion: 1, EvidenceFingerprint: options.EvidenceFingerprint,
		Binding:  provider.AdmissionBinding{RequestProfileFingerprint: requestFingerprint, ConfigFingerprint: options.Config.Fingerprint()},
		Decision: provider.DecisionAdmitted, DecidedAt: time.Now().UTC(),
		Authority: provider.AuthorityInteractiveConfirm, Reason: options.Reason, SupersedesDecisionFingerprint: options.Supersedes,
	}
	fingerprint, err = decisions.Append(&decision)
	if err != nil {
		return "", err
	}
	partition, err = decisions.ListPartition(requestFingerprint)
	if err != nil {
		return "", err
	}
	_, err = provider.ResolveChainHead(partition)
	return fingerprint, err
}
