// Provider 构造工厂：按协议装配适配器与能力卡。
// Author: fxbin
package app

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/fxbin/cairn/internal/adapters/provider/anthropicmessages"
	"github.com/fxbin/cairn/internal/adapters/provider/geminigen"
	"github.com/fxbin/cairn/internal/adapters/provider/openaicompat"
	"github.com/fxbin/cairn/internal/adapters/provider/openairesponses"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/provider"
)

// 时失败关闭校验；显式生成参数原样透传，不在此层引入默认值。
func (service Service) provider(configuration config.Config, workspacePath string) (ports.Provider, error) {
	if service.NewProvider != nil {
		return service.NewProvider(configuration)
	}
	if configuration.Provider.BaseURL == "" || configuration.Provider.Model == "" {
		return nil, errors.New("provider base URL and model are required")
	}
	apiKey, err := providerCredential(workspacePath, configuration.Provider.APIKeyEnv)
	if err != nil {
		return nil, err
	}
	timeout, err := configuration.RequestTimeout()
	if err != nil {
		return nil, err
	}
	protocol := configuration.Provider.Protocol
	if protocol == "" {
		protocol = config.ProtocolOpenAIChatCompletions
	}
	card, err := resolveProviderCardForProtocol(protocol, configuration.Provider.Model, config.ProviderCapabilityDeclaration(configuration))
	if err != nil {
		return nil, fmt.Errorf("resolve provider card for stream requirements: %w", err)
	}
	// 协议静态分派：未知协议失败关闭，不做自动降级或故障转移。
	switch protocol {
	case config.ProtocolOpenAIChatCompletions:
		client := openaicompat.Client{
			BaseURL: configuration.Provider.BaseURL, APIKey: apiKey,
			HTTPClient: &http.Client{Timeout: timeout},
			Requirements: openaicompat.Requirements{
				RequireUsage:       card.StreamingUsage,
				AllowedStopReasons: append([]string(nil), card.StopReasons...),
			},
		}
		if generation := configuration.Provider.Generation; generation != nil {
			client.Generation = &openaicompat.GenerationOptions{
				TemperatureMilli: generation.TemperatureMilli,
				TopPMilli:        generation.TopPMilli, ReasoningEffort: generation.ReasoningEffort, ToolChoice: generation.ToolChoice,
			}
			if generation.MaxOutputTokens != nil {
				client.Generation.MaxOutputTokens = *generation.MaxOutputTokens
			}
		}
		return client, nil
	case config.ProtocolAnthropicMessages:
		client := anthropicmessages.Client{
			BaseURL: configuration.Provider.BaseURL, APIKey: apiKey,
			HTTPClient:   &http.Client{Timeout: timeout},
			Requirements: anthropicmessages.Requirements{RequireUsage: card.StreamingUsage, AllowedStopReasons: append([]string(nil), card.StopReasons...)},
		}
		if generation := configuration.Provider.Generation; generation != nil {
			if generation.MaxOutputTokens != nil {
				client.MaxTokens = *generation.MaxOutputTokens
			}
			client.TemperatureMilli = generation.TemperatureMilli
			client.TopPMilli = generation.TopPMilli
			client.ToolChoice = generation.ToolChoice
		}
		return client, nil
	case config.ProtocolOpenAIResponses:
		client := openairesponses.Client{
			BaseURL: configuration.Provider.BaseURL, APIKey: apiKey,
			HTTPClient:   &http.Client{Timeout: timeout},
			Requirements: openairesponses.Requirements{RequireUsage: card.StreamingUsage, AllowedStopReasons: append([]string(nil), card.StopReasons...)},
		}
		if generation := configuration.Provider.Generation; generation != nil {
			if generation.MaxOutputTokens != nil {
				client.MaxOutputTokens = *generation.MaxOutputTokens
			}
			client.TemperatureMilli = generation.TemperatureMilli
			client.TopPMilli = generation.TopPMilli
			client.ToolChoice = generation.ToolChoice
		}
		return client, nil
	case config.ProtocolGeminiGenerate:
		client := geminigen.Client{
			BaseURL: configuration.Provider.BaseURL, APIKey: apiKey,
			HTTPClient:   &http.Client{Timeout: timeout},
			Requirements: geminigen.Requirements{RequireUsage: card.StreamingUsage, AllowedStopReasons: append([]string(nil), card.StopReasons...)},
		}
		if generation := configuration.Provider.Generation; generation != nil {
			if generation.MaxOutputTokens != nil {
				client.MaxOutputTokens = *generation.MaxOutputTokens
			}
			client.TemperatureMilli = generation.TemperatureMilli
			client.TopPMilli = generation.TopPMilli
		}
		return client, nil
	default:
		return nil, fmt.Errorf("provider protocol %q is not supported", protocol)
	}
}

// resolveProviderCardForProtocol 按规范协议标识解析能力卡；协议与历史
// ProviderKind 的映射闭合，未知协议失败关闭。
func resolveProviderCardForProtocol(protocol, model string, declared *contract.ProviderCapabilityDeclaration) (contract.ProviderCapabilityCard, error) {
	kind, err := provider.KindFromProtocol(protocol)
	if err != nil {
		return contract.ProviderCapabilityCard{}, err
	}
	return contract.ResolveProviderCard(kind, model, declared)
}

// providerKindForProtocol 把 effective protocol 映射为历史 ProviderKind；
// 缺省协议回退 Chat Completions，未知协议失败关闭。
func providerKindForProtocol(protocol string) string {
	if protocol == "" {
		protocol = config.ProtocolOpenAIChatCompletions
	}
	kind, err := provider.KindFromProtocol(protocol)
	if err != nil {
		return ""
	}
	return kind
}
