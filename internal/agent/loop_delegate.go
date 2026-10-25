// loop_delegate.go 实现 delegate_task 在 Agent Loop 内的受控执行：委派
// 回调以父运行身份启动子 run 并同步等待；回调期间应用层向父 Journal 追加
// delegated/completed 外部事实，返回后先确定性再同步投影再继续本 Loop 的
// 事件追加，保持单写入者与 CAS 合同。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// DelegatorFunc 以父运行身份执行一次委派；结果按工具结果合同回传，错误
// 编码进 Result 而不是以 Go error 中断父循环。
type DelegatorFunc func(ctx context.Context, parentRunID domain.RunID, arguments json.RawMessage) tools.Result

// executeDelegateTask 经装配的委派回调执行子任务并再同步父投影；未装配
// 委派回调的运行（含子运行自身）失败关闭，不虚构子代理。
func (loop *Loop) executeDelegateTask(ctx context.Context, projection *domain.RunProjection, input RunInput, call ports.ToolCall) tools.Result {
	if loop.Delegator == nil {
		return tools.Failure("not_wired", "delegate_task is not configured for this runtime")
	}
	result := redactToolResult(loop.Delegator(ctx, input.RunID, call.Arguments))
	if err := loop.resyncProjection(ctx, projection, input); err != nil {
		return tools.Failure("delegate_resync_failed", err.Error())
	}
	return result
}

// resyncProjection 重载父 Journal 尾部并把外部追加的事实逐事件应用到投影；
// 外部事件违反当前运行状态机时失败关闭。父 Loop 仍是自身事件的唯一调度者，
// 再同步只吸收家族写入的委托/完成/steering 事实，不重放本 Loop 已应用事件。
func (loop *Loop) resyncProjection(ctx context.Context, projection *domain.RunProjection, input RunInput) error {
	events, err := loop.Journal.Load(ctx, input.RunID)
	if err != nil {
		return fmt.Errorf("reload journal for delegation resync: %w", err)
	}
	for _, event := range events {
		if event.Seq <= projection.LastSeq {
			continue
		}
		if event.RunID != input.RunID {
			return fmt.Errorf("delegation resync encountered an event of foreign run %s", event.RunID)
		}
		if err := projection.Apply(event); err != nil {
			return fmt.Errorf("apply external event %s after sequence %d: %w", event.Type, projection.LastSeq, err)
		}
	}
	return nil
}
