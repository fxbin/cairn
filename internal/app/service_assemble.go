// 装配 helper：工作区、执行工具、Journal、制品库与 Loop。
// Author: fxbin
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/lock"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// workspaceFor 创建受限工作区视图，并以其规范根路径计算稳定工作区哈希。
func workspaceFor(root string, configuration config.Config) (tools.Workspace, string, error) {
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return tools.Workspace{}, "", err
		}
		root = current
	}
	workspace, err := tools.NewWorkspace(root, configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
	if err != nil {
		return tools.Workspace{}, "", err
	}
	digest := sha256.Sum256([]byte(workspace.Root))
	return workspace, "sha256:" + hex.EncodeToString(digest[:]), nil
}

// execToolFor 以同一份冻结配置装配 Run、Resume、Reconcile 与 Doctor 使用的
// 命令工具；Docker 后端只携带显式镜像和有限 PID 配额。
func execToolFor(workspace tools.Workspace, configuration config.Config) (tools.ExecCommand, error) {
	if err := config.Validate(configuration); err != nil {
		return tools.ExecCommand{}, err
	}
	environmentAllow := make(map[string]struct{}, len(configuration.Policy.EnvAllow))
	for _, key := range configuration.Policy.EnvAllow {
		environmentAllow[key] = struct{}{}
	}
	grace, err := configuration.TerminateGrace()
	if err != nil {
		return tools.ExecCommand{}, err
	}
	return tools.ExecCommand{
		Workspace:     workspace,
		EnvAllow:      environmentAllow,
		Grace:         grace,
		Backend:       tools.ExecBackend(configuration.Sandbox.Backend),
		DockerImage:   configuration.Sandbox.DockerImage,
		DockerMaxPIDs: configuration.Sandbox.MaxPIDs,
		// host 后端在存在 OS 沙箱（macOS Seatbelt）时默认收紧；无沙箱平台
		// 保持显式 host，不伪造隔离。
		HostConfined: tools.ExecBackend(configuration.Sandbox.Backend) != tools.ExecBackendDocker && tools.OSSandboxAvailable(),
		HostWrite:    configuration.Policy.Write != "deny",
	}, nil
}

// openJournal 打开指定工作区的可写 SQLite 事件日志。
func openJournal(stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.Open(filepath.Join(directory, "runs.db"))
}

// openArtifactStore 打开指定工作区的脱敏内容寻址制品库。
func openArtifactStore(stateDir, workspaceHash string, output config.OutputConfig) (*artifact.Store, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	store, err := artifact.New(filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts"), output.MaxPreviewBytes)
	if err != nil {
		return nil, err
	}
	store.MaxBytes = output.MaxArtifactBytes
	return store, nil
}

// 引用一致，供展示层（如补丁差异视图）消费已持久化证据而不触碰任何状态。
func (service Service) ReadRunArtifact(workspacePath, stateDir string, configuration config.Config, reference artifact.Ref) ([]byte, error) {
	_, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return nil, err
	}
	return readArtifactContent(manifestArtifactRoot(stateDir, hash), reference)
}

// openReadOnlyJournal 打开指定工作区的不可写 SQLite 事件日志。
func openReadOnlyJournal(stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.OpenReadOnly(filepath.Join(directory, "runs.db"))
}

// openReadOnlyJournalContext 在调用方上下文约束下打开指定工作区的不可写
// SQLite 事件日志，供需要在连接探测阶段响应取消的只读用例使用。
func openReadOnlyJournalContext(ctx context.Context, stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.OpenReadOnlyContext(ctx, filepath.Join(directory, "runs.db"))
}

// acquireWorkspaceLock 为指定工作区获取进程级排他锁。
func acquireWorkspaceLock(stateDir, workspaceHash string) (*lock.Lock, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return lock.Acquire(filepath.Join(stateDir, "workspaces", workspaceHash, "run.lock"))
}

