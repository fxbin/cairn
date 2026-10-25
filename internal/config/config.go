// Package config 读取 Cairn 小型、不可包含密钥的仓库配置。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/userdir"
)

const (
	// CurrentVersion 是当前唯一受支持的 Agent 配置合同版本。
	CurrentVersion = 2
	// DefaultMaxTurns 是普通任务的默认模型轮次硬上限。
	DefaultMaxTurns = 40
	// ComplexMaxTurns 是复杂任务建议显式选择的模型轮次档位；更高上限仍由场景配置决定。
	ComplexMaxTurns = 80
)

// Config 汇集一次 Cairn 运行的全部非敏感配置。
type Config struct {
	Version   int             `yaml:"version"`
	Profile   string          `yaml:"profile"`
	Preset    string          `yaml:"preset"`
	Provider  ProviderConfig  `yaml:"provider"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Policy    PolicyConfig    `yaml:"policy"`
	Tools     ToolsConfig     `yaml:"tools"`
	Hooks     []HookConfig    `yaml:"hooks,omitempty"`
	Prompt    PromptConfig    `yaml:"prompt,omitempty"`
	// Guard 覆盖循环熔断阈值；缺省使用默认值且不改变指纹。
	Guard *GuardConfig `yaml:"guard,omitempty"`
	// ActiveMode 选择 modes 中的命名装配块；空值表示不套用 mode。
	ActiveMode string `yaml:"active_mode,omitempty"`
	// Modes 是命名配置块；字段形状与主配置子集兼容，合并时显式 YAML 优先。
	Modes        map[string]yaml.Node `yaml:"modes,omitempty"`
	Verification VerificationConfig   `yaml:"verification"`
	Budget       BudgetConfig         `yaml:"budget"`
	Context      ContextConfig        `yaml:"context"`
	Pricing      PricingConfig        `yaml:"pricing"`
	Output       OutputConfig         `yaml:"output"`
	Cancellation CancellationConfig   `yaml:"cancellation"`
	Sandbox      SandboxConfig        `yaml:"sandbox"`
	MCP          *MCPConfig           `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	Skills       *SkillsConfig        `yaml:"skills,omitempty" json:"skills,omitempty"`
	// Resolution 是解析过程生成的非敏感 Resolved Execution Plan（字段→值→来源），
	// 只读派生事实：不参与 YAML 解码、不进入配置指纹（语义已由解析后的字段承载），
	// 随冻结配置制品持久化，供诊断与可访问性视图使用。
	Resolution []ResolutionEntry `yaml:"-" json:"resolution,omitempty"`
}

