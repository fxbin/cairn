package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/agent"
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
	commandsFingerprint, err := agent.FingerprintVerificationCommands(commands)
	if err != nil {
		return nil, fmt.Errorf("fingerprint frozen verification commands: %w", err)
	}
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
			Commands: len(commands), Fingerprint: commandsFingerprint,
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

// readLatestVerificationPlan 按最新 finished 事件定位同轮次 started 所引用的
// 冻结计划制品。旧事件未保存 plan_ref 时返回 nil，读侧据此降为不可信；引用
// 存在却无法校验内容时返回错误，避免把损坏制品当作正常缺证。
func readLatestVerificationPlan(events []domain.Event, finishedSeq uint64, stateDir, workspaceHash string) (json.RawMessage, error) {
	if finishedSeq == 0 {
		return nil, nil
	}
	finishIndex := -1
	var verificationID string
	var turnID domain.TurnID
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationFinished || event.Seq != finishedSeq || event.TurnID == nil {
			continue
		}
		var payload domain.VerificationFinishedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode verification finish: %w", err)
		}
		finishIndex, verificationID, turnID = index, payload.VerificationID, *event.TurnID
		break
	}
	if finishIndex < 0 || verificationID == "" {
		return nil, nil
	}
	for index := finishIndex - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationStarted || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload struct {
			VerificationID string        `json:"verification_id"`
			PlanRef        *artifact.Ref `json:"plan_ref"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode verification start: %w", err)
		}
		if payload.VerificationID != verificationID || payload.PlanRef == nil {
			return nil, nil
		}
		content, err := readArtifactContent(manifestArtifactRoot(stateDir, workspaceHash), *payload.PlanRef)
		if err != nil {
			return nil, fmt.Errorf("verify frozen verification plan: %w", err)
		}
		return content, nil
	}
	return nil, nil
}

// runResultFromEvents 从冻结配置、最新验证计划与已回放事件构造统一结果，
// 供运行返回、只读 Inspect、Evidence 和记忆候选使用同一可信口径。
func runResultFromEvents(events []domain.Event, projection domain.RunProjection, stateDir, workspaceHash string) (contract.Result, error) {
	frozenConfig, err := readFrozenRunConfig(events, stateDir, workspaceHash)
	if err != nil {
		return contract.Result{}, err
	}
	plan, err := readLatestVerificationPlan(events, projection.LastVerificationSeq, stateDir, workspaceHash)
	if err != nil {
		return contract.Result{}, err
	}
	return contract.ResultFromRun(events, projection, contract.FreeformTaskSpec(projection.Task), frozenConfig, plan)
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
	return runResultFromEvents(events, projection, stateDir, workspaceHash)
}
