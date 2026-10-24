// Package app 装配面向 CLI 的 Cairn 运行、恢复与诊断用例。
package app

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/adapters/mcpclient"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/hooks"
	"github.com/fxbin/cairn/internal/lock"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// Boot 是唯一的 Composition Root：按确定顺序装配一次执行所需的全部组件，
// 并持有 Journal 与工作区锁的唯一资源所有权。Run、Resume 与 Reconcile 共享
// 同一条装配路径；不引入容器、注册表或生命周期事件总线。
type Boot struct {
	Config        config.Config
	Workspace     tools.Workspace
	WorkspaceHash string
	ExecTool      tools.ExecCommand
	Journal       *sqlite.Journal
	workspaceLock *lock.Lock
	Artifacts     *artifact.Store
	Provider      ports.Provider
	ProviderCard  contract.ProviderCapabilityCard
	Registry      *tools.Registry
	MCP           tools.MCPCatalog
	Gate          *policy.Gate
	Verification  []agent.VerificationCommand
	Loop          agent.Loop
	closeJournal  func() error
	releaseLock   func() error
	closed        bool
}

// resolveProviderCard 解析版本化 Provider 能力卡并对照预算：仅显式配置声明
// 产出卡，未声明一律失败关闭。解析是纯函数，装配在创建任何
// 资源之前执行，保证能力未知时绝不启动。
func resolveProviderCard(configuration config.Config) (contract.ProviderCapabilityCard, error) {
	var declared *contract.ProviderCapabilityDeclaration
	if capability := configuration.Provider.Capability; capability != nil {
		declared = &contract.ProviderCapabilityDeclaration{
			ToolCalls: capability.ToolCalls, StreamingUsage: capability.StreamingUsage,
			ContextWindowTokens: capability.ContextWindowTokens, MaxOutputTokens: capability.MaxOutputTokens,
			StopReasons: capability.StopReasons,
		}
	}
	protocol := configuration.Provider.Protocol
	if protocol == "" {
		protocol = config.ProtocolOpenAIChatCompletions
	}
	card, err := resolveProviderCardForProtocol(protocol, configuration.Provider.Model, declared)
	if err != nil {
		return contract.ProviderCapabilityCard{}, err
	}
	if err := card.ValidateBudget(configuration.Context.MaxPromptTokens, configuration.Budget.MaxOutputTokens); err != nil {
		return contract.ProviderCapabilityCard{}, err
	}
	return card, nil
}

// assembleBoot 按固定顺序装配：workspace → execTool → journal → 工作区锁 →
// provider → artifacts → 工具注册表 → 策略门 → 验证命令 → Loop。任一步失败都
// 会关闭已完成初始化的资源并合并关闭错误（Journal 关闭期间继续持有工作区锁），
// 不留下孤儿 Journal 或锁；装配只做纯构造，不产生事件或副作用。
func (service Service) assembleBoot(workspacePath, stateDir string, cfg config.Config, onEvent EventSink) (*Boot, error) {
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	boot := &Boot{Config: cfg}
	// 能力卡解析先于任何资源创建：未知能力失败关闭，不留孤儿 Journal 或锁。
	card, err := resolveProviderCard(cfg)
	if err != nil {
		return nil, err
	}
	boot.ProviderCard = card
	workspace, hash, err := workspaceFor(workspacePath, cfg)
	if err != nil {
		return nil, err
	}
	boot.Workspace, boot.WorkspaceHash = workspace, hash
	execTool, err := execToolFor(workspace, cfg)
	if err != nil {
		return nil, err
	}
	boot.ExecTool = execTool
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		return nil, err
	}
	boot.Journal = journal
	boot.closeJournal = journal.Close
	workspaceLock, err := acquireWorkspaceLock(stateDir, hash)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.workspaceLock = workspaceLock
	boot.releaseLock = workspaceLock.Release
	provider, err := service.provider(cfg, workspace.Root)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.Provider = provider
	artifacts, err := openArtifactStore(stateDir, hash, cfg.Output)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.Artifacts = artifacts
	var catalog tools.MCPCatalog
	if cfg.MCP != nil && hasEnabledMCP(cfg.MCP) {
		authDirectory, err := mcpclient.CredentialDirectory()
		if err != nil {
			return nil, bootAssemblyFailure(boot, errors.New("resolve MCP credential directory failed"))
		}
		catalog, err = mcpclient.New(*cfg.MCP, workspace, execTool, authDirectory)
		if err != nil {
			return nil, bootAssemblyFailure(boot, err)
		}
		boot.MCP = catalog
	}
	available := presetTools(workspace, execTool, cfg.Preset, cfg.Tools)
	skillTool, err := service.configuredSkillTool(workspace.Root, cfg)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	if skillTool != nil {
		available = append(available, skillTool)
	}
	registry, err := tools.NewRegistryWithMCP(catalog, available...)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.Registry = registry
	patchTool := tools.ApplyPatch{Workspace: workspace}
	gate, err := policy.New(cfg.Policy.Write, cfg.Policy.Exec, cfg.Policy.ProtectedPaths, patchTool)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.Gate = gate
	gate.MCPRegistry = registry
	gate.SetHooks(hooksFromConfig(cfg.Hooks))
	verification, err := verificationCommands(cfg.Verification)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.Verification = verification
	boot.Loop = configuredLoop(journal, provider, registry, gate, verification, artifacts, cfg, onEvent)
	boot.Loop.RequireProviderUsage = card.StreamingUsage
	boot.Loop.AllowedProviderStopReasons = append([]string(nil), card.StopReasons...)
	return boot, nil
}

