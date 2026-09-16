package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// marshalFrozenRunConfig 把已校验配置投影为不含凭据定位信息的版本化制品。
// 验证摘要直接基于 Loop 实际消费的命令结构计算，保证配置、事件与 manifest
// 使用同一序列化口径。
func marshalFrozenRunConfig(configuration config.Config, configFingerprint string) ([]byte, error) {
	if err := config.Validate(configuration); err != nil {
		return nil, err
	}
	commands, err := verificationCommands(configuration.Verification)
	if err != nil {
		return nil, fmt.Errorf("build frozen verification commands: %w", err)
	}
	encodedCommands, err := json.Marshal(commands)
	if err != nil {
		return nil, fmt.Errorf("encode frozen verification commands: %w", err)
	}
	verificationDigest := sha256.Sum256(encodedCommands)
	profile := configuration.Profile
	if profile == "" {
		profile = config.ProfileDefault
	}
	preset := configuration.Preset
	if preset == "" {
		preset = config.PresetCoding
	}
	providerID := configuration.Provider.ID
	admission := configuration.Provider.Admission
	if providerID == "" {
		providerID = contract.ProviderIDCustom
	}
	var capability *contract.FrozenProviderCapabilityProjection
	if declared := configuration.Provider.Capability; declared != nil {
		capability = &contract.FrozenProviderCapabilityProjection{
			ToolCalls: declared.ToolCalls, StreamingUsage: declared.StreamingUsage,
			ContextWindowTokens: declared.ContextWindowTokens, MaxOutputTokens: declared.MaxOutputTokens,
			StopReasons: append([]string(nil), declared.StopReasons...),
		}
	}
	frozen, err := contract.NewFrozenRunConfig(configFingerprint, contract.FrozenConfigProjection{
		ConfigVersion: configuration.Version,
		Preset:        preset,
		Profile:       profile,
		Provider: contract.FrozenProviderProjection{
			Kind: contract.ProviderKindOpenAICompatible, BaseURL: configuration.Provider.BaseURL,
			Model: configuration.Provider.Model, ID: providerID, Admission: admission, Capability: capability,
		},
		Policy: contract.PolicyIdentity{
			Write: configuration.Policy.Write, Exec: configuration.Policy.Exec,
			ProtectedPaths: append([]string(nil), configuration.Policy.ProtectedPaths...),
		},
		Verification: contract.FrozenVerificationProjection{
			Commands: len(commands), Fingerprint: fmt.Sprintf("sha256:%x", verificationDigest),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build frozen run config: %w", err)
	}
	encoded, err := json.Marshal(frozen)
	if err != nil {
		return nil, fmt.Errorf("encode frozen run config: %w", err)
	}
	return encoded, nil
}

// readFrozenRunConfig 只读加载 run_created 引用的冻结配置制品，并校验内容
// 摘要一致后返回其非敏感内容。缺少引用时返回 nil，由当前合同读取器统一
// 失败关闭；引用缺失、格式错误或内容篡改同样不能伪装成合法投影。
func readFrozenRunConfig(events []domain.Event, stateDir, workspaceHash string) (json.RawMessage, error) {
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return nil, nil
	}
	var payload struct {
		ConfigRef *artifact.Ref `json:"config_ref"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode run_created config reference: %w", err)
	}
	if payload.ConfigRef == nil {
		return nil, nil
	}
	root := manifestArtifactRoot(stateDir, workspaceHash)
	content, err := readArtifactContent(root, *payload.ConfigRef)
	if err != nil {
		return nil, fmt.Errorf("verify frozen run config: %w", err)
	}
	if err := contract.ValidateFrozenRunConfig(events, content); err != nil {
		return nil, fmt.Errorf("validate frozen run config: %w", err)
	}
	return content, nil
}

// runManifest 投影一次普通运行的产品合同 manifest：任务身份来自 run_created
// 的任务全文摘要，配置侧字段来自冻结配置制品。任何解码失败都视为状态损坏。
func runManifest(events []domain.Event, projection domain.RunProjection, stateDir, workspaceHash string) (contract.RunManifest, error) {
	frozenConfig, err := readFrozenRunConfig(events, stateDir, workspaceHash)
	if err != nil {
		return contract.RunManifest{}, err
	}
	return contract.ManifestFromRun(events, contract.FreeformTaskSpec(projection.Task), frozenConfig)
}

// projectRunResult 在运行结束后从日志投影完整的 contract.Result；ctx 使用
// 调用方给出的上下文（失败路径应传入 context.Background 以避免读取被取消）。
func projectRunResult(ctx context.Context, journal *sqlite.Journal, stateDir, workspaceHash string, runID domain.RunID) (contract.Result, error) {
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return contract.Result{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return contract.Result{}, err
	}
	frozenConfig, err := readFrozenRunConfig(events, stateDir, workspaceHash)
	if err != nil {
		return contract.Result{}, err
	}
	return contract.ResultFromRun(events, projection, contract.FreeformTaskSpec(projection.Task), frozenConfig)
}
