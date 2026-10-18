// Package provider 实现 Provider 身份分层：静态注册表、历史 ProviderKind 与
// 规范 ProtocolID 的闭合映射（D1），以及 ProviderSelection 的派生解析。本包
// 不发起网络请求、不持有凭据、不做目录扫描或动态发现；身份事实全部由调用
// 方显式传入，未知成员或非法形态一律失败关闭。
//
// Author: fxbin
package provider

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/contract"
)

// providerSelectionSchemaVersion 是本包产出的 ProviderSelection 合同版本。
const providerSelectionSchemaVersion = 1

// providerIDMaxLen 限定 ProviderID 的最长字符数，防止无界标识进入指纹与审计。
const providerIDMaxLen = 64

// staticDefinitions 是编译期冻结的静态注册表：只注册当前确有需求的定义，
// 不扫描目录、不支持运行期注册。custom 语义即"用户显式 base_url + model +
// api_key_env"，与其余具名成员共享同一规范协议。
var staticDefinitions = map[string]contract.ProviderDefinition{
	contract.ProviderIDCustom: {ProviderID: contract.ProviderIDCustom, ProtocolID: contract.ProtocolOpenAIChatCompletions},
}

// ProtocolFromKind 把历史 ProviderKind 映射为规范 ProtocolID；映射是闭合
// 函数，未知值失败关闭，永不重新解释历史字面值。
func ProtocolFromKind(kind string) (string, error) {
	switch kind {
	case contract.ProviderKindOpenAICompatible:
		return contract.ProtocolOpenAIChatCompletions, nil
	case contract.ProviderKindAnthropicMessages:
		return contract.ProtocolAnthropicMessages, nil
	case contract.ProviderKindOpenAIResponses:
		return contract.ProtocolOpenAIResponses, nil
	default:
		return "", fmt.Errorf("provider kind %q has no protocol mapping", kind)
	}
}

// KindFromProtocol 把规范 ProtocolID 反向映射为历史 ProviderKind；供事件
// payload 写入等只允许历史词表的边界使用，未知值失败关闭。
func KindFromProtocol(protocolID string) (string, error) {
	switch protocolID {
	case contract.ProtocolOpenAIChatCompletions:
		return contract.ProviderKindOpenAICompatible, nil
	case contract.ProtocolAnthropicMessages:
		return contract.ProviderKindAnthropicMessages, nil
	case contract.ProtocolOpenAIResponses:
		return contract.ProviderKindOpenAIResponses, nil
	default:
		return "", fmt.Errorf("protocol %q has no provider kind mapping", protocolID)
	}
}

// Lookup 返回静态注册表中的 ProviderDefinition；未知 ProviderID 失败关闭，
// 不存在隐式回退到 custom。
func Lookup(providerID string) (contract.ProviderDefinition, error) {
	if err := ValidateProviderID(providerID); err != nil {
		return contract.ProviderDefinition{}, err
	}
	definition, ok := staticDefinitions[providerID]
	if !ok {
		return contract.ProviderDefinition{}, fmt.Errorf("provider %q is not registered", providerID)
	}
	return definition, nil
}

// ValidateProviderID 校验 ProviderID 的确定性形状：小写 kebab
// （[a-z][a-z0-9-]*）且不超过上限。空值用于缺省归一路径、由调用方先处理。
// ProviderID 非敏感，错误文本允许回显值本身以便定位配置错误；config 层在
// version 2 校验中复用本函数，保证注册表与配置层形状规则单一来源。
func ValidateProviderID(providerID string) error {
	if providerID == "" {
		return errors.New("provider id must not be empty")
	}
	if len(providerID) > providerIDMaxLen {
		return fmt.Errorf("provider id exceeds %d characters", providerIDMaxLen)
	}
	for index := 0; index < len(providerID); index++ {
		character := providerID[index]
		lower := character >= 'a' && character <= 'z'
		digit := index > 0 && character >= '0' && character <= '9'
		dash := index > 0 && character == '-'
		if !lower && !digit && !dash {
			return fmt.Errorf("provider id %q must be lowercase kebab: [a-z][a-z0-9-]*", providerID)
		}
	}
	return nil
}

// SelectionFacts 是解析 ProviderSelection 所需的全部显式身份事实：ProviderID
// 缺省按 custom 语义处理；Model 与 BaseURL 由调用方保证已通过配置校验。
type SelectionFacts struct {
	ProviderID string
	Model      string
	BaseURL    string
}

// ResolveSelection 由身份事实派生 ProviderSelection：协议来自静态注册表而
// 不是配置输入；model_ref 是派生值（provider_id + "/" + model，解析按第一个
// 斜杠分割）；endpoint_digest 复用无凭据规范化基地址摘要。任何事实缺失或
// 非法都在此失败关闭。
func ResolveSelection(facts SelectionFacts) (contract.ProviderSelection, error) {
	providerID := facts.ProviderID
	if providerID == "" {
		providerID = contract.ProviderIDCustom
	}
	definition, err := Lookup(providerID)
	if err != nil {
		return contract.ProviderSelection{}, err
	}
	if facts.Model == "" {
		return contract.ProviderSelection{}, errors.New("provider selection requires a model")
	}
	digest, err := contract.BaseURLIdentityDigest(facts.BaseURL)
	if err != nil {
		return contract.ProviderSelection{}, err
	}
	return contract.ProviderSelection{
		SchemaVersion:  providerSelectionSchemaVersion,
		ProviderID:     providerID,
		ProtocolID:     definition.ProtocolID,
		ModelRef:       providerID + "/" + facts.Model,
		EndpointDigest: digest,
	}, nil
}
