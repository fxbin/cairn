package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/domain"
)

// manifestSchemaV1 是 version 1 配置对应的 RunManifest 产品合同版本。
const manifestSchemaV1 = 1

// manifestSchemaV2 是 version 2 配置对应的 RunManifest 产品合同版本：新增
// ProviderSelection 身份投影。由发起 run 时的 Config.Version 决定，历史
// （version 1）run 的投影永不自动补造 Selection；新 reader 支持 v1/v2，旧
// reader 不保证消费 v2。
const manifestSchemaV2 = 2

// frozenRunConfigSchemaV1 是新冻结配置制品的外层合同版本。该制品不保存
// APIKeyEnv 等凭据定位信息，只保存 manifest 所需的非敏感投影及其独立摘要。
const frozenRunConfigSchemaV1 = 1

// frozenRunConfigProjectionPrefix 隔离冻结配置投影摘要与其他 sha256 指纹域。
const frozenRunConfigProjectionPrefix = "cairn-frozen-run-config-projection-v1"

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

// FrozenRunConfig 是新运行写入制品库的版本化冻结配置合同。ConfigFingerprint
// 绑定 run_created 中的完整执行语义指纹；ProjectionFingerprint 允许读取方
// 独立验证下方非敏感投影未被重新解释。
type FrozenRunConfig struct {
	SchemaVersion         int                    `json:"schema_version"`
	ConfigFingerprint     string                 `json:"config_fingerprint"`
	ProjectionFingerprint string                 `json:"projection_fingerprint"`
	Projection            FrozenConfigProjection `json:"projection"`
}

// FrozenConfigProjection 是可安全落盘并足以构造 RunManifest 的配置子集。
// APIKeyEnv 不进入此结构，完整配置仍由 ConfigFingerprint 与创建事件绑定。
type FrozenConfigProjection struct {
	ConfigVersion int                          `json:"config_version"`
	Preset        string                       `json:"preset"`
	Profile       string                       `json:"profile"`
	Provider      FrozenProviderProjection     `json:"provider"`
	Policy        PolicyIdentity               `json:"policy"`
	Verification  FrozenVerificationProjection `json:"verification"`
}

// FrozenProviderProjection 保存非敏感 Provider 身份与显式能力声明。
type FrozenProviderProjection struct {
	Kind       string                              `json:"kind"`
	BaseURL    string                              `json:"base_url"`
	Model      string                              `json:"model"`
	ID         string                              `json:"id,omitempty"`
	Admission  string                              `json:"admission,omitempty"`
	Capability *FrozenProviderCapabilityProjection `json:"capability,omitempty"`
}

// FrozenProviderCapabilityProjection 保存构造能力卡所需的显式声明。
type FrozenProviderCapabilityProjection struct {
	ToolCalls           bool     `json:"tool_calls"`
	StreamingUsage      bool     `json:"streaming_usage"`
	ContextWindowTokens int      `json:"context_window_tokens,omitempty"`
	MaxOutputTokens     int      `json:"max_output_tokens,omitempty"`
	StopReasons         []string `json:"stop_reasons,omitempty"`
}

// FrozenVerificationProjection 保存验证命令数量及与运行事件共用的命令摘要。
type FrozenVerificationProjection struct {
	Commands    int    `json:"commands"`
	Fingerprint string `json:"fingerprint"`
}

// NewFrozenRunConfig 构造并校验版本化冻结配置制品。调用方负责从已通过
// config.Validate 的配置映射 Projection，并传入同一次运行将写入事件的完整
// 配置指纹。
func NewFrozenRunConfig(configFingerprint string, projection FrozenConfigProjection) (FrozenRunConfig, error) {
	frozen := FrozenRunConfig{
		SchemaVersion:     frozenRunConfigSchemaV1,
		ConfigFingerprint: configFingerprint,
		Projection:        projection,
	}
	fingerprint, err := canonicaljson.Fingerprint(frozenRunConfigProjectionPrefix, projection)
	if err != nil {
		return FrozenRunConfig{}, fmt.Errorf("contract: fingerprint frozen config projection: %w", err)
	}
	frozen.ProjectionFingerprint = fingerprint
	if err := validateFrozenRunConfig(frozen, configFingerprint); err != nil {
		return FrozenRunConfig{}, err
	}
	return frozen, nil
}

