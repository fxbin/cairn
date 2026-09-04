// 本文件实现首跑配置的应用层服务：把模型配置语义（目录预填、零期望实测
// 探测、能力硬性校验、最小 YAML 生成与回读自证）从 CLI 向导提取为纯应用
// 层，供 CLI、TUI 与未来 GUI 复用。密钥永不落盘——服务只生成引用环境变
// 量名的配置；目录与探测依赖可注入，交互问答与文案呈现仍属调用方。
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/adapters/provider/openaicompat"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/modelcatalog"
	"github.com/fxbin/cairn/internal/ports"
)

// setupProbeRequestTimeout 是向导零期望探测的单次请求与总超时。
const setupProbeRequestTimeout = 60 * time.Second

// catalogNamePreviewLimit 是多候选注记最多展示的候选名数量；其余以总数概括。
const catalogNamePreviewLimit = 3

// protocolStopReasons 是 OpenAI 兼容线协议的标准停止原因集；目录不含此维
// 度，由实测探测核对实际观测值。
var protocolStopReasons = []string{contract.StopReasonStop, contract.StopReasonLength, contract.StopReasonToolCalls}

// SetupInput 是向导三要素与目录选项；Workspace 为配置写入目标目录（空=
// 当前目录）。任何要素缺省时由调用方（CLI 问答或 TUI 表单）补齐后再进入
// 本服务。
type SetupInput struct {
	BaseURL         string
	Model           string
	APIKeyEnv       string
	CatalogURL      string
	CatalogCacheDir string
	Workspace       string
	// ConfigVersion 选择生成的配置版本：0 与 1 生成 version 1（历史行为）；
	// 2 生成 version 2 身份配置，此时 ProviderID 与 Admission 必须显式合法。
	ConfigVersion int
	ProviderID    string
	Admission     string
	// OverwriteExisting 允许覆盖既有配置：覆盖前旧文件被原子改名为同目录
	// 的 config.yaml.bak 备份，生成仍走原子写与回读自证。调用方必须先让
	// 用户知情（向导确认页或 CLI 显式旗标），因为覆盖会把手调字段重置为
	// 最小配置。
	OverwriteExisting bool
	// ScopeUserConfig 把写入目标从工作区 .agent/config.yaml 切换到用户级
	// 配置（ADR-0016 回退层）：供"一次配置、多工作区可用"的向导默认路径；
	// 用户级目录不可解析时失败关闭，不回退写工作区。
	ScopeUserConfig bool
}

// SetupOutcome 承载向导的落盘结果与验证结论：全部字段非敏感，可安全进入
// 任何呈现层。
type SetupOutcome struct {
	ConfigPath     string
	APIKeyEnv      string
	CatalogMatched bool
	CatalogName    string
	ProbeRan       bool
	Notes          []string
}

// ProbeObservations 是零期望探测的观测结论：只收集事实，不做合同判定。
type ProbeObservations struct {
	UsageStreamed bool
	StopReason    string
}

// SetupService 承载可注入的外部依赖：目录装载器与端点探测器。默认实现走
// 真实目录与零期望网络探测；测试注入替身后整个向导语义可离线复演。
type SetupService struct {
	// CatalogLoader 装载模型元数据目录；返回错误时向导降级为安全默认。
	CatalogLoader func(options modelcatalog.Options) (*modelcatalog.Catalog, error)
	// EndpointProber 以零期望模式观测 usage 流式与停止原因。
	EndpointProber func(ctx context.Context, baseURL, model, apiKeyEnv string) (*ProbeObservations, error)
}

// NewSetupService 返回使用真实目录装载与零期望探测的服务实例。
func NewSetupService() *SetupService {
	return &SetupService{CatalogLoader: modelcatalog.Load, EndpointProber: probeEndpointBehavior}
}

