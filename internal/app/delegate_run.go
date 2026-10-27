// delegate_run.go 实现模型侧 delegate_task 的应用层回调：向父 Journal 记录
// 委派事实，受控启动子 run 并同步等待终态；子预算与工具面收窄沿用既有合同，
// 不扩大授权、不虚构子代理。另含子运行启动时的家族活跃运行豁免。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// DelegateResponseMaxBytes 限制回传父上下文的子最终回复长度，防止子回复
// 无界占据父上下文。
const DelegateResponseMaxBytes = 2048

// delegatorFor 构建根运行的委派回调；回调复用父 Boot 的 Journal 与运行配置，
// 父 Loop 在工具执行期间阻塞等待，期间不追加自身事件，保持单写入者合同。
func (service Service) delegatorFor(boot *Boot, options RunOptions) agent.DelegatorFunc {
	return func(ctx context.Context, parentRunID domain.RunID, arguments json.RawMessage) tools.Result {
		var request tools.DelegateRequest
		if err := json.Unmarshal(arguments, &request); err != nil || strings.TrimSpace(request.Task) == "" || request.MaxTurns < 0 || request.MaxTurns > tools.DelegateMaxTurnsLimit {
			return tools.Failure("invalid_arguments", "delegate_task requires task and optional max_turns within limit")
		}
		childRunID := domain.NewRunID()
		// 配置与额度校验先于委派事实落盘：失败关闭时不留下孤儿 delegated
		// 事实（无子运行的悬空委派会永久占据父投影 Children）。
		childConfig, err := childDelegationConfig(ctx, boot.Journal, parentRunID, options.Config, request)
		if err != nil {
			return tools.Failure("delegate_failed", err.Error())
		}
		if _, err := RecordSubagentDelegated(ctx, boot.Journal, parentRunID, boot.WorkspaceHash, DelegateOptions{
			ParentRunID: parentRunID,
			ChildRunID:  childRunID,
			Task:        request.Task,
			Budget:      SubagentBudget{MaxTurns: request.MaxTurns},
		}, time.Now().UTC()); err != nil {
			return tools.Failure("delegate_failed", err.Error())
		}
		result, err := service.Run(ctx, RunOptions{
			Task:             request.Task,
			Workspace:        options.Workspace,
			StateDir:         options.StateDir,
			Config:           childConfig,
			PreassignedRunID: childRunID,
			ParentRunID:      parentRunID,
			// 显式传递父运行真实模型可见工具面（含 skill 与 MCP 目录），
			// 避免按冻结 Preset 重建的基础面因缺少 opt-in 扩展而误拒收窄。
			ParentTools: boot.Registry.Names(),
		})
		return delegateTaskResult(childRunID, request.MaxTurns, result, err)
	}
}

// childDelegationConfig 从父配置派生子配置：轮数取「请求额度、父配置额度、
// 父剩余轮数」的最小值；父显式配置了轮数且已无剩余时失败关闭，禁止把
// 超额或无限制额度借给子运行。其余字段与父配置一致。
func childDelegationConfig(ctx context.Context, journal ports.Journal, parentRunID domain.RunID, parentConfig config.Config, request tools.DelegateRequest) (config.Config, error) {
	childConfig := parentConfig
	limit := request.MaxTurns
	if parentConfig.Budget.MaxTurns > 0 && (limit <= 0 || limit > parentConfig.Budget.MaxTurns) {
		limit = parentConfig.Budget.MaxTurns
	}
	if parentConfig.Budget.MaxTurns > 0 {
		opened, err := parentOpenedTurns(ctx, journal, parentRunID)
		if err != nil {
			return config.Config{}, err
		}
		remaining := parentConfig.Budget.MaxTurns - opened
		if remaining < 1 {
			return config.Config{}, errors.New("parent run has no remaining turns to delegate")
		}
		if limit <= 0 || limit > remaining {
			limit = remaining
		}
	}
	childConfig.Budget.MaxTurns = limit
	return childConfig, nil
}

// parentOpenedTurns 统计父运行已开启的轮数，供子额度钳制。
func parentOpenedTurns(ctx context.Context, journal ports.Journal, parentRunID domain.RunID) (int, error) {
	events, err := journal.Load(ctx, parentRunID)
	if err != nil {
		return 0, fmt.Errorf("load parent run for remaining turns: %w", err)
	}
	opened := 0
	for _, event := range events {
		if event.Type == domain.EventTurnOpened {
			opened++
		}
	}
	return opened, nil
}