// assembleRecoveryBoot 只装配确定性恢复控制面需要的工作区、Journal 与排他锁；
// 它不解析能力卡、不构造 Provider、工具或 Loop，供 abort_run 与显式 Abort 在
// Provider 不可用时仍能依据既有事件安全收尾。
func assembleRecoveryBoot(workspacePath, stateDir string, cfg config.Config) (*Boot, error) {
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	boot := &Boot{Config: cfg}
	workspace, hash, err := workspaceFor(workspacePath, cfg)
	if err != nil {
		return nil, err
	}
	boot.Workspace, boot.WorkspaceHash = workspace, hash
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		return nil, err
	}
	boot.Journal = journal
	boot.closeJournal = journal.Close
	workspaceLock, err := acquireWorkspaceLock(stateDir, hash)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.workspaceLock = workspaceLock
	boot.releaseLock = workspaceLock.Release
	return boot, nil
}

// bootAssemblyFailure 在装配失败后尝试关闭已取得的资源，并保留原始失败与全部
// 清理失败；调用方据此既能看到装配根因，也不会丢失可能影响后续恢复的关闭错误。
func bootAssemblyFailure(boot *Boot, cause error) error {
	return errors.Join(cause, boot.Close())
}

// Close 在释放工作区锁前先关闭 Journal，并返回关闭错误；Loop、Provider、Registry
// 与 Gate 无常驻句柄，Artifacts 无需显式回收。Close 幂等，重复调用安全；单个组件
// 的关闭错误不阻止其余资源释放，全部错误合并返回供调用方上报。
func (boot *Boot) Close() error {
	if boot == nil || boot.closed {
		return nil
	}
	boot.closed = true
	var closeErrors []error
	if boot.MCP != nil {
		if err := boot.MCP.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close MCP: %w", err))
		}
	}
	closeJournal := boot.closeJournal
	if closeJournal == nil && boot.Journal != nil {
		closeJournal = boot.Journal.Close
	}
	if closeJournal != nil {
		if err := closeJournal(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close journal: %w", err))
		}
	}
	releaseLock := boot.releaseLock
	if releaseLock == nil && boot.workspaceLock != nil {
		releaseLock = boot.workspaceLock.Release
	}
	if releaseLock != nil {
		if err := releaseLock(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("release workspace lock: %w", err))
		}
	}
	return errors.Join(closeErrors...)
}

// ToolCapability 描述固定注册表中一件工具的非敏感能力事实：名称与其保守
// 副作用分类；只读工具为 none，apply_patch 为 workspace，exec_command 为 process。
type ToolCapability struct {
	Name       string `json:"name"`
	SideEffect string `json:"side_effect"`
}

