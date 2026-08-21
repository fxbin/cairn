// Package app 装配面向 CLI 的 Cairn 运行、恢复与诊断用例。
package app

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
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
	Registry      *tools.Registry
	Gate          *policy.Gate
	Verification  []agent.VerificationCommand
	Loop          agent.Loop
	closeJournal  func() error
	releaseLock   func() error
	closed        bool
}

// assembleBoot 按固定顺序装配：workspace → execTool → journal → 工作区锁 →
// provider → artifacts → 工具注册表 → 策略门 → 验证命令 → Loop。任一步失败都
// 会关闭已完成初始化的资源并合并关闭错误（Journal 关闭期间继续持有工作区锁），
// 不留下孤儿 Journal 或锁；装配只做纯构造，不产生事件或副作用。
func (service Service) assembleBoot(workspacePath, stateDir string, cfg config.Config, onEvent EventSink) (*Boot, error) {
	boot := &Boot{Config: cfg}
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
	provider, err := service.provider(cfg)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.Provider = provider
	artifacts, err := openArtifactStore(stateDir, hash, cfg.Output)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.Artifacts = artifacts
	registry, err := tools.NewRegistry(presetTools(workspace, execTool, cfg.Preset)...)
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
	verification, err := verificationCommands(cfg.Verification)
	if err != nil {
		return nil, bootAssemblyFailure(boot, err)
	}
	boot.Verification = verification
	boot.Loop = configuredLoop(journal, provider, registry, gate, verification, artifacts, cfg, onEvent)
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

// CapabilityManifest 是对当前装配能力的只读、可追溯描述；完整 Preset 合同
// （工具/Policy/Verifier/预算展开进指纹）由后续切片扩展，此处仅汇总非敏感
// 能力事实供诊断与测试使用。
type CapabilityManifest struct {
	Preset       string   `json:"preset"`
	Profile      string   `json:"profile"`
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Sandbox      string   `json:"sandbox"`
	Policy       string   `json:"policy"`
	BudgetTurns  int      `json:"budget_max_turns"`
	Tools        []string `json:"tools"`
	Verification int      `json:"verification_commands"`
}

// Manifest 返回当前装配的非敏感能力清单；工具名来自固定注册表的规格顺序。
func (boot *Boot) Manifest() CapabilityManifest {
	names := make([]string, 0, len(boot.Registry.Specs()))
	for _, spec := range boot.Registry.Specs() {
		names = append(names, spec.Name)
	}
	return CapabilityManifest{
		Preset:       boot.Config.Preset,
		Profile:      boot.Config.Profile,
		Provider:     boot.Config.Provider.BaseURL,
		Model:        boot.Config.Provider.Model,
		Sandbox:      boot.Config.Sandbox.Backend,
		Policy:       "write=" + boot.Config.Policy.Write + ",exec=" + boot.Config.Policy.Exec,
		BudgetTurns:  boot.Config.Budget.MaxTurns,
		Tools:        names,
		Verification: len(boot.Verification),
	}
}