// delegateTaskPayload 是回传父模型的委派结果载荷。
type delegateTaskPayload struct {
	ChildRunID string `json:"child_run_id"`
	Outcome    string `json:"outcome"`
	MaxTurns   int    `json:"max_turns,omitempty"`
	Response   string `json:"response,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Failure    string `json:"failure,omitempty"`
}

// delegateTaskResult 把子 run 的返回映射为父工具结果；子审批暂停不是父错误，
// 取消与失败按终态语义回传，供父模型继续决策。
func delegateTaskResult(childRunID domain.RunID, maxTurns int, result RunResult, err error) tools.Result {
	var waiting *ports.WaitingApprovalError
	if errors.As(err, &waiting) {
		payload, _ := json.Marshal(delegateTaskPayload{ChildRunID: string(childRunID), Outcome: "awaiting_approval", MaxTurns: maxTurns})
		return tools.Result{OK: true, Code: "awaiting_approval", Data: payload, Preview: "子运行 " + string(childRunID) + " 等待审批"}
	}
	// 结果口径以子运行已持久化终态为准（与完成事实回写一致）；投影不可用时
	// 才回退错误映射，避免预算耗尽等场景在父视图与 Journal 间出现两种口径。
	outcome := ""
	if result.Result != nil {
		switch result.Result.State {
		case string(domain.RunCompleted):
			outcome = "ok"
		case string(domain.RunCancelled):
			outcome = "cancelled"
		case string(domain.RunFailed):
			outcome = "failed"
		}
	}
	if outcome == "" {
		outcome = MapRunOutcome(err, "")
	}
	response := result.Response
	truncated := len(response) > DelegateResponseMaxBytes
	if truncated {
		cut := response[:DelegateResponseMaxBytes]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		response = cut
	}
	payload, _ := json.Marshal(delegateTaskPayload{ChildRunID: string(childRunID), Outcome: outcome, MaxTurns: maxTurns, Response: response, Truncated: truncated, Failure: errorText(err)})
	return tools.Result{OK: err == nil, Code: outcome, Data: payload, Preview: "子运行 " + string(childRunID) + " " + outcome, Truncated: truncated, Bytes: int64(len(response))}
}

// errorText 提取错误文本；空错误返回空串。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// tolerateFamilyActiveRuns 子运行家族豁免：活跃运行必须全部是本子运行的
// 祖先（沿 parent_run_id 上溯）或同父兄弟（父投影 Children），否则维持
// 排他拒绝；祖先历史缺失或损坏失败关闭。
func tolerateFamilyActiveRuns(ctx context.Context, journal ports.Journal, workspaceHash string, active []domain.RunProjection, parentRunID domain.RunID) error {
	if len(active) == 0 {
		return nil
	}
	allowed := map[domain.RunID]struct{}{parentRunID: {}}
	frontier := []domain.RunID{parentRunID}
	for len(frontier) > 0 {
		current := frontier[0]
		frontier = frontier[1:]
		parent, err := parentOfRun(ctx, journal, current)
		if err != nil {
			return err
		}
		if parent == "" {
			continue
		}
		if _, seen := allowed[parent]; seen {
			continue
		}
		allowed[parent] = struct{}{}
		frontier = append(frontier, parent)
	}
	if parentEvents, err := journal.Load(ctx, parentRunID); err == nil {
		if projection, replayErr := domain.Replay(parentEvents); replayErr == nil {
			for childID := range projection.Children {
				allowed[domain.RunID(childID)] = struct{}{}
			}
		}
	}
	for _, run := range active {
		if _, ok := allowed[run.ID]; !ok {
			return fmt.Errorf("%w: resume %s", ErrActiveRun, run.ID)
		}
	}
	return nil
}

// parentOfRun 只读回放创建事件返回其 parent_run_id；历史缺失或首事件非法
// 时失败关闭，不猜测亲缘关系。
func parentOfRun(ctx context.Context, journal ports.Journal, runID domain.RunID) (domain.RunID, error) {
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load ancestor run %s: %w", runID, err)
	}
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return "", fmt.Errorf("ancestor run %s is missing a creation event", runID)
	}
	var payload struct {
		ParentRunID domain.RunID `json:"parent_run_id"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		return "", fmt.Errorf("decode ancestor creation for run %s: %w", runID, err)
	}
	return payload.ParentRunID, nil
}
