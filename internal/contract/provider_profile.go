// Package contract 的 Provider Request Profile 部分定义版本化的评测请求合同：
// 它把实际生效的提供方身份、能力声明与生成参数投影为可进指纹的闭合结构，
// 供 EvalSet 冻结与合同检查消费。全部字段为整数、布尔、字符串与数组，禁止
// 浮点与路径；模型修订无法钉住时必须显式记录 absent，不伪装成确定性版本。
//
// Author: fxbin
package contract

import (
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

// providerProfileSchemaV1 是 ProviderRequestProfile 的当前合同版本。
const providerProfileSchemaV1 = 1

// 生成参数枚举与 Milli 边界是产品合同的单一事实源：config 层引用同一常量
// 完成校验，契约层在直接构造调用时重复防线，两处取值不允许漂移。
const (
	GenerationToolChoiceAuto = "auto"
	ReasoningEffortOff       = "off"
	ReasoningEffortLow       = "low"
	ReasoningEffortMedium    = "medium"
	ReasoningEffortHigh      = "high"
	// GenerationTempMaxMilli 是温度千分位的闭合上限，0 为显式确定性取值。
	GenerationTempMaxMilli = 2000
	// GenerationTopPMinMilli 与 GenerationTopPMaxMilli 是 top_p 千分位区间。
	GenerationTopPMinMilli = 1
	GenerationTopPMaxMilli = 1000
)

// pricingSnapshotSchemaV1 是 PricingSnapshot 的当前合同版本。
const pricingSnapshotSchemaV1 = 1

// RevisionAbsent 是模型修订的显式缺席标记：别名或兼容端点无法钉住服务端
// 模型修订时，profile 必须携带该字面量，由成对交错执行降低时段漂移。
const RevisionAbsent = "absent"

// capabilityFingerprintPrefix 与 baseURLEntityFingerprintPrefix 分别是能力卡
// 指纹与基地址身份摘要的版本化域前缀。
const (
	capabilityFingerprintPrefix = "cairn-provider-capability-v1"
	baseURLIdentityPrefix       = "cairn-base-url-identity-v1"
)

// pricingFingerprintPrefix 是冻结单价快照指纹的版本化域前缀。
const pricingFingerprintPrefix = "cairn-pricing-snapshot-v1"

// microusdPerMillionTokensExponent 把 USD/token 折算为 micro-USD/M token 的
// 十进制位移量：USD/token ×10^6（美元转微美分）×10^6（token 转百万）= ×10^12。
const microusdPerMillionTokensExponent = 12

// microusdUpperBound 约束折算结果落在 int64 可表示范围内，防御荒谬价格输入。
const microusdUpperBound = int64(1) << 62

// ProviderGenerationDeclaration 是上层传入的生成参数显式值集合：字段缺失
// 即 unsupported，契约层不再引入第三态。
type ProviderGenerationDeclaration struct {
	MaxOutputTokens  int
	TemperatureMilli *int
	TopPMilli        *int
	ReasoningEffort  string
	ToolChoice       string
}

// ProviderGenerationProfile 是进入请求合同的生成参数投影：未声明字段经
// omitempty 缺席表达 unsupported，canonical 序列化后显式值与缺席在字节层面
// 可区分，满足"支持即序列化、不支持即声明"的两态要求。
type ProviderGenerationProfile struct {
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	TemperatureMilli *int   `json:"temperature_milli,omitempty"`
	TopPMilli        *int   `json:"top_p_milli,omitempty"`
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	ToolChoice       string `json:"tool_choice,omitempty"`
}

// ProviderRequestDeclaration 是解析 profile 所需的全部来源事实，由 config 层
// 完成严格校验后传入；契约层保持对 config 包零依赖。
type ProviderRequestDeclaration struct {
	BaseURL       string
	Model         string
	ModelRevision string
	Capability    *ProviderCapabilityDeclaration
	Generation    *ProviderGenerationDeclaration
}

// ProviderRequestProfile 是版本化、可冻结、可复算的模型请求身份合同：
// 同一指纹意味着同一提供方端点、同一模型修订语义、同一能力边界与同一
// 生成参数两态组合；它是 EvalSet 冻结与跨 run 对比的前提。
type ProviderRequestProfile struct {
	SchemaVersion         int                       `json:"schema_version"`
	ProviderKind          string                    `json:"provider_kind"`
	BaseURLIdentityDigest string                    `json:"base_url_identity_digest"`
	Model                 string                    `json:"model"`
	ProviderModelRevision string                    `json:"provider_model_revision"`
	CapabilityFingerprint string                    `json:"capability_fingerprint"`
	GenerationOptions     ProviderGenerationProfile `json:"generation_options"`
}

// ResolveProviderRequestProfile 从声明事实构建请求合同：能力卡未显式声明时
// 失败关闭（未知能力不可评测），基地址必须通过无凭据规范化校验后才允许
// 进入身份摘要，生成参数在契约层再次做闭合区间校验以防御直接构造调用。
func ResolveProviderRequestProfile(kind string, declaration ProviderRequestDeclaration) (ProviderRequestProfile, error) {
	if kind != ProviderKindOpenAICompatible {
		return ProviderRequestProfile{}, fmt.Errorf("provider kind %q has no request profile contract", kind)
	}
	if declaration.Model == "" {
		return ProviderRequestProfile{}, errors.New("provider request profile requires a model")
	}
	card, err := ResolveProviderCard(kind, declaration.Model, declaration.Capability)
	if err != nil {
		return ProviderRequestProfile{}, err
	}
	capabilityDigest, err := card.Fingerprint()
	if err != nil {
		return ProviderRequestProfile{}, fmt.Errorf("fingerprint capability card: %w", err)
	}
	digest, err := BaseURLIdentityDigest(declaration.BaseURL)
	if err != nil {
		return ProviderRequestProfile{}, err
	}
	generation, err := resolveGenerationProfile(declaration.Generation, card)
	if err != nil {
		return ProviderRequestProfile{}, err
	}
	revision := declaration.ModelRevision
	if revision == "" {
		revision = RevisionAbsent
	}
	return ProviderRequestProfile{
		SchemaVersion:         providerProfileSchemaV1,
		ProviderKind:          kind,
		BaseURLIdentityDigest: digest,
		Model:                 declaration.Model,
		ProviderModelRevision: revision,
		CapabilityFingerprint: capabilityDigest,
		GenerationOptions:     generation,
	}, nil
}

// resolveGenerationProfile 校验并投影生成参数块；上限与枚举约束和能力卡
// 对照重复一次属于纵深防御：直接构造调用的调用方绕过了 config 层校验，
// 合同层必须在发出网络请求前保持失败关闭。
func resolveGenerationProfile(generation *ProviderGenerationDeclaration, card ProviderCapabilityCard) (ProviderGenerationProfile, error) {
	if generation == nil {
		return ProviderGenerationProfile{}, nil
	}
	if generation.MaxOutputTokens < 0 {
		return ProviderGenerationProfile{}, errors.New("generation max_output_tokens cannot be negative")
	}
	if generation.MaxOutputTokens > 0 && card.MaxOutputTokens > 0 && generation.MaxOutputTokens > card.MaxOutputTokens {
		return ProviderGenerationProfile{}, fmt.Errorf("generation max_output_tokens %d exceeds declared capability limit %d", generation.MaxOutputTokens, card.MaxOutputTokens)
	}
	if generation.TemperatureMilli != nil && (*generation.TemperatureMilli < 0 || *generation.TemperatureMilli > GenerationTempMaxMilli) {
		return ProviderGenerationProfile{}, fmt.Errorf("generation temperature_milli %d outside [0,%d]", *generation.TemperatureMilli, GenerationTempMaxMilli)
	}
	if generation.TopPMilli != nil && (*generation.TopPMilli < GenerationTopPMinMilli || *generation.TopPMilli > GenerationTopPMaxMilli) {
		return ProviderGenerationProfile{}, fmt.Errorf("generation top_p_milli %d outside [%d,%d]", *generation.TopPMilli, GenerationTopPMinMilli, GenerationTopPMaxMilli)
	}
	switch generation.ReasoningEffort {
	case "", ReasoningEffortOff, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh:
	default:
		return ProviderGenerationProfile{}, fmt.Errorf("unknown reasoning_effort %q", generation.ReasoningEffort)
	}
	switch generation.ToolChoice {
	case "", GenerationToolChoiceAuto:
	default:
		return ProviderGenerationProfile{}, fmt.Errorf("unknown tool_choice %q", generation.ToolChoice)
	}
	return ProviderGenerationProfile{
		MaxOutputTokens:  generation.MaxOutputTokens,
		TemperatureMilli: generation.TemperatureMilli,
		TopPMilli:        generation.TopPMilli,
		ReasoningEffort:  generation.ReasoningEffort,
		ToolChoice:       generation.ToolChoice,
	}, nil
}

// BaseURLIdentityDigest 计算无凭据基地址的身份摘要：scheme 与 host 归一小写、
// 去尾斜杠后保留端口与路径；userinfo、query、fragment 一律拒绝。摘要取代
// 明文地址参与冻结指纹与审计输出，避免端点细节扩散。
func BaseURLIdentityDigest(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base URL must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported base URL scheme %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("base URL must not embed userinfo credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", fmt.Errorf("base URL must not carry query or fragment")
	}
	normalized := url.URL{Scheme: strings.ToLower(parsed.Scheme), Host: strings.ToLower(parsed.Host), Path: parsed.Path}
	return canonicaljson.Fingerprint(baseURLIdentityPrefix, normalized.String())
}

// capabilityFingerprintView 是能力卡参与指纹的闭合投影：排除 Source 等非
// 能力事实字段，只保留协议、工具、usage、上限与停止原因。
type capabilityFingerprintView struct {
	SchemaVersion       int      `json:"schema_version"`
	ProviderKind        string   `json:"provider_kind"`
	Model               string   `json:"model"`
	Protocol            string   `json:"protocol"`
	ToolCalls           bool     `json:"tool_calls"`
	StreamingUsage      bool     `json:"streaming_usage"`
	ContextWindowTokens int      `json:"context_window_tokens,omitempty"`
	MaxOutputTokens     int      `json:"max_output_tokens,omitempty"`
	StopReasons         []string `json:"stop_reasons"`
}

// Fingerprint 以版本化前缀计算能力卡的规范摘要；相同声明的两张卡恒同指纹，
// 能力边界的任何漂移都会改变 EvalSet 冻结身份并被拒绝继续执行。
func (card ProviderCapabilityCard) Fingerprint() (string, error) {
	view := capabilityFingerprintView{
		SchemaVersion: card.SchemaVersion, ProviderKind: card.ProviderKind, Model: card.Model, Protocol: card.Protocol,
		ToolCalls: card.ToolCalls, StreamingUsage: card.StreamingUsage,
		ContextWindowTokens: card.ContextWindowTokens, MaxOutputTokens: card.MaxOutputTokens,
		StopReasons: card.StopReasons,
	}
	return canonicaljson.Fingerprint(capabilityFingerprintPrefix, view)
}

// PricingSnapshot 用整数 micro-USD/M token 表达冻结单价：比较器成本指标与
// EvalSet 定价引用只接受该快照，禁止比较时查询实时价格或把浮点写入指纹。
type PricingSnapshot struct {
	SchemaVersion                       int   `json:"schema_version"`
	InputPriceMicrousdPerMillionTokens  int64 `json:"input_price_microusd_per_million_tokens"`
	OutputPriceMicrousdPerMillionTokens int64 `json:"output_price_microusd_per_million_tokens"`
}

// NewPricingSnapshot 由 config 层的 USD/token 浮点单价构造整数快照。换算经
// 十进制字符串位移完成：当有效小数位超过 exponent、位移除不尽或结果越界时
// 失败关闭，绝不静默舍入——估计值不得冒充冻结单价。
func NewPricingSnapshot(inputUSDPerToken, outputUSDPerToken float64) (PricingSnapshot, error) {
	input, err := priceToMicrousdPerMillion(inputUSDPerToken)
	if err != nil {
		return PricingSnapshot{}, fmt.Errorf("pricing input rate: %w", err)
	}
	output, err := priceToMicrousdPerMillion(outputUSDPerToken)
	if err != nil {
		return PricingSnapshot{}, fmt.Errorf("pricing output rate: %w", err)
	}
	return PricingSnapshot{SchemaVersion: pricingSnapshotSchemaV1, InputPriceMicrousdPerMillionTokens: input, OutputPriceMicrousdPerMillionTokens: output}, nil
}

// priceToMicrousdPerMillion 执行单个单价的十进制位移折算。
func priceToMicrousdPerMillion(usdPerToken float64) (int64, error) {
	if usdPerToken < 0 {
		return 0, errors.New("token price cannot be negative")
	}
	text := strconv.FormatFloat(usdPerToken, 'f', -1, 64)
	integerPart, fractionPart := text, ""
	if dot := strings.IndexByte(text, '.'); dot >= 0 {
		integerPart, fractionPart = text[:dot], text[dot+1:]
	}
	digits := integerPart + fractionPart
	if digits == "" || !allDecimalDigits(digits) {
		return 0, fmt.Errorf("token price %q is not a decimal literal", text)
	}
	numerator, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return 0, fmt.Errorf("token price %q is not a decimal literal", text)
	}
	exponent := microusdPerMillionTokensExponent - len(fractionPart)
	result := new(big.Int)
	if exponent >= 0 {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
		result.Mul(numerator, scale)
	} else {
		// 小数位超过位移量时要求被整除：无法精确折算的单价必须失败关闭，
		// 舍入估计不得冒充冻结单价。
		divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-exponent)), nil)
		if new(big.Int).Mod(numerator, divisor).Sign() != 0 {
			return 0, fmt.Errorf("token price %q has no exact micro-USD representation", text)
		}
		result.Quo(numerator, divisor)
	}
	if !result.IsInt64() || result.Int64() > microusdUpperBound {
		return 0, fmt.Errorf("token price %q overflows the micro-USD snapshot range", text)
	}
	return result.Int64(), nil
}

// allDecimalDigits 报告字符串是否仅由十进制数字组成。
func allDecimalDigits(text string) bool {
	for index := range len(text) {
		if text[index] < '0' || text[index] > '9' {
			return false
		}
	}
	return true
}

// Fingerprint 返回定价快照的规范摘要，供 EvalSet 冻结引用。
func (snapshot PricingSnapshot) Fingerprint() (string, error) {
	return canonicaljson.Fingerprint(pricingFingerprintPrefix, snapshot)
}
