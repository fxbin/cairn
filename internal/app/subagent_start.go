// subagent_start.go 实现 AGT-02 显式委派与受控启动：子 run 必须先有
// delegated 事实，工具集从父收窄，共享工作区保持排他锁语义；无运行时
// 不得虚构子代理身份。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// ErrSubagentStart 表示子运行无法受控启动。
var ErrSubagentStart = errors.New("subagent start failed")

// StartSubagentOptions 描述一次受控子启动的输入。
type StartSubagentOptions struct {
	ParentRunID   domain.RunID
	ChildRunID    domain.RunID
	Task          string
	WorkspaceHash string
	Budget        SubagentBudget
	// ParentTools 是父运行可见工具名集合；子集必须 ⊆ 父集合。
	ParentTools []string
	// ChildTools 是本次子运行请求的工具名集合。
	ChildTools []string
	// Config 是子运行冻结配置；工具面与预算需与委派一致。
	Config config.Config
}

// NarrowTools 返回 child ⊆ parent 的收窄结果；越界返回错误。
func NarrowTools(parentTools, childTools []string) ([]string, error) {
	parentSet := make(map[string]struct{}, len(parentTools))
	for _, name := range parentTools {
		parentSet[name] = struct{}{}
	}
	narrowed := make([]string, 0, len(childTools))
	seen := make(map[string]struct{}, len(childTools))
	for _, name := range childTools {
		if _, ok := parentSet[name]; !ok {
			return nil, fmt.Errorf("%w: tool %q not in parent set", ErrSubagentStart, name)
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		narrowed = append(narrowed, name)
	}
	return narrowed, nil
}

// PrepareSubagentStart 校验委派存在、工具收窄与预算一致，并返回可启动的
// 子配置；不启动进程、不写虚构身份。
func PrepareSubagentStart(ctx context.Context, journal ports.Journal, options StartSubagentOptions) (config.Config, error) {
	if journal == nil {
		return config.Config{}, fmt.Errorf("%w: journal is required", ErrSubagentStart)
	}
	if options.ParentRunID == "" || options.ChildRunID == "" || options.ParentRunID == options.ChildRunID {
		return config.Config{}, fmt.Errorf("%w: parent and child run ids are required and must differ", ErrSubagentStart)
	}
	if _, err := NarrowTools(options.ParentTools, options.ChildTools); err != nil {
		return config.Config{}, err
	}
	projection, events, err := subagentParent(ctx, journal, options.ParentRunID, options.WorkspaceHash)
	if err != nil {
		return config.Config{}, fmt.Errorf("%w: %v", ErrSubagentStart, err)
	}
	child, exists := projection.Children[string(options.ChildRunID)]
	if !exists || child.State != "delegated" {
		return config.Config{}, fmt.Errorf("%w: missing live delegation for child (no fictional subagent)", ErrSubagentStart)
	}
	var delegated *DelegatedPayload
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != domain.EventSubagentDelegated {
			continue
		}
		var payload DelegatedPayload
		if err := jsonUnmarshal(events[index].Payload, &payload); err != nil {
			continue
		}
		if payload.ChildRunID == string(options.ChildRunID) {
			delegated = &payload
			break
		}
	}
	if delegated == nil {
		return config.Config{}, fmt.Errorf("%w: delegation fact is missing", ErrSubagentStart)
	}
	if delegated.TaskDigest != TaskDigest(options.Task) {
		return config.Config{}, fmt.Errorf("%w: task digest mismatch", ErrSubagentStart)
	}
	childConfig := options.Config
	if delegated.Budget.MaxTurns > 0 && (childConfig.Budget.MaxTurns <= 0 || childConfig.Budget.MaxTurns > delegated.Budget.MaxTurns) {
		childConfig.Budget.MaxTurns = delegated.Budget.MaxTurns
	}
	parentRemaining := NewBudgetRemaining(options.Config.Budget)
	need := BudgetRemaining{
		MaxTurns:        delegated.Budget.MaxTurns,
		MaxInputTokens:  int64(childConfig.Budget.MaxInputTokens),
		MaxOutputTokens: int64(childConfig.Budget.MaxOutputTokens),
	}
	if _, err := parentRemaining.Deduct(need); err != nil {
		return config.Config{}, fmt.Errorf("%w: %v", ErrSubagentStart, err)
	}
	return childConfig, nil
}