// probeEndpointBehavior 以零期望 Requirements 向端点发一条极小探测请求，
// 观测 usage 流式与停止原因的真实行为；不做任何合同判定，只收集事实。
func probeEndpointBehavior(ctx context.Context, baseURL, model, apiKeyEnv string) (*ProbeObservations, error) {
	client := openaicompat.Client{
		BaseURL: baseURL, APIKey: os.Getenv(apiKeyEnv),
		HTTPClient:   &http.Client{Timeout: setupProbeRequestTimeout},
		Requirements: openaicompat.Requirements{RequireUsage: false},
	}
	observations := &ProbeObservations{}
	requestCtx, cancel := context.WithTimeout(ctx, setupProbeRequestTimeout)
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

// Run 执行向导主流程：输入校验 → 目录预填 → 可选实测探测 → 硬性能力校验
// → 生成最小 YAML → 落盘 → 回读自证。默认不覆盖既有配置；输入显式允许覆
// 盖时旧文件先原子改名为 config.yaml.bak，再以临时文件与原子重命名写入新
// 配置。任何失败都不留下半成品文件；工具调用与流式 usage 是生产配置的硬
// 性要求，目录或实测给出任一否定即终止且不落盘。
func (service *SetupService) Run(ctx context.Context, input SetupInput) (*SetupOutcome, error) {
	if input.BaseURL == "" || input.Model == "" || input.APIKeyEnv == "" {
		return nil, errors.New("base_url、模型名与密钥环境变量名均不可为空")
	}
	if input.ConfigVersion != 0 && input.ConfigVersion != 1 && input.ConfigVersion != 2 {
		return nil, fmt.Errorf("unsupported config version %d", input.ConfigVersion)
	}
	if input.ConfigVersion == 2 {
		if input.Admission != config.ProviderAdmissionOptional && input.Admission != config.ProviderAdmissionRequired {
			return nil, fmt.Errorf("version 2 配置必须显式声明 admission（%s 或 %s）", config.ProviderAdmissionOptional, config.ProviderAdmissionRequired)
		}
	}
	outcome := &SetupOutcome{APIKeyEnv: input.APIKeyEnv}
	capability := config.ProviderCapabilityConfig{
		ToolCalls: true, StreamingUsage: true,
		StopReasons: append([]string(nil), protocolStopReasons...),
	}
	var pricing *config.PricingConfig

	options := modelcatalog.Options{CatalogURL: input.CatalogURL}
	if input.CatalogCacheDir != "" {
		options.CachePath = input.CatalogCacheDir
	} else if cachePath, err := modelcatalog.DefaultCachePath(); err == nil {
		options.CachePath = cachePath
	}
	if catalog, err := service.CatalogLoader(options); err == nil {
		candidates := catalog.Lookup(input.Model)
		if len(candidates) == 0 {
			outcome.Notes = append(outcome.Notes, "目录未命中该模型，使用安全默认能力声明（上下文/输出上限跳过预算对照）")
		} else {
			if len(candidates) > 1 {
				// 同名模型常被多家网关转售，命中数可能很大；只展示前几个名字
				// 加总数，避免把整行淹没在候选清单里。
				shown := make([]string, 0, catalogNamePreviewLimit)
				for _, entry := range candidates {
					shown = append(shown, entry.Name)
					if len(shown) == catalogNamePreviewLimit {
						break
					}
				}
				outcome.Notes = append(outcome.Notes, fmt.Sprintf("目录命中 %d 个候选（多为网关转售，按精确度排序），取第一个：%s", len(candidates), strings.Join(shown, "、")))
			}
			entry := candidates[0]
			outcome.CatalogMatched = true
			outcome.CatalogName = entry.Name
			capability.ToolCalls = entry.ToolCall
			capability.ContextWindowTokens = entry.ContextTokens
			capability.MaxOutputTokens = entry.OutputTokens
			pricing = &config.PricingConfig{InputTokenUSD: entry.InputUSDPerMillion / 1e6, OutputTokenUSD: entry.OutputUSDPerMillion / 1e6}
			outcome.Notes = append(outcome.Notes, fmt.Sprintf("目录命中: %s（context %d / output %d / 单价 $%.2f|M in, $%.2f|M out——按你的实际账单口径可改）",
				entry.Name, entry.ContextTokens, entry.OutputTokens, entry.InputUSDPerMillion, entry.OutputUSDPerMillion))
		}
	} else {
		outcome.Notes = append(outcome.Notes, fmt.Sprintf("目录不可用（%v），使用安全默认能力声明", err))
	}

	// 实测探测：密钥已在环境时以零期望模式核对 usage 流式与 stop reason。
	if os.Getenv(input.APIKeyEnv) != "" {
		observations, probeErr := service.EndpointProber(ctx, input.BaseURL, input.Model, input.APIKeyEnv)
		if probeErr != nil {
			outcome.Notes = append(outcome.Notes, fmt.Sprintf("探测失败（配置仍按预填生成，建议稍后运行 check-provider 复核）: %v", probeErr))
		} else {
			outcome.ProbeRan = true
			outcome.Notes = append(outcome.Notes, "usage 流式: 实测支持")
			if observations.StopReason != "" && !containsString(capability.StopReasons, observations.StopReason) {
				capability.StopReasons = append(capability.StopReasons, observations.StopReason)
				outcome.Notes = append(outcome.Notes, fmt.Sprintf("停止原因: 实测 %q 不在协议标准集，已并入声明", observations.StopReason))
			} else if observations.StopReason != "" {
				outcome.Notes = append(outcome.Notes, fmt.Sprintf("停止原因: 实测 %q ∈ 协议标准集", observations.StopReason))
			}
		}
	} else {
		outcome.Notes = append(outcome.Notes, "密钥未设置，跳过实测探测——建议 export 后运行 cairn check-provider 验证")
	}
	if !capability.ToolCalls || !capability.StreamingUsage {
		return nil, errors.New("该模型/端点缺少工具调用或流式 usage 能力，不满足 Cairn 生产配置的硬性要求；向导终止且未写入任何文件")
	}

	configPath := config.DefaultPath(input.Workspace)
	if input.ScopeUserConfig {
		userPath, err := config.UserConfigPath()
		if err != nil {
			return nil, fmt.Errorf("解析用户级配置目录失败: %w", err)
		}
		configPath = userPath
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return nil, fmt.Errorf("创建 .agent 目录: %w", err)
	}
	if _, err := os.Stat(configPath); err == nil {
		if !input.OverwriteExisting {
			return nil, fmt.Errorf("配置已存在（%s）；如确认覆盖请重试并显式允许（旧文件会备份为 config.yaml.bak）", configPath)
		}
		if err := os.Rename(configPath, configPath+".bak"); err != nil {
			return nil, fmt.Errorf("备份既有配置失败（未做任何修改）: %w", err)
		}
	}
	temporary := configPath + ".tmp"
	if err := os.WriteFile(temporary, []byte(RenderMinimalConfig(input, capability, pricing)), 0o600); err != nil {
		return nil, fmt.Errorf("写入配置: %w", err)
	}
	if err := os.Rename(temporary, configPath); err != nil {
		return nil, fmt.Errorf("落盘配置: %w", err)
	}
	outcome.ConfigPath = configPath

	// 回读自证：生成的文件必须能严格装载且能力卡与预期一致。
	loaded, err := config.Load(configPath, config.Overrides{})
	if err != nil {
		os.Remove(configPath)
		return nil, fmt.Errorf("生成的配置无法装载（已删除）: %w", err)
	}
	if loaded.Provider.Capability == nil {
		os.Remove(configPath)
		return nil, errors.New("生成的配置缺少能力声明（已删除，请报告此缺陷）")
	}
	if loaded.Provider.Capability.ToolCalls != capability.ToolCalls || loaded.Provider.Capability.StreamingUsage != capability.StreamingUsage {
		os.Remove(configPath)
		return nil, errors.New("生成的配置能力卡与预期不符（已删除，请报告此缺陷）")
	}
	return outcome, nil
}

// RenderMinimalConfig 生成带中文注释的最小配置文本：只写 provider 段（与
// 命中目录时的 pricing 段），其余全部走安全默认值；密钥只以环境变量名引用。
func RenderMinimalConfig(input SetupInput, capability config.ProviderCapabilityConfig, pricing *config.PricingConfig) string {
	var builder strings.Builder
	builder.WriteString("# Cairn 配置——由 cairn init/provider configure 生成；只写了 Provider 段，其余全部使用安全默认值。\n")
	builder.WriteString("# 模型密钥只从环境变量读取，绝不写入本文件。\n")
	if input.ConfigVersion == 2 {
		builder.WriteString("version: 2\n\n")
	} else {
		builder.WriteString("version: 1\n\n")
	}
	builder.WriteString("provider:\n")
	if input.ConfigVersion == 2 {
		providerID := input.ProviderID
		if providerID == "" {
			providerID = contract.ProviderIDCustom
		}
		fmt.Fprintf(&builder, "  id: %s\n", providerID)
		fmt.Fprintf(&builder, "  admission: %s\n", input.Admission)
	}
	fmt.Fprintf(&builder, "  base_url: %s\n", input.BaseURL)
	fmt.Fprintf(&builder, "  model: %s\n", input.Model)
	fmt.Fprintf(&builder, "  api_key_env: %s\n", input.APIKeyEnv)
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
		builder.WriteString("\n# 定价（每 token 的美元单价，已由目录每百万单价换算；按实际账单口径修改）\n")
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