// DefaultStateDir 返回平台默认的 Cairn 状态目录；空 stateDir 参数的装配路径
// 统一回退到此值，导出供 CLI 命令复用同一解析。
func DefaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".cairn"
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Cairn")
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "cairn")
	}
	return filepath.Join(home, ".local", "state", "cairn")
}

// patchObservation 返回目标文件当前摘要或 absent，用于人工 patch 对账。
func patchObservation(workspace tools.Workspace, relativePath string) (string, error) {
	path, _, err := workspace.Resolve(relativePath)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// verificationCommands 将配置中的固定验收命令转换为 Agent 可执行计划。
func verificationCommands(configuration config.VerificationConfig) ([]agent.VerificationCommand, error) {
	commands := make([]agent.VerificationCommand, 0, len(configuration.Commands))
	for index, command := range configuration.Commands {
		timeout := 120 * time.Second
		if command.Timeout != "" {
			parsed, err := time.ParseDuration(command.Timeout)
			if err != nil {
				return nil, err
			}
			timeout = parsed
		}
		maximumOutput := command.MaxOutputBytes
		if maximumOutput == 0 {
			maximumOutput = 256 * 1024
		}
		arguments, err := json.Marshal(tools.ExecArguments{Argv: command.Argv, CWD: command.CWD, TimeoutMS: int(timeout.Milliseconds()), MaxOutputBytes: maximumOutput, Env: command.Env})
		if err != nil {
			return nil, err
		}
		commands = append(commands, agent.VerificationCommand{ID: fmt.Sprintf("verification-%d", index+1), Arguments: arguments})
	}
	return commands, nil
}

// configuredLoop 将应用配置与基础设施依赖装配为一个 Agent Loop。
func configuredLoop(journal ports.Journal, provider ports.Provider, registry *tools.Registry, gate *policy.Gate, verification []agent.VerificationCommand, artifacts *artifact.Store, configuration config.Config, onEvent EventSink) agent.Loop {
	loop := agent.Loop{
		Journal: journal, Provider: provider, Tools: registry, Gate: gate,
		Model: configuration.Provider.Model, ConfigFingerprint: configuration.Fingerprint(),
		ProviderKind: providerKindForProtocol(configuration.Provider.Protocol),
		MaxTurns:     configuration.Budget.MaxTurns, MaxToolCalls: configuration.Budget.MaxToolCalls,
		MaxInputTokens: configuration.Budget.MaxInputTokens, MaxOutputTokens: configuration.Budget.MaxOutputTokens,
		MaxCostUSD: configuration.Budget.MaxCostUSD, InputTokenUSD: configuration.Pricing.InputTokenUSD,
		OutputTokenUSD:     configuration.Pricing.OutputTokenUSD,
		ContextAutoCompact: configuration.Context.AutoCompact, ContextMaxPromptTokens: configuration.Context.MaxPromptTokens,
		ContextMaxSummaryBytes:         configuration.Context.MaxSummaryBytes,
		ContextInventoryBudgetPercent:  configuration.Context.InventoryBudgetPercent,
		ContextThresholdReservePercent: configuration.Context.ThresholdReservePercent,
		VerificationAutoAfterPatch:     configuration.Verification.AutoAfterPatch,
		Verification:                   verification, VerificationWorkspace: gate.Patch.Workspace.ObservationView(configuration.Verification.VolatilePaths),
		Artifacts: artifacts, OnEvent: agent.EventSink(onEvent),
		SystemPrompt: configuration.Prompt.Resolved,
	}
	if checkpoint := configuration.Context.DecisionCheckpoint; checkpoint != nil && checkpoint.Enabled {
		loop.DecisionCheckpointEnabled = true
		minTurns := checkpoint.MinTurns
		if minTurns <= 0 {
			minTurns = config.DecisionCheckpointMinTurns
		}
		loop.DecisionCheckpointMinTurns = minTurns
	}
	return loop
}