// SubagentStartEvidence 是可展示的受控启动证据（无虚构）。
type SubagentStartEvidence struct {
	ChildRunID    string    `json:"child_run_id"`
	ParentRunID   string    `json:"parent_run_id"`
	NarrowedTools []string  `json:"narrowed_tools"`
	StartedAt     time.Time `json:"started_at"`
}

// BuildStartEvidence 从收窄结果组装启动证据。
func BuildStartEvidence(parent, child domain.RunID, narrowed []string, at time.Time) SubagentStartEvidence {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return SubagentStartEvidence{
		ChildRunID:    string(child),
		ParentRunID:   string(parent),
		NarrowedTools: append([]string(nil), narrowed...),
		StartedAt:     at.UTC(),
	}
}

// enforceSubagentToolNarrowing 在子 run 启动前强制工具面收窄：子可见工具必须
// ⊆ 父工具集。父集合优先取调用方显式传入的 ParentTools；缺失时按父冻结配置
// 的 Preset 重建基础工具面（不含 opt-in 扩展），保持失败关闭。
func enforceSubagentToolNarrowing(ctx context.Context, journal ports.Journal, artifacts *artifact.Store, workspaceHash string, registry *tools.Registry, options RunOptions) error {
	if registry == nil {
		return fmt.Errorf("%w: tool registry is required", ErrSubagentStart)
	}
	childTools := registry.Names()
	parentTools := options.ParentTools
	if len(parentTools) == 0 {
		derived, err := parentBaseToolNames(ctx, journal, artifacts, options.StateDir, workspaceHash, options.ParentRunID)
		if err != nil {
			return err
		}
		parentTools = derived
	}
	if _, err := NarrowTools(parentTools, childTools); err != nil {
		return err
	}
	return nil
}

// parentBaseToolNames 从父 run 冻结配置的 Preset 重建基础工具面；缺失或损坏
// 失败关闭，不得以空集合或子集合顶替。
func parentBaseToolNames(ctx context.Context, journal ports.Journal, artifacts *artifact.Store, stateDir, workspaceHash string, parentRunID domain.RunID) ([]string, error) {
	if journal == nil || parentRunID == "" {
		return nil, fmt.Errorf("%w: parent journal and run id are required to derive tools", ErrSubagentStart)
	}
	events, err := journal.Load(ctx, parentRunID)
	if err != nil {
		return nil, fmt.Errorf("%w: load parent run: %v", ErrSubagentStart, err)
	}
	frozenRaw, err := readFrozenRunConfig(events, stateDir, workspaceHash)
	if err != nil {
		return nil, fmt.Errorf("%w: parent frozen config: %v", ErrSubagentStart, err)
	}
	if len(frozenRaw) == 0 {
		return nil, fmt.Errorf("%w: parent frozen config is missing", ErrSubagentStart)
	}
	var frozen contract.FrozenRunConfig
	if err := json.Unmarshal(frozenRaw, &frozen); err != nil {
		return nil, fmt.Errorf("%w: decode parent frozen config: %v", ErrSubagentStart, err)
	}
	preset := frozen.Projection.Preset
	if preset == "" {
		preset = config.PresetCoding
	}
	empty := tools.Workspace{}
	registry, err := tools.NewRegistry(presetTools(empty, tools.ExecCommand{Workspace: empty}, preset, config.ToolsConfig{})...)
	if err != nil {
		return nil, fmt.Errorf("%w: rebuild parent tool surface: %v", ErrSubagentStart, err)
	}
	return registry.Names(), nil
}