// frozenConfigView 是新旧冻结制品经严格解码后共享的 manifest 投影视图。
type frozenConfigView struct {
	Version                 int
	Preset                  string
	Profile                 string
	Provider                frozenProviderView
	Policy                  PolicyIdentity
	VerificationCommands    int
	VerificationFingerprint string
	StronglyBound           bool
}

// frozenProviderView 统一新旧冻结格式中的 Provider 投影。
type frozenProviderView struct {
	Kind       string
	BaseURL    string
	Model      string
	ID         string
	Admission  string
	Capability *FrozenProviderCapabilityProjection
}

// legacyFrozenConfig 严格描述旧版直接 json.Marshal(config.Config) 的制品形状。
// 这些制品没有独立投影指纹，保留只读兼容但不会冒充新格式的强绑定证明。
type legacyFrozenConfig struct {
	Version      int                      `json:"Version"`
	Profile      string                   `json:"Profile"`
	Preset       string                   `json:"Preset"`
	Provider     legacyProviderConfig     `json:"Provider"`
	Workspace    legacyWorkspaceConfig    `json:"Workspace"`
	Policy       legacyPolicyConfig       `json:"Policy"`
	Verification legacyVerificationConfig `json:"Verification"`
	Budget       legacyBudgetConfig       `json:"Budget"`
	Context      legacyContextConfig      `json:"Context"`
	Pricing      legacyPricingConfig      `json:"Pricing"`
	Output       legacyOutputConfig       `json:"Output"`
	Cancellation legacyCancellationConfig `json:"Cancellation"`
	Sandbox      legacySandboxConfig      `json:"Sandbox"`
	Resolution   []legacyResolutionEntry  `json:"resolution,omitempty"`
}

// legacyProviderConfig 描述旧版 Provider JSON 字段。
type legacyProviderConfig struct {
	BaseURL        string                          `json:"BaseURL"`
	Model          string                          `json:"Model"`
	APIKeyEnv      string                          `json:"APIKeyEnv"`
	RequestTimeout string                          `json:"RequestTimeout"`
	ID             string                          `json:"ID"`
	Admission      string                          `json:"Admission"`
	Capability     *legacyProviderCapabilityConfig `json:"Capability"`
	Generation     *legacyProviderGenerationConfig `json:"Generation"`
}

// legacyProviderCapabilityConfig 描述旧版能力声明 JSON 字段。
type legacyProviderCapabilityConfig struct {
	ToolCalls           bool     `json:"ToolCalls"`
	StreamingUsage      bool     `json:"StreamingUsage"`
	ContextWindowTokens int      `json:"ContextWindowTokens"`
	MaxOutputTokens     int      `json:"MaxOutputTokens"`
	StopReasons         []string `json:"StopReasons"`
}

// legacyProviderGenerationConfig 描述旧版生成参数 JSON 字段。
type legacyProviderGenerationConfig struct {
	MaxOutputTokens  *int   `json:"MaxOutputTokens"`
	TemperatureMilli *int   `json:"TemperatureMilli"`
	TopPMilli        *int   `json:"TopPMilli"`
	ReasoningEffort  string `json:"ReasoningEffort"`
	ToolChoice       string `json:"ToolChoice"`
}

// legacyWorkspaceConfig 描述旧版工作区限制 JSON 字段。
type legacyWorkspaceConfig struct {
	Ignore         []string `json:"Ignore"`
	MaxReadBytes   int64    `json:"MaxReadBytes"`
	MaxSearchFiles int      `json:"MaxSearchFiles"`
	MaxSearchBytes int64    `json:"MaxSearchBytes"`
}

// legacyPolicyConfig 描述旧版策略 JSON 字段。
type legacyPolicyConfig struct {
	Write          string   `json:"Write"`
	Exec           string   `json:"Exec"`
	ProtectedPaths []string `json:"ProtectedPaths"`
	EnvAllow       []string `json:"EnvAllow"`
}

// legacyVerificationConfig 描述旧版验证配置 JSON 字段。
type legacyVerificationConfig struct {
	VolatilePaths  []string                    `json:"VolatilePaths"`
	Commands       []legacyVerificationCommand `json:"Commands"`
	AutoAfterPatch bool                        `json:"AutoAfterPatch"`
}

