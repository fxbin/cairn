package contract

import (
	"encoding/json"
	"errors"

	"github.com/fxbin/cairn/internal/domain"
)

// manifestSchemaV1 是 version 1 配置对应的 RunManifest 产品合同版本。
const manifestSchemaV1 = 1

// manifestSchemaV2 是 version 2 配置对应的 RunManifest 产品合同版本：新增
// ProviderSelection 身份投影。由发起 run 时的 Config.Version 决定，历史
// （version 1）run 的投影永不自动补造 Selection；新 reader 支持 v1/v2，旧
// reader 不保证消费 v2。
const manifestSchemaV2 = 2

// ProviderKindOpenAICompatible 是当前唯一生产 Provider 线协议标识。
const ProviderKindOpenAICompatible = "openai_compatible"

// ProviderIdentity 定位一次运行实际使用的模型服务：线协议种类、端点、模型
// 与运行期能力卡。BaseURL、Model 与 Card 来自冻结配置，Kind 来自事件中的
// 真实模型请求。卡回放与 Boot 共用同一解析函数：仅显式能力声明产出卡；
// 声明缺失或解析失败（防御路径，合法 run 不可达）时保持 nil 不伪造。
type ProviderIdentity struct {
	Kind      string                  `json:"kind,omitempty"`
	BaseURL   string                  `json:"base_url,omitempty"`
	Model     string                  `json:"model,omitempty"`
	Card      *ProviderCapabilityCard `json:"card,omitempty"`
	Selection *ProviderSelection      `json:"selection,omitempty"`
}

// PolicyIdentity 暴露进入执行语义的写入与命令策略；它们已绑定在配置指纹内。
type PolicyIdentity struct {
	Write          string   `json:"write,omitempty"`
	Exec           string   `json:"exec,omitempty"`
	ProtectedPaths []string `json:"protected_paths,omitempty"`
}

// VerificationIdentity 定位运行期验证计划：命令数量与冻结计划摘要。
// Fingerprint 来自 verification_started 事件，与 capability manifest 同算法。
type VerificationIdentity struct {
	Commands    int    `json:"commands,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// RunManifest 是定位一次 Run 所需的版本化、非敏感身份集合：任务、工作区、
// 构建身份、配置指纹、Provider/模型、Preset/Profile/Policy 与验证计划。
// 全部字段由事件与冻结配置制品投影；旧 Run 缺失的字段保持空值而非伪造。
type RunManifest struct {
	SchemaVersion     int                  `json:"schema_version"`
	RunID             domain.RunID         `json:"run_id"`
	WorkspaceHash     string               `json:"workspace_hash,omitempty"`
	Task              TaskSpec             `json:"task"`
	Build             domain.BuildIdentity `json:"build,omitempty"`
	ConfigFingerprint string               `json:"config_fingerprint,omitempty"`
	Provider          ProviderIdentity     `json:"provider"`
	Preset            string               `json:"preset,omitempty"`
	Profile           string               `json:"profile,omitempty"`
	Policy            PolicyIdentity       `json:"policy"`
	Verification      VerificationIdentity `json:"verification"`
}

// frozenConfigView 只挑取冻结配置制品中进入 manifest 的非敏感字段；配置
// 结构仅带 yaml 标签，其 JSON 编码沿用 Go 字段名（如 BaseURL），此处必须
// 与之对齐。它绝不反序列化凭据类内容，未知字段一律忽略。
type frozenConfigView struct {
	Version  int    `json:"Version"`
	Preset   string `json:"Preset"`
	Profile  string `json:"Profile"`
	Provider struct {
		BaseURL    string `json:"BaseURL"`
		Model      string `json:"Model"`
		ID         string `json:"ID"`
		Admission  string `json:"Admission"`
		Capability *struct {
			ToolCalls           bool     `json:"ToolCalls"`
			StreamingUsage      bool     `json:"StreamingUsage"`
			ContextWindowTokens int      `json:"ContextWindowTokens"`
			MaxOutputTokens     int      `json:"MaxOutputTokens"`
			StopReasons         []string `json:"StopReasons"`
		} `json:"Capability"`
	} `json:"Provider"`
	Policy struct {
		Write          string   `json:"Write"`
		Exec           string   `json:"Exec"`
		ProtectedPaths []string `json:"ProtectedPaths"`
	} `json:"Policy"`
	Verification struct {
		Commands []json.RawMessage `json:"Commands"`
	} `json:"Verification"`
}

// ManifestFromRun 由事件流、任务身份与冻结配置制品内容投影 RunManifest。
// 事件是运行事实的唯一来源：run_created 提供 run/workspace/build/指纹，
// 首个 model_request_started 提供实际 Provider 线协议与模型，首个
// verification_started 提供验证计划摘要。frozenConfig 可为 nil（旧 Run 或
// 制品不可读时），此时仅配置侧字段留空；事件缺失同样只留空、不报错，
// 唯一错误条件是事件流没有 run_created 起点。
func ManifestFromRun(events []domain.Event, task TaskSpec, frozenConfig json.RawMessage) (RunManifest, error) {
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return RunManifest{}, errors.New("contract: run events must start with run_created")
	}
	manifest := RunManifest{SchemaVersion: manifestSchemaV1, RunID: events[0].RunID, WorkspaceHash: events[0].WorkspaceHash, Task: task}
	var created struct {
		ConfigFingerprint string                `json:"config_fingerprint"`
		Build             *domain.BuildIdentity `json:"build"`
	}
	if err := json.Unmarshal(events[0].Payload, &created); err != nil {
		return RunManifest{}, errors.New("contract: decode run_created payload")
	}
	manifest.ConfigFingerprint = created.ConfigFingerprint
	if created.Build != nil {
		manifest.Build = *created.Build
	}
	var modelSeen bool
	for _, event := range events {
		switch event.Type {
		case domain.EventModelRequestStarted:
			if modelSeen {
				continue
			}
			var payload struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				manifest.Provider.Kind = payload.Provider
				manifest.Provider.Model = payload.Model
				modelSeen = true
			}
		case domain.EventVerificationStarted:
			if manifest.Verification.Fingerprint != "" {
				continue
			}
			var payload struct {
				CommandsFingerprint string `json:"commands_fingerprint"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				manifest.Verification.Fingerprint = payload.CommandsFingerprint
			}
		}
	}
	if len(frozenConfig) != 0 {
		var view frozenConfigView
		if json.Unmarshal(frozenConfig, &view) == nil {
			manifest.Preset = view.Preset
			manifest.Profile = view.Profile
			manifest.Provider.BaseURL = view.Provider.BaseURL
			if manifest.Provider.Model == "" {
				manifest.Provider.Model = view.Provider.Model
			}
			if view.Version == manifestSchemaV2 {
				manifest.SchemaVersion = manifestSchemaV2
				manifest.Provider.Selection = manifestProviderSelection(view)
			}
			manifest.Policy = PolicyIdentity{Write: view.Policy.Write, Exec: view.Policy.Exec, ProtectedPaths: view.Policy.ProtectedPaths}
			manifest.Verification.Commands = len(view.Verification.Commands)
			if view.Provider.Capability != nil {
				manifest.Provider.Card = manifestCapabilityCard(manifest, view)
			}
		}
	}
	return manifest, nil
}

