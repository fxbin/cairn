// 本文件承载首跑配置向导：以最少问答生成一份可直接运行的最小配置，用外
// 部模型元数据目录预填能力卡与预算上限，并在环境变量就绪时用零期望实测
// 探测对账修正。密钥永不落盘——向导只生成引用环境变量名的配置。
//
// Author: fxbin
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/adapters/provider/openaicompat"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/modelcatalog"
	"github.com/fxbin/cairn/internal/ports"
)

// 协议标准停止原因集：OpenAI 兼容线协议的闭合词表；目录不含此维度，由实
// 测探测核对实际观测值。
var protocolStopReasons = []string{"stop", "length", "tool_calls"}

// defaultProviderKeyEnv 是向导的密钥环境变量名默认值：语义中性，不预设厂
// 商；用户可改。
const defaultProviderKeyEnv = "CAIRN_PROVIDER_KEY"

// initOutcome 承载向导的落盘结果与验证结论。
type initOutcome struct {
	ConfigPath     string
	APIKeyEnv      string
	CatalogMatched bool
	CatalogName    string
	ProbeRan       bool
	ProbeNotes     []string
}

// initFlags 收集向导输入的三要素；任何一项缺省时进入交互问答。
type initFlags struct {
	BaseURL    string
	Model      string
	APIKeyEnv  string
	CatalogURL string
}

// interactivePrompt 在 stdin 上读取一行输入；空输入回车接受默认值。
func interactivePrompt(reader *bufio.Reader, writer io.Writer, label, defaultValue string) string {
	if defaultValue != "" {
		fmt.Fprintf(writer, "%s [%s]: ", label, defaultValue)
	} else {
		fmt.Fprintf(writer, "%s: ", label)
	}
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return defaultValue
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return defaultValue
	}
	return answer
}