// legacyVerificationCommand 描述旧版单条验证命令 JSON 字段。
type legacyVerificationCommand struct {
	Argv           []string          `json:"Argv"`
	CWD            string            `json:"CWD"`
	Timeout        string            `json:"Timeout"`
	MaxOutputBytes int               `json:"MaxOutputBytes"`
	Env            map[string]string `json:"Env"`
}

// legacyBudgetConfig 描述旧版预算 JSON 字段。
type legacyBudgetConfig struct {
	MaxTurns        int     `json:"MaxTurns"`
	MaxToolCalls    int     `json:"MaxToolCalls"`
	MaxInputTokens  int     `json:"MaxInputTokens"`
	MaxOutputTokens int     `json:"MaxOutputTokens"`
	MaxWallTime     string  `json:"MaxWallTime"`
	MaxCostUSD      float64 `json:"MaxCostUSD"`
}

// legacyContextConfig 描述旧版上下文配置 JSON 字段。
type legacyContextConfig struct {
	AutoCompact             bool `json:"AutoCompact"`
	MaxPromptTokens         int  `json:"MaxPromptTokens"`
	MaxSummaryBytes         int  `json:"MaxSummaryBytes"`
	InventoryBudgetPercent  int  `json:"InventoryBudgetPercent"`
	ThresholdReservePercent int  `json:"ThresholdReservePercent"`
}

// legacyPricingConfig 描述旧版价格 JSON 字段。
type legacyPricingConfig struct {
	InputTokenUSD  float64 `json:"InputTokenUSD"`
	OutputTokenUSD float64 `json:"OutputTokenUSD"`
}

// legacyOutputConfig 描述旧版制品输出限制 JSON 字段。
type legacyOutputConfig struct {
	MaxPreviewBytes  int `json:"MaxPreviewBytes"`
	MaxArtifactBytes int `json:"MaxArtifactBytes"`
}

// legacyCancellationConfig 描述旧版取消配置 JSON 字段。
type legacyCancellationConfig struct {
	TerminateGrace string `json:"TerminateGrace"`
}

// legacySandboxConfig 描述旧版执行沙箱 JSON 字段。
type legacySandboxConfig struct {
	Backend     string `json:"Backend"`
	DockerImage string `json:"DockerImage"`
	MaxPIDs     int    `json:"MaxPIDs"`
}