// manifestProviderSelection 由冻结配置视图派生 version 2 的 ProviderSelection
// 身份投影：provider_id 缺省归一为 custom；protocol_id 取当前唯一注册协议
// （注册表成员与协议的编译期绑定，此处不引入第二份映射）；model_ref 是派生
// 值；endpoint_digest 复用无凭据规范化摘要。派生失败（防御路径，version 2
// 配置已在校验期保证基地址合法）时保持 nil 不伪造身份。
func manifestProviderSelection(view frozenConfigView) *ProviderSelection {
	providerID := view.Provider.ID
	if providerID == "" {
		providerID = ProviderIDCustom
	}
	digest, err := BaseURLIdentityDigest(view.Provider.BaseURL)
	if err != nil || view.Provider.Model == "" {
		return nil
	}
	return &ProviderSelection{
		SchemaVersion:  1,
		ProviderID:     providerID,
		ProtocolID:     ProtocolOpenAIChatCompletions,
		ModelRef:       providerID + "/" + view.Provider.Model,
		EndpointDigest: digest,
	}
}

// manifestCapabilityCard 由冻结配置中的显式能力声明构建运行期卡；它与 Boot
// 时的 resolveProviderCard 使用同一解析函数，保证回放视图与装配视图不漂移。
func manifestCapabilityCard(manifest RunManifest, view frozenConfigView) *ProviderCapabilityCard {
	declared := view.Provider.Capability
	card, err := ResolveProviderCard(ProviderKindOpenAICompatible, manifest.Provider.Model, &ProviderCapabilityDeclaration{
		ToolCalls: declared.ToolCalls, StreamingUsage: declared.StreamingUsage,
		ContextWindowTokens: declared.ContextWindowTokens, MaxOutputTokens: declared.MaxOutputTokens,
		StopReasons: declared.StopReasons,
	})
	if err != nil {
		return nil
	}
	return &card
}