// ResolutionEntry 是 Resolved Execution Plan 的单条记录：关键字段的最终值与其
// 来源（default、yaml、profile:<name>、env 或 cli）。
type ResolutionEntry struct {
	Field  string `json:"field"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// ProfileDefault 与 ProfileComplex 是当前版本化的命名 Execution Profile；空值
// 等价 default。Profile 只预设预算字段，不自动推断任务复杂度。
const (
	ProfileDefault = "default"
	ProfileComplex = "complex"
)

// 静态 Preset 是编译期已知组件的能力组合：工具子集、策略模式与验证要求。
// 不存在目录扫描、自注册或动态加载；未知 Preset 在校验层失败关闭。
const (
	PresetCoding    = "coding"
	PresetReadOnly  = "read-only"
	PresetBenchmark = "benchmark"
)

// resolveProviderConnection 解析 provider.use 与 presets：use 设置时必须命中
// 已命名预设且不得与内联连接字段并存，预设投影为有效连接；协议字段缺省
// openai-chat-completions，未知协议失败关闭。调用方在 Validate 前执行，
// 使指纹、准入与客户端装配读取同一有效连接。overlay 提供 YAML 显式在场
// 事实，避免 Defaults 预填误判互斥。
func WithPresetResolution(configuration Config, preset, source string) Config {
	configuration.Resolution = append([]ResolutionEntry(nil), configuration.Resolution...)
	recorder := resolutionRecorder{entries: configuration.Resolution}
	recorder.record("preset", preset, source)
	configuration.Resolution = recorder.entries
	return configuration
}

// budgetSourceLabel 返回预算字段在某 Profile 下的来源标签。
func budgetSourceLabel(profile string) string {
	if profile == "" || profile == ProfileDefault {
		return "default"
	}
	return "profile:" + profile
}

// ProviderAdmissionOptional 与 ProviderAdmissionRequired 是 Provider 准入
// 语义的显式取值：optional 表示门不拦截，required 表示启动前必须命中该绑定
// 的唯一准入链头。配置装载默认使用 required，只有显式配置才允许 optional。
const (
	ProviderAdmissionOptional = "optional"
	ProviderAdmissionRequired = "required"
)

// ProviderConfig 汇集提供方连接、身份与准入事实。Protocol 绑定静态协议
// 标识（缺省 openai-chat-completions）；Use 选择 Presets 中的已命名连接预设，
// 设置时预设投影为有效连接，与内联连接字段互斥。保留单 provider 内联块
// 可读：未设置 Use 时行为与旧配置完全一致。
type ProviderConfig struct {
	BaseURL        string                    `yaml:"base_url"`
	Model          string                    `yaml:"model"`
	APIKeyEnv      string                    `yaml:"api_key_env"`
	RequestTimeout string                    `yaml:"request_timeout"`
	ID             string                    `yaml:"id"`
	Admission      string                    `yaml:"admission"`
	Protocol       string                    `yaml:"protocol"`
	Use            string                    `yaml:"use"`
	Presets        map[string]ProviderPreset `yaml:"presets"`
	Capability     *ProviderCapabilityConfig `yaml:"capability"`
	Generation     *ProviderGenerationConfig `yaml:"generation"`
}

// ProviderPreset 是一份可复用的已命名连接预设；字段语义与内联 ProviderConfig
// 连接段一致，Protocol 缺省时沿用 openai-chat-completions。
type ProviderPreset struct {
	Protocol       string                    `yaml:"protocol"`
	BaseURL        string                    `yaml:"base_url"`
	Model          string                    `yaml:"model"`
	APIKeyEnv      string                    `yaml:"api_key_env"`
	RequestTimeout string                    `yaml:"request_timeout"`
	ID             string                    `yaml:"id"`
	Admission      string                    `yaml:"admission"`
	Capability     *ProviderCapabilityConfig `yaml:"capability"`
	Generation     *ProviderGenerationConfig `yaml:"generation"`
}

// ProtocolOpenAIChatCompletions 是配置层缺省协议标识，与 contract 常量同值。
const ProtocolOpenAIChatCompletions = "openai-chat-completions"

// ProtocolAnthropicMessages 是 Anthropic Messages 协议标识。
const ProtocolAnthropicMessages = "anthropic-messages"

// ProtocolOpenAIResponses 是 OpenAI Responses 协议标识。
const ProtocolOpenAIResponses = "openai-responses"

// ProtocolGeminiGenerate 是 Gemini GenerateContent 协议标识。
const ProtocolGeminiGenerate = "gemini-generate-content"

// supportedProviderProtocols 是配置层接受的协议闭合集合。
var supportedProviderProtocols = map[string]struct{}{
	ProtocolOpenAIChatCompletions: {},
	ProtocolAnthropicMessages:     {},
	ProtocolOpenAIResponses:       {},
	ProtocolGeminiGenerate:        {},
}

// supportedProviderProtocolList 返回受支持协议的稳定排序文案，供错误信息使用。
func supportedProviderProtocolList() string {
	return ProtocolOpenAIChatCompletions + ", " + ProtocolAnthropicMessages + ", " + ProtocolOpenAIResponses + ", " + ProtocolGeminiGenerate
}

// ProviderGenerationConfig 是 provider.generation 显式生成参数块：每个字段
// 只有显式值或未声明两态，显式值必须原样序列化进模型请求，未声明字段在
// Resolved Execution Plan 与指纹投影中记录为 unsupported，不允许静默采用
// 提供方默认值。 Milli 后缀表示千分位整数（指纹禁浮点）；输出上限采用指针
// 使显式写入的 0 可被失败关闭识别，而不是被误当作未声明。该块是冻结评测
// 资产属性，只来自 YAML 显式值，不提供环境变量或 CLI 覆盖。
// EffectiveThinkingEffort 返回归一化的思考力度。
func (generation ProviderGenerationConfig) EffectiveThinkingEffort() ThinkingEffort {
	return NormalizeThinkingEffort(generation.ReasoningEffort)
}

type ProviderGenerationConfig struct {
	MaxOutputTokens  *int   `yaml:"max_output_tokens"`
	TemperatureMilli *int   `yaml:"temperature_milli"`
	TopPMilli        *int   `yaml:"top_p_milli"`
	ReasoningEffort  string `yaml:"reasoning_effort"`
	ToolChoice       string `yaml:"tool_choice"`
}

// ProviderCapabilityDeclaration 把配置层能力声明投影为合同层声明输入；所有
// 运行、探测与冻结入口必须复用此函数，避免能力画像出现多份映射规则。
func ProviderCapabilityDeclaration(configuration Config) *contract.ProviderCapabilityDeclaration {
	capability := configuration.Provider.Capability
	if capability == nil {
		return nil
	}
	return &contract.ProviderCapabilityDeclaration{
		ToolCalls: capability.ToolCalls, StreamingUsage: capability.StreamingUsage,
		ContextWindowTokens: capability.ContextWindowTokens, MaxOutputTokens: capability.MaxOutputTokens,
		StopReasons: append([]string(nil), capability.StopReasons...),
	}
}

// ProviderGenerationDeclaration 把显式生成参数投影为合同层声明输入；缺席块
// 返回 nil，显式零值与 unsupported 仍由指针形态精确区分。
func ProviderGenerationDeclaration(configuration Config) *contract.ProviderGenerationDeclaration {
	generation := configuration.Provider.Generation
	if generation == nil {
		return nil
	}
	declaration := &contract.ProviderGenerationDeclaration{
		TemperatureMilli: generation.TemperatureMilli,
		TopPMilli:        generation.TopPMilli, ReasoningEffort: generation.ReasoningEffort, ToolChoice: generation.ToolChoice,
	}
	if generation.MaxOutputTokens != nil {
		declaration.MaxOutputTokens = *generation.MaxOutputTokens
	}
	return declaration
}

// ProviderCapabilityConfig 是 provider.capability 显式能力声明块：
// 为所用模型声明工具调用、流式 usage 与可选的上下文/输出上限；能力事实
// 只来自配置，编译期不携带模型目录。
// ToolCalls 与 StreamingUsage 必须显式为 true——Cairn 唯一生产路径依赖两者，
// 省缺即假值会在配置校验时失败关闭；上限 0 表示未声明。
type ProviderCapabilityConfig struct {
	ToolCalls           bool     `yaml:"tool_calls"`
	StreamingUsage      bool     `yaml:"streaming_usage"`
	ContextWindowTokens int      `yaml:"context_window_tokens"`
	MaxOutputTokens     int      `yaml:"max_output_tokens"`
	StopReasons         []string `yaml:"stop_reasons"`
}

// WorkspaceConfig 限制模型工具可读取和检索的工作区范围。
type WorkspaceConfig struct {
	Ignore         []string `yaml:"ignore"`
	MaxReadBytes   int64    `yaml:"max_read_bytes"`
	MaxSearchFiles int      `yaml:"max_search_files"`
	MaxSearchBytes int64    `yaml:"max_search_bytes"`
}

// BudgetConfig 定义运行在轮次、工具、token、时间和成本上的硬上限。
type BudgetConfig struct {
	MaxTurns        int     `yaml:"max_turns"`
	MaxToolCalls    int     `yaml:"max_tool_calls"`
	MaxInputTokens  int     `yaml:"max_input_tokens"`
	MaxOutputTokens int     `yaml:"max_output_tokens"`
	MaxWallTime     string  `yaml:"max_wall_time"`
	MaxCostUSD      float64 `yaml:"max_cost_usd"`
}

// ContextConfig 限制单次模型请求的可见历史，并控制自动压缩生成的检查点体积。
// PromptConfig 控制系统提示词来源；两者皆空时使用内置默认提示。
// 文件路径相对工作区或绝对路径；正文进指纹，文件在 Load 时展开为正文。
type PromptConfig struct {
	// SystemPrompt 直接内联系统提示（优先于 File）。
	SystemPrompt string `yaml:"system_prompt,omitempty"`
	// SystemPromptFile 指向 UTF-8 文本文件。
	SystemPromptFile string `yaml:"system_prompt_file,omitempty"`
	// Resolved 是 Load 展开后的最终正文（不进 YAML 解码，仅供装配）。
	Resolved string `yaml:"-" json:"-"`
}

type ContextConfig struct {
	AutoCompact             bool `yaml:"auto_compact"`
	MaxPromptTokens         int  `yaml:"max_prompt_tokens"`
	MaxSummaryBytes         int  `yaml:"max_summary_bytes"`
	InventoryBudgetPercent  int  `yaml:"inventory_budget_percent"`
	ThresholdReservePercent int  `yaml:"threshold_reserve_percent"`
	// DecisionCheckpoint 是可选的软决策检查点：在仍无补丁提议且已有读取
	// 证据时插入一次说明性反馈。指针 + omitempty：未配置时指纹与旧版逐位
	// 一致，不影响既有冻结配置。
	DecisionCheckpoint *DecisionCheckpointConfig `yaml:"decision_checkpoint,omitempty" json:"DecisionCheckpoint,omitempty"`
}

// DecisionCheckpointConfig 控制软决策检查点实验：只反馈、不强制写入。
type DecisionCheckpointConfig struct {
	Enabled bool `yaml:"enabled"`
	// MinTurns 是触发前至少已完成的模型轮次；缺省 DecisionCheckpointMinTurns。
	MinTurns int `yaml:"min_turns"`
}

// DecisionCheckpointMinTurns 是软决策检查点的默认最早触发轮次。
const DecisionCheckpointMinTurns = 4

// GuardConfig 覆盖循环熔断阈值；指针 + omitempty：未配置时使用默认值且
// 指纹与旧版逐位一致。提高阈值必须显式配置并进入指纹。
type GuardConfig struct {
	// ConsecutiveOperationFailures 是相同操作指纹连续失败的熔断阈值。
	ConsecutiveOperationFailures int `yaml:"consecutive_operation_failures"`
	// PeriodicTurnWindow 是 A-B 周期检测的轮次窗口。
	PeriodicTurnWindow int `yaml:"periodic_turn_window"`
	// NoProgressTurnLimit 是工作区/验证长期无进展的轮次阈值。
	NoProgressTurnLimit int `yaml:"no_progress_turn_limit"`
}

// DefaultGuardConsecutiveOperationFailures 是默认连续同指纹失败熔断阈值。
const DefaultGuardConsecutiveOperationFailures = 3

// DefaultGuardPeriodicTurnWindow 是默认周期轮次窗口。
const DefaultGuardPeriodicTurnWindow = 6

// DefaultGuardNoProgressTurnLimit 是默认无进展轮次阈值。
const DefaultGuardNoProgressTurnLimit = 6

// PricingConfig 必须显式启用；只有输入与输出 token 价格都给出时，成本预算
// 才合法，Cairn 不会静默采用虚构的成本估计。
type PricingConfig struct {
	InputTokenUSD  float64 `yaml:"input_token_usd"`
	OutputTokenUSD float64 `yaml:"output_token_usd"`
}

// OutputConfig 限制事件预览和证据制品的体积。
type OutputConfig struct {
	MaxPreviewBytes  int `yaml:"max_preview_bytes"`
	MaxArtifactBytes int `yaml:"max_artifact_bytes"`
}

// CancellationConfig 配置终止子进程前的优雅等待时间。
type CancellationConfig struct {
	TerminateGrace string `yaml:"terminate_grace"`
}

// SandboxConfig 决定 exec_command 使用宿主机还是受限 Docker 容器；Docker
// 后端必须显式指定已存在的镜像，不能隐式回退到宿主机执行。
type SandboxConfig struct {
	Backend     string `yaml:"backend"`
	DockerImage string `yaml:"docker_image"`
	MaxPIDs     int    `yaml:"max_pids"`
}

// ToolsConfig 控制可选工具面暴露；Web、ExecSession、Browser、RunTests 与
// LspDiagnostics 默认关闭或仅出现在非 read-only Preset（opt-in）。
type ToolsConfig struct {
	Web bool `yaml:"web"`
	// ExecSession 显式启用受控 tmux 会话工具；默认不注册。
	ExecSession bool `yaml:"exec_session"`
	// Browser 启用 headless Chrome CDP 工具；默认不注册。
	Browser bool `yaml:"browser"`
	// RunTests 启用 run_tests 结构化测试执行。
	RunTests bool `yaml:"run_tests"`
	// LspDiagnostics 启用 lsp_diagnostics 只读诊断。
	LspDiagnostics bool `yaml:"lsp_diagnostics"`
}

// HookConfig 单条工作区钩子（YAML 形状与 hooks.Rule 对齐）。
type HookConfig struct {
	ID        string `yaml:"id"`
	Event     string `yaml:"event"`
	ToolMatch string `yaml:"tool_match,omitempty"`
	Effect    string `yaml:"effect,omitempty"`
	Reason    string `yaml:"reason,omitempty"`
	Enabled   bool   `yaml:"enabled"`
	Actions   []struct {
		Kind    string `yaml:"kind"`
		Content string `yaml:"content,omitempty"`
	} `yaml:"actions,omitempty"`
}

// PolicyConfig 决定工作区写入、进程执行和环境变量暴露的默认边界。
type PolicyConfig struct {
	// Mode 是三档命名权限：review（默认审核）、workspace（默认权限）、full（完全权限）。
	// 显式 write/exec 优先于 Mode 合成值。
	Mode           string   `yaml:"mode,omitempty"`
	Write          string   `yaml:"write"`
	Exec           string   `yaml:"exec"`
	ProtectedPaths []string `yaml:"protected_paths"`
	EnvAllow       []string `yaml:"env_allowlist"`
}

// 权限模式常量；与 Codex/DSH 三档对齐。
const (
	// PermissionReview 默认审核：写与执行均需确认。
	PermissionReview = "review"
	// PermissionWorkspace 默认权限：工作区内写与常规执行放行（沙箱与危险命令仍拦）。
	PermissionWorkspace = "workspace"
	// PermissionFull 完全权限：写与执行放行；危险命令与受保护路径仍失败关闭。
	PermissionFull = "full"
)

// ApplyPermissionMode 将命名权限模式合成 Write/Exec；显式非空值优先。
func ApplyPermissionMode(policy *PolicyConfig) error {
	if policy == nil {
		return errors.New("policy is required")
	}
	mode := strings.TrimSpace(policy.Mode)
	if mode == "" {
		return nil
	}
	var write, exec string
	switch mode {
	case PermissionReview:
		write, exec = "ask", "ask"
	case PermissionWorkspace:
		write, exec = "allow", "allow"
	case PermissionFull:
		write, exec = "allow", "allow"
	default:
		return fmt.Errorf("unknown policy.mode %q (supported: %s, %s, %s)", mode, PermissionReview, PermissionWorkspace, PermissionFull)
	}
	if strings.TrimSpace(policy.Write) == "" {
		policy.Write = write
	}
	if strings.TrimSpace(policy.Exec) == "" {
		policy.Exec = exec
	}
	return nil
}

// VerificationConfig 描述完成前必须执行的固定验收命令。
type VerificationConfig struct {
	VolatilePaths  []string              `yaml:"volatile_paths"`
	Commands       []VerificationCommand `yaml:"commands"`
	AutoAfterPatch bool                  `yaml:"auto_after_patch"`
}

// VerificationCommand 是独立于模型提议的受控验证命令。
type VerificationCommand struct {
	Argv           []string          `yaml:"argv"`
	CWD            string            `yaml:"cwd"`
	Timeout        string            `yaml:"timeout"`
	MaxOutputBytes int               `yaml:"max_output_bytes"`
	Env            map[string]string `yaml:"env"`
}

// Overrides 保存由环境变量或 CLI 提供的可控运行时覆盖项。
type Overrides struct {
	// Mode 覆盖 active_mode（CLI --mode）。
	Mode            string
	BaseURL         string
	Model           string
	APIKeyEnv       string
	MaxTurns        int
	MaxToolCalls    int
	MaxInputTokens  int
	MaxOutputTokens int
	MaxWallTime     string
	MaxCostUSD      float64
}

// Defaults 返回安全优先的 v2 默认配置，Provider 启动前必须通过准入门，
// 写入和进程执行都要求审批。
func Defaults() Config {
	return Config{
		Version: CurrentVersion,
		Profile: ProfileDefault,
		Preset:  PresetCoding,
		Provider: ProviderConfig{
			APIKeyEnv: "AGENT_API_KEY", RequestTimeout: "120s",
			ID: contract.ProviderIDCustom, Admission: ProviderAdmissionRequired,
		},
		Workspace: WorkspaceConfig{Ignore: []string{".git", ".agent", ".cairn", "node_modules", "vendor"}, MaxReadBytes: 1024 * 1024, MaxSearchFiles: 10_000, MaxSearchBytes: 64 * 1024 * 1024},
		Policy:    PolicyConfig{Write: "ask", Exec: "ask", ProtectedPaths: []string{".git", ".agent", ".cairn", ".env", ".env.*", "**/*credential*"}, EnvAllow: []string{"PATH", "LANG", "TERM", "TMPDIR", "GOFLAGS"}},
		Budget: BudgetConfig{
			MaxTurns: DefaultMaxTurns, MaxToolCalls: 100, MaxInputTokens: 500_000,
			MaxOutputTokens: 100_000, MaxWallTime: "30m",
		},
		Context: ContextConfig{
			AutoCompact: true, MaxPromptTokens: 50_000, MaxSummaryBytes: 16 * 1024,
			InventoryBudgetPercent: 25, ThresholdReservePercent: 20,
		},
		Verification: VerificationConfig{VolatilePaths: []string{".cache", "tmp/test-results"}},
		Output:       OutputConfig{MaxPreviewBytes: 16 * 1024, MaxArtifactBytes: 10 * 1024 * 1024},
		Cancellation: CancellationConfig{TerminateGrace: "5s"},
		Sandbox:      SandboxConfig{Backend: "host", MaxPIDs: 64},
	}
}

// Load 按固定优先级合并配置：安全默认值 → 命名 Profile 预设 → YAML 显式值 →
// 环境覆盖 → CLI 显式值，并校验最终配置。解析过程为每个关键字段记录最终值与
// 来源，生成非敏感的 Resolved Execution Plan；恢复比较使用解析后的配置指纹，
// Profile 或字段漂移都会被既有指纹校验拒绝。
func (config Config) RequestTimeout() (time.Duration, error) {
	return time.ParseDuration(config.Provider.RequestTimeout)
}

// WallTime 解析并确保运行总时长预算为正数。
func (config Config) WallTime() (time.Duration, error) {
	duration, err := time.ParseDuration(config.Budget.MaxWallTime)
	if err != nil {
		return 0, err
	}
	if duration <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return duration, nil
}

// TerminateGrace 解析并确保进程终止宽限期为正数。
func (config Config) TerminateGrace() (time.Duration, error) {
	duration, err := time.ParseDuration(config.Cancellation.TerminateGrace)
	if err != nil {
		return 0, err
	}
	if duration <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return duration, nil
}

// resolutionUnsupportedValue 标记 Resolved Execution Plan 中未声明的生成参数：
// 两态合同要求缺席也被显式记录，不允许消费方把缺省误解为提供方默认值。
const resolutionUnsupportedValue = "unsupported"

// recordProviderGenerationResolution 把生成参数五字段的最终值与来源写入
// Resolved Execution Plan：显式值来源 yaml，未声明字段统一记录 unsupported
// 与 default 来源。该块没有 Profile 预设、环境变量或 CLI 覆盖路径。
func DefaultPath(workspace string) string { return filepath.Join(workspace, ".agent", "config.yaml") }

// UserConfigPath 返回 ~/.cairn/config.yaml，供用户配置读取与写入共用。
func UserConfigPath() (string, error) {
	return userdir.Path("config.yaml")
}

// ResolveDefaultPath 在无显式 --config 时优先选工作区配置，再选用户配置；
// 两者缺失时返回工作区目标，存在但不可用的来源交由装载入口报错。
func ResolveDefaultPath(workspace string) string {
	workspacePath := DefaultPath(workspace)
	if _, err := os.Lstat(workspacePath); !errors.Is(err, os.ErrNotExist) {
		return workspacePath
	}
	userPath, err := UserConfigPath()
	if err != nil {
		return workspacePath
	}
	if _, err := os.Lstat(userPath); !errors.Is(err, os.ErrNotExist) {
		return userPath
	}
	return workspacePath
}

// applyEnvironment 将允许的 AGENT_* 环境变量覆写到配置中并记录来源；非法数值
// 视为配置错误而非静默忽略，与同名 CLI flag 的行为保持一致。