// BudgetView 展开进入执行语义的全部预算字段，与配置指纹的预算视图一一对应。
type BudgetView struct {
	MaxTurns        int     `json:"max_turns"`
	MaxToolCalls    int     `json:"max_tool_calls"`
	MaxInputTokens  int     `json:"max_input_tokens"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	MaxWallTime     string  `json:"max_wall_time"`
	MaxCostUSD      float64 `json:"max_cost_usd,omitempty"`
}

// SandboxView 展开执行后端与任务镜像；镜像仅在 Docker 后端下非空。
type SandboxView struct {
	Backend     string `json:"backend"`
	DockerImage string `json:"docker_image,omitempty"`
}

// PolicyView 展开写入与命令执行策略及防篡改路径模式；全部为非敏感配置事实。
type PolicyView struct {
	Write          string   `json:"write"`
	Exec           string   `json:"exec"`
	ProtectedPaths []string `json:"protected_paths,omitempty"`
}

// VerificationView 展开冻结验证命令数量与其稳定摘要；摘要算法与运行期
// 验证计划（createVerificationPlan）一致，保证 manifest 与事件证据可对账。
type VerificationView struct {
	Commands    int    `json:"commands"`
	Fingerprint string `json:"fingerprint"`
}

// CapabilityManifest 是对当前装配能力的只读、可追溯描述：Preset 的工具、
// Policy、Verifier、Sandbox、预算与 Provider 能力卡完整展开，供诊断与测试
// 对账配置指纹。
type CapabilityManifest struct {
	Preset            string                           `json:"preset"`
	Profile           string                           `json:"profile"`
	Provider          string                           `json:"provider"`
	Model             string                           `json:"model"`
	ProviderCard      *contract.ProviderCapabilityCard `json:"provider_card,omitempty"`
	Sandbox           SandboxView                      `json:"sandbox"`
	Policy            PolicyView                       `json:"policy"`
	Budget            BudgetView                       `json:"budget"`
	Tools             []ToolCapability                 `json:"tools"`
	Verification      VerificationView                 `json:"verification"`
	ConfigFingerprint string                           `json:"config_fingerprint"`
	MCP               []MCPDeclaration                 `json:"mcp,omitempty"`
	Skills            *config.SkillsConfig             `json:"skills,omitempty"`
}

// Manifest 返回当前装配的非敏感能力清单；由装配后的注册表与冻结验证命令构建。
func (boot *Boot) Manifest() CapabilityManifest {
	tools := make([]ToolCapability, 0, len(boot.Registry.Specs()))
	for _, spec := range boot.Registry.Specs() {
		tools = append(tools, ToolCapability{Name: spec.Name, SideEffect: string(boot.Registry.SideEffect(spec.Name))})
	}
	if boot.ProviderCard.SchemaVersion == 0 {
		return buildCapabilityManifest(boot.Config, tools, boot.Verification, nil)
	}
	card := boot.ProviderCard
	return buildCapabilityManifest(boot.Config, tools, boot.Verification, &card)
}

// buildCapabilityManifest 由配置、工具能力与冻结验证命令构建唯一投影；
// Boot.Manifest 与 doctor 共用本函数，保证两个视图不矛盾。
// 未解析（nil）时 manifest 省略卡字段，避免把零值卡误读为空卡合同。
func buildCapabilityManifest(configuration config.Config, tools []ToolCapability, verification []agent.VerificationCommand, card *contract.ProviderCapabilityCard) CapabilityManifest {
	fingerprint, err := agent.FingerprintVerificationCommands(verification)
	if err != nil {
		fingerprint = ""
	}
	return CapabilityManifest{
		Preset:            configuration.Preset,
		Profile:           configuration.Profile,
		Provider:          configuration.Provider.BaseURL,
		Model:             configuration.Provider.Model,
		ProviderCard:      card,
		Sandbox:           SandboxView{Backend: configuration.Sandbox.Backend, DockerImage: configuration.Sandbox.DockerImage},
		Policy:            PolicyView{Write: configuration.Policy.Write, Exec: configuration.Policy.Exec, ProtectedPaths: append([]string(nil), configuration.Policy.ProtectedPaths...)},
		Budget:            BudgetView{MaxTurns: configuration.Budget.MaxTurns, MaxToolCalls: configuration.Budget.MaxToolCalls, MaxInputTokens: configuration.Budget.MaxInputTokens, MaxOutputTokens: configuration.Budget.MaxOutputTokens, MaxWallTime: configuration.Budget.MaxWallTime, MaxCostUSD: configuration.Budget.MaxCostUSD},
		Tools:             tools,
		Verification:      VerificationView{Commands: len(verification), Fingerprint: fingerprint},
		ConfigFingerprint: configuration.Fingerprint(),
		MCP:               mcpDeclarations(configuration.MCP),
		Skills:            configuration.Skills,
	}
}

// hooksFromConfig 将 YAML 钩子配置转换为运行时规则；非法规则跳过（失败关闭装配）。
func hooksFromConfig(configured []config.HookConfig) []hooks.Rule {
	if len(configured) == 0 {
		return nil
	}
	rules := make([]hooks.Rule, 0, len(configured))
	for _, item := range configured {
		rule := hooks.Rule{
			ID:        item.ID,
			Event:     hooks.Event(item.Event),
			ToolMatch: item.ToolMatch,
			Effect:    hooks.Effect(item.Effect),
			Reason:    item.Reason,
			Enabled:   item.Enabled,
		}
		for _, action := range item.Actions {
			rule.Actions = append(rule.Actions, hooks.Action{
				Kind:    hooks.ActionKind(action.Kind),
				Content: action.Content,
			})
		}
		if err := hooks.ValidateRule(rule); err != nil {
			continue
		}
		rules = append(rules, rule)
	}
	return rules
}