// runInit 执行向导主流程：三要素输入 → 目录预填 → 可选实测探测 → 生成
// 最小 YAML → 回读自证。
func (command Command) runInit(ctx context.Context, flags initFlags) (*initOutcome, error) {
	reader := bufio.NewReader(os.Stdin)
	if flags.BaseURL == "" {
		flags.BaseURL = interactivePrompt(reader, command.Stdout, "Provider Base URL", "")
	}
	if flags.Model == "" {
		flags.Model = interactivePrompt(reader, command.Stdout, "模型名（如 glm-5.3-flash）", "")
	}
	if flags.APIKeyEnv == "" {
		flags.APIKeyEnv = interactivePrompt(reader, command.Stdout, "API Key 环境变量名", defaultProviderKeyEnv)
	}
	if flags.BaseURL == "" || flags.Model == "" || flags.APIKeyEnv == "" {
		return nil, fmt.Errorf("base_url、模型名与密钥环境变量名均不可为空")
	}

	outcome := &initOutcome{APIKeyEnv: flags.APIKeyEnv}
	capability := config.ProviderCapabilityConfig{
		ToolCalls: true, StreamingUsage: true,
		StopReasons: append([]string(nil), protocolStopReasons...),
	}
	var pricing *config.PricingConfig

	// 目录预填：命中则以上限与单价充实能力卡；未命中走安全默认（上限零=
	// 预算对照跳过）。缓存默认落用户缓存目录，弱网时降级陈旧缓存。
	catalogOptions := modelcatalog.Options{CatalogURL: flags.CatalogURL}
	if cacheDir, err := os.UserCacheDir(); err == nil {
		catalogOptions.CachePath = filepath.Join(cacheDir, "cairn", "models-catalog.json")
	}
	if catalog, err := modelcatalog.Load(catalogOptions); err == nil {
		candidates := catalog.Lookup(flags.Model)
		if len(candidates) == 1 {
			entry := candidates[0]
			outcome.CatalogMatched = true
			outcome.CatalogName = entry.Name
			capability.ToolCalls = entry.ToolCall
			capability.ContextWindowTokens = entry.ContextTokens
			capability.MaxOutputTokens = entry.OutputTokens
			pricing = &config.PricingConfig{InputTokenUSD: entry.InputUSDPerMillion / 1e6, OutputTokenUSD: entry.OutputUSDPerMillion / 1e6}
			fmt.Fprintf(command.Stdout, "目录命中: %s（context %d / output %d / 单价 $%.2f|M in, $%.2f|M out——按你的实际账单口径可改）\n",
				entry.Name, entry.ContextTokens, entry.OutputTokens, entry.InputUSDPerMillion, entry.OutputUSDPerMillion)
		} else if len(candidates) > 1 {
			fmt.Fprintf(command.Stdout, "目录命中 %d 个候选，取名称完全一致者或第一个：", len(candidates))
			for _, entry := range candidates {
				fmt.Fprintf(command.Stdout, " %s", entry.Name)
			}
			fmt.Fprintln(command.Stdout)
			entry := candidates[0]
			outcome.CatalogMatched = true
			outcome.CatalogName = entry.Name
			capability.ToolCalls = entry.ToolCall
			capability.ContextWindowTokens = entry.ContextTokens
			capability.MaxOutputTokens = entry.OutputTokens
			pricing = &config.PricingConfig{InputTokenUSD: entry.InputUSDPerMillion / 1e6, OutputTokenUSD: entry.OutputUSDPerMillion / 1e6}
		} else {
			fmt.Fprintln(command.Stdout, "目录未命中该模型，使用安全默认能力声明（上下文/输出上限跳过预算对照）")
		}
	} else {
		fmt.Fprintf(command.Stdout, "目录不可用（%v），使用安全默认能力声明\n", err)
	}

	// 实测探测：密钥已在环境时以零期望模式核对 usage 流式与 stop reason，
	// 修正预填。密钥未设则跳过，落盘后提示 check-provider 验证。
	if os.Getenv(flags.APIKeyEnv) != "" {
		observations, probeErr := probeEndpointBehavior(ctx, flags.BaseURL, flags.Model, flags.APIKeyEnv)
		if probeErr != nil {
			outcome.ProbeNotes = append(outcome.ProbeNotes, fmt.Sprintf("探测失败（配置仍按预填生成，建议稍后运行 check-provider 复核）: %v", probeErr))
		} else {
			outcome.ProbeRan = true
			if observations.UsageStreamed {
				outcome.ProbeNotes = append(outcome.ProbeNotes, "usage 流式: 实测支持")
			} else {
				capability.StreamingUsage = false
				outcome.ProbeNotes = append(outcome.ProbeNotes, "usage 流式: 实测未见（能力卡已按实测修正为 false）")
			}
			if observations.StopReason != "" && !containsString(protocolStopReasons, observations.StopReason) {
				capability.StopReasons = append(capability.StopReasons, observations.StopReason)
				outcome.ProbeNotes = append(outcome.ProbeNotes, fmt.Sprintf("停止原因: 实测 %q 不在协议标准集，已并入声明", observations.StopReason))
			} else if observations.StopReason != "" {
				outcome.ProbeNotes = append(outcome.ProbeNotes, fmt.Sprintf("停止原因: 实测 %q ∈ 协议标准集", observations.StopReason))
			}
		}
	} else {
		outcome.ProbeNotes = append(outcome.ProbeNotes, "密钥未设置，跳过实测探测——建议 export 后运行 cairn check-provider 验证")
	}

	// 生成最小 YAML：只写 provider 段，其余全部走默认值。
	if err := os.MkdirAll(filepath.Dir(config.DefaultPath(".")), 0o700); err != nil {
		return nil, fmt.Errorf("创建 .agent 目录: %w", err)
	}
	configPath := config.DefaultPath(".")
	if _, err := os.Stat(configPath); err == nil {
		return nil, fmt.Errorf("配置已存在（%s）；向导不覆盖既有配置", configPath)
	}
	yamlContent := renderMinimalConfig(flags, capability, pricing)
	if err := os.WriteFile(configPath, []byte(yamlContent), 0o600); err != nil {
		return nil, fmt.Errorf("写入配置: %w", err)
	}
	outcome.ConfigPath = configPath

	// 回读自证：生成的文件必须能严格装载且能力卡可解析。
	loaded, err := config.Load(configPath, config.Overrides{})
	if err != nil {
		return nil, fmt.Errorf("生成的配置无法装载（即将删除）: %w", err)
	}
	if loaded.Provider.Capability == nil {
		os.Remove(configPath)
		return nil, fmt.Errorf("生成的配置缺少能力声明（已删除，请报告此缺陷）")
	}
	if loaded.Provider.Capability.ToolCalls != capability.ToolCalls || loaded.Provider.Capability.StreamingUsage != capability.StreamingUsage {
		os.Remove(configPath)
		return nil, fmt.Errorf("生成的配置能力卡与预期不符（已删除，请报告此缺陷）")
	}
	return outcome, nil
}