// legacyResolutionEntry 描述旧版冻结制品附带的配置来源记录。
type legacyResolutionEntry struct {
	Field  string `json:"field"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// ValidateFrozenRunConfig 严格解析冻结配置，并在新格式下校验内部投影摘要与
// run_created.config_fingerprint 的交叉绑定。frozenConfig 为空时按没有
// config_ref 的历史 Run 处理。
func ValidateFrozenRunConfig(events []domain.Event, frozenConfig json.RawMessage) error {
	if len(frozenConfig) == 0 {
		return nil
	}
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return errors.New("contract: run events must start with run_created")
	}
	var created struct {
		ConfigFingerprint string `json:"config_fingerprint"`
	}
	if err := json.Unmarshal(events[0].Payload, &created); err != nil {
		return fmt.Errorf("contract: decode event payload seq=%d type=%s: %w", events[0].Seq, events[0].Type, err)
	}
	_, err := decodeFrozenConfig(frozenConfig, created.ConfigFingerprint)
	return err
}

// decodeFrozenConfig 识别新旧冻结格式并返回统一投影视图。新格式必须完成
// 完整指纹交叉绑定；旧格式只作为历史只读兼容入口，但同样拒绝空对象、未知
// 字段、尾随内容和版本组合矛盾。
func decodeFrozenConfig(frozenConfig json.RawMessage, expectedConfigFingerprint string) (frozenConfigView, error) {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(frozenConfig, &shape); err != nil {
		return frozenConfigView{}, fmt.Errorf("contract: decode frozen config: %w", err)
	}
	if len(shape) == 0 {
		return frozenConfigView{}, errors.New("contract: decode frozen config: value must be a non-empty object")
	}
	if _, current := shape["schema_version"]; current {
		var frozen FrozenRunConfig
		if err := decodeStrictJSON(frozenConfig, &frozen); err != nil {
			return frozenConfigView{}, fmt.Errorf("contract: decode frozen config: %w", err)
		}
		if err := validateFrozenRunConfig(frozen, expectedConfigFingerprint); err != nil {
			return frozenConfigView{}, err
		}
		view := frozenConfigView{
			Version:                 frozen.Projection.ConfigVersion,
			Preset:                  frozen.Projection.Preset,
			Profile:                 frozen.Projection.Profile,
			Provider:                frozenProviderViewFromProjection(frozen.Projection.Provider),
			Policy:                  frozen.Projection.Policy,
			VerificationCommands:    frozen.Projection.Verification.Commands,
			VerificationFingerprint: frozen.Projection.Verification.Fingerprint,
			StronglyBound:           true,
		}
		return view, nil
	}
	var legacy legacyFrozenConfig
	if err := decodeStrictJSON(frozenConfig, &legacy); err != nil {
		return frozenConfigView{}, fmt.Errorf("contract: decode legacy frozen config: %w", err)
	}
	view := frozenConfigView{
		Version:              legacy.Version,
		Preset:               legacy.Preset,
		Profile:              legacy.Profile,
		Provider:             frozenProviderViewFromLegacy(legacy.Provider),
		Policy:               PolicyIdentity{Write: legacy.Policy.Write, Exec: legacy.Policy.Exec, ProtectedPaths: append([]string(nil), legacy.Policy.ProtectedPaths...)},
		VerificationCommands: len(legacy.Verification.Commands),
	}
	if err := validateFrozenConfigView(view, false); err != nil {
		return frozenConfigView{}, err
	}
	return view, nil
}

// decodeStrictJSON 解码单个 JSON 值，拒绝未知字段和尾随内容。
func decodeStrictJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

// validateFrozenRunConfig 校验新格式的版本、摘要、事件绑定与投影语义。
func validateFrozenRunConfig(frozen FrozenRunConfig, expectedConfigFingerprint string) error {
	if frozen.SchemaVersion != frozenRunConfigSchemaV1 {
		return fmt.Errorf("contract: frozen config schema_version %d is unsupported", frozen.SchemaVersion)
	}
	if !digestShape(frozen.ConfigFingerprint) || !digestShape(frozen.ProjectionFingerprint) {
		return errors.New("contract: frozen config fingerprints must be sha256 digests")
	}
	if expectedConfigFingerprint == "" || frozen.ConfigFingerprint != expectedConfigFingerprint {
		return errors.New("contract: frozen config fingerprint does not match run_created")
	}
	actual, err := canonicaljson.Fingerprint(frozenRunConfigProjectionPrefix, frozen.Projection)
	if err != nil {
		return fmt.Errorf("contract: fingerprint frozen config projection: %w", err)
	}
	if actual != frozen.ProjectionFingerprint {
		return errors.New("contract: frozen config projection fingerprint mismatch")
	}
	view := frozenConfigView{
		Version:                 frozen.Projection.ConfigVersion,
		Preset:                  frozen.Projection.Preset,
		Profile:                 frozen.Projection.Profile,
		Provider:                frozenProviderViewFromProjection(frozen.Projection.Provider),
		Policy:                  frozen.Projection.Policy,
		VerificationCommands:    frozen.Projection.Verification.Commands,
		VerificationFingerprint: frozen.Projection.Verification.Fingerprint,
		StronglyBound:           true,
	}
	return validateFrozenConfigView(view, true)
}

// frozenProviderViewFromProjection 把新格式 Provider 投影转换为内部共享视图。
func frozenProviderViewFromProjection(provider FrozenProviderProjection) frozenProviderView {
	return frozenProviderView{
		Kind: provider.Kind, BaseURL: provider.BaseURL, Model: provider.Model,
		ID: provider.ID, Admission: provider.Admission, Capability: provider.Capability,
	}
}

// frozenProviderViewFromLegacy 把旧版 Provider 配置转换为内部共享视图。
func frozenProviderViewFromLegacy(provider legacyProviderConfig) frozenProviderView {
	view := frozenProviderView{
		Kind: ProviderKindOpenAICompatible, BaseURL: provider.BaseURL, Model: provider.Model,
		ID: provider.ID, Admission: provider.Admission,
	}
	if capability := provider.Capability; capability != nil {
		view.Capability = &FrozenProviderCapabilityProjection{
			ToolCalls: capability.ToolCalls, StreamingUsage: capability.StreamingUsage,
			ContextWindowTokens: capability.ContextWindowTokens, MaxOutputTokens: capability.MaxOutputTokens,
			StopReasons: append([]string(nil), capability.StopReasons...),
		}
	}
	return view
}

// validateFrozenConfigView 校验进入 manifest 的关键字段组合。strong 表示新格式，
// 此时所有字段均由当前写入器生成，任何缺失或非法能力声明都必须失败关闭。
func validateFrozenConfigView(view frozenConfigView, strong bool) error {
	if view.Version != manifestSchemaV1 && view.Version != manifestSchemaV2 {
		return fmt.Errorf("contract: frozen config version %d is unsupported", view.Version)
	}
	switch view.Profile {
	case "default", "complex":
	case "":
		if strong {
			return errors.New("contract: frozen config profile is required")
		}
	default:
		return fmt.Errorf("contract: frozen config profile %q is unsupported", view.Profile)
	}
	switch view.Preset {
	case "coding", "read-only", "benchmark":
	case "":
		if strong {
			return errors.New("contract: frozen config preset is required")
		}
	default:
		return fmt.Errorf("contract: frozen config preset %q is unsupported", view.Preset)
	}
	if strong {
		if view.Provider.Kind != ProviderKindOpenAICompatible {
			return fmt.Errorf("contract: frozen config provider kind %q is unsupported", view.Provider.Kind)
		}
		if view.Provider.BaseURL == "" || view.Provider.Model == "" {
			return errors.New("contract: frozen config provider base_url and model are required")
		}
		if _, err := BaseURLIdentityDigest(view.Provider.BaseURL); err != nil {
			return fmt.Errorf("contract: frozen config provider base_url: %w", err)
		}
	}
	if view.Version == manifestSchemaV1 {
		if view.Provider.ID != "" || view.Provider.Admission != "" {
			return errors.New("contract: frozen config provider id and admission require config version 2")
		}
	} else {
		if view.Provider.BaseURL == "" || view.Provider.Model == "" {
			return errors.New("contract: version 2 frozen config requires provider base_url and model")
		}
		if view.Provider.Admission != "optional" && view.Provider.Admission != "required" {
			return errors.New("contract: version 2 frozen config provider admission must be optional or required")
		}
		if strong && view.Provider.ID != ProviderIDCustom {
			return fmt.Errorf("contract: frozen config provider id %q is unsupported", view.Provider.ID)
		}
		if strong {
			if _, err := BaseURLIdentityDigest(view.Provider.BaseURL); err != nil {
				return fmt.Errorf("contract: frozen config provider base_url: %w", err)
			}
		}
	}
	if view.Policy.Write != "" || view.Policy.Exec != "" || strong {
		if !validFrozenPolicyEffect(view.Policy.Write) || !validFrozenPolicyEffect(view.Policy.Exec) {
			return errors.New("contract: frozen config policy write and exec must be allow, deny, or ask")
		}
	}
	if view.Preset == "read-only" && (view.Policy.Write != "deny" || view.Policy.Exec != "deny" || view.VerificationCommands != 0) {
		return errors.New("contract: read-only frozen config requires deny policies and no verification commands")
	}
	if view.VerificationCommands < 0 {
		return errors.New("contract: frozen config verification command count cannot be negative")
	}
	if strong && !digestShape(view.VerificationFingerprint) {
		return errors.New("contract: frozen config verification fingerprint must be a sha256 digest")
	}
	if view.Provider.Capability != nil {
		capability := view.Provider.Capability
		_, err := ResolveProviderCard(ProviderKindOpenAICompatible, view.Provider.Model, &ProviderCapabilityDeclaration{
			ToolCalls: capability.ToolCalls, StreamingUsage: capability.StreamingUsage,
			ContextWindowTokens: capability.ContextWindowTokens, MaxOutputTokens: capability.MaxOutputTokens,
			StopReasons: capability.StopReasons,
		})
		if err != nil && strong {
			return fmt.Errorf("contract: frozen config provider capability: %w", err)
		}
	}
	return nil
}

// validFrozenPolicyEffect 判断策略值是否属于冻结合同的闭合词表。
func validFrozenPolicyEffect(value string) bool {
	return value == "allow" || value == "deny" || value == "ask"
}

// ManifestFromRun 由事件流、任务身份与冻结配置制品内容投影 RunManifest。
// 事件是运行事实的唯一来源：run_created 提供 run/workspace/build/指纹，
// 首个 model_request_started 提供实际 Provider 线协议与模型，首个
// verification_started 提供验证计划摘要。frozenConfig 可为 nil（旧 Run），
// 此时仅配置侧字段留空；非空配置与所有事件 payload 必须是合法 JSON 对象，
// 被消费事件的强类型解码失败时返回带序号和类型的损坏错误。
func ManifestFromRun(events []domain.Event, task TaskSpec, frozenConfig json.RawMessage) (RunManifest, error) {
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return RunManifest{}, errors.New("contract: run events must start with run_created")
	}
	for _, event := range events {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &object); err != nil {
			return RunManifest{}, fmt.Errorf("contract: decode event payload seq=%d type=%s: %w", event.Seq, event.Type, err)
		}
		if object == nil {
			return RunManifest{}, fmt.Errorf("contract: decode event payload seq=%d type=%s: payload must be an object", event.Seq, event.Type)
		}
		if len(object) == 0 {
			return RunManifest{}, fmt.Errorf("contract: decode event payload seq=%d type=%s: payload must be a non-empty object", event.Seq, event.Type)
		}
	}
	manifest := RunManifest{SchemaVersion: manifestSchemaV1, RunID: events[0].RunID, WorkspaceHash: events[0].WorkspaceHash, Task: task}
	var created struct {
		ConfigFingerprint string                `json:"config_fingerprint"`
		Build             *domain.BuildIdentity `json:"build"`
	}
	if err := json.Unmarshal(events[0].Payload, &created); err != nil {
		return RunManifest{}, fmt.Errorf("contract: decode event payload seq=%d type=%s: %w", events[0].Seq, events[0].Type, err)
	}
	manifest.ConfigFingerprint = created.ConfigFingerprint
	if created.Build != nil {
		manifest.Build = *created.Build
	}
	var modelSeen, verificationSeen bool
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
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return RunManifest{}, fmt.Errorf("contract: decode event payload seq=%d type=%s: %w", event.Seq, event.Type, err)
			}
			manifest.Provider.Kind = payload.Provider
			manifest.Provider.Model = payload.Model
			modelSeen = true
		case domain.EventVerificationStarted:
			if verificationSeen {
				continue
			}
			var payload struct {
				CommandsFingerprint string `json:"commands_fingerprint"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return RunManifest{}, fmt.Errorf("contract: decode event payload seq=%d type=%s: %w", event.Seq, event.Type, err)
			}
			manifest.Verification.Fingerprint = payload.CommandsFingerprint
			verificationSeen = true
		}
	}
	if len(frozenConfig) != 0 {
		view, err := decodeFrozenConfig(frozenConfig, created.ConfigFingerprint)
		if err != nil {
			return RunManifest{}, err
		}
		if modelSeen {
			if manifest.Provider.Kind != view.Provider.Kind {
				return RunManifest{}, fmt.Errorf("contract: model request provider %q conflicts with frozen config provider %q", manifest.Provider.Kind, view.Provider.Kind)
			}
			if manifest.Provider.Model != view.Provider.Model {
				return RunManifest{}, fmt.Errorf("contract: model request model %q conflicts with frozen config model %q", manifest.Provider.Model, view.Provider.Model)
			}
		}
		if verificationSeen && view.StronglyBound && manifest.Verification.Fingerprint != view.VerificationFingerprint {
			return RunManifest{}, errors.New("contract: verification event fingerprint conflicts with frozen config")
		}
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
		manifest.Verification.Commands = view.VerificationCommands
		if view.Provider.Capability != nil {
			manifest.Provider.Card = manifestCapabilityCard(manifest, view)
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