// probeObservations 是零期望探测的观测结论。
type probeObservations struct {
	UsageStreamed bool
	StopReason    string
}

// probeEndpointBehavior 以零期望 Requirements 向端点发一条极小探测请求，
// 观测 usage 流式与停止原因的真实行为；不做任何合同判定，只收集事实。
func probeEndpointBehavior(ctx context.Context, baseURL, model, apiKeyEnv string) (*probeObservations, error) {
	client := openaicompat.Client{
		BaseURL: baseURL, APIKey: os.Getenv(apiKeyEnv),
		HTTPClient:   &http.Client{Timeout: 60 * time.Second},
		Requirements: openaicompat.Requirements{RequireUsage: false},
	}
	observations := &probeObservations{}
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err := client.Stream(requestCtx, ports.ModelRequest{Model: model, Messages: []ports.Message{{Role: "user", Content: "Reply with the single word: ok."}}},
		func(event ports.ModelEvent) error {
			switch event.Kind {
			case ports.UsageEvent:
				observations.UsageStreamed = true
			case ports.Done:
				observations.StopReason = event.StopReason
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return observations, nil
}

// renderMinimalConfig 生成带中文注释的最小配置文本。
func renderMinimalConfig(flags initFlags, capability config.ProviderCapabilityConfig, pricing *config.PricingConfig) string {
	var builder strings.Builder
	builder.WriteString("# Cairn 配置——由 cairn init 生成；只写了 Provider 段，其余全部使用安全默认值。\n")
	builder.WriteString("# 模型密钥只从环境变量读取，绝不写入本文件。\n")
	builder.WriteString("version: 1\n\n")
	builder.WriteString("provider:\n")
	fmt.Fprintf(&builder, "  base_url: %s\n", flags.BaseURL)
	fmt.Fprintf(&builder, "  model: %s\n", flags.Model)
	fmt.Fprintf(&builder, "  api_key_env: %s\n", flags.APIKeyEnv)
	builder.WriteString("  request_timeout: 120s\n")
	builder.WriteString("  capability:\n")
	fmt.Fprintf(&builder, "    tool_calls: %t\n", capability.ToolCalls)
	fmt.Fprintf(&builder, "    streaming_usage: %t\n", capability.StreamingUsage)
	if capability.ContextWindowTokens > 0 {
		fmt.Fprintf(&builder, "    context_window_tokens: %d\n", capability.ContextWindowTokens)
	}
	if capability.MaxOutputTokens > 0 {
		fmt.Fprintf(&builder, "    max_output_tokens: %d\n", capability.MaxOutputTokens)
	}
	builder.WriteString("    stop_reasons: [")
	for index, reason := range capability.StopReasons {
		if index > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(reason)
	}
	builder.WriteString("]\n")
	if pricing != nil {
		builder.WriteString("\n# 定价（每百万 token 的美元单价；目录预填，按实际账单口径修改）\n")
		builder.WriteString("pricing:\n")
		fmt.Fprintf(&builder, "  input_token_usd: %.10g\n", pricing.InputTokenUSD)
		fmt.Fprintf(&builder, "  output_token_usd: %.10g\n", pricing.OutputTokenUSD)
	}
	return builder.String()
}

// containsString 判断切片是否含指定字符串。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
