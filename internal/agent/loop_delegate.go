// loop_delegate.go 实现 delegate_task 在 Agent Loop 内的受控执行：委派
// 回调以父运行身份启动子 run 并同步等待；回调期间应用层向父 Journal 追加
// delegated/completed 外部事实，返回后先确定性再同步投影再继续本 Loop 的
// 事件追加，保持单写入者与 CAS 合同。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// DelegatorFunc 以父运行身份执行一次委派；结果按工具结果合同回传，错误
// 编码进 Result 而不是以 Go error 中断父循环。
type DelegatorFunc func(ctx context.Context, parentRunID domain.RunID, arguments json.RawMessage) tools.Result

// classifyBatchSpecialError 识别批次错误中必须原样上抛的特殊分类：工具结果
// 落盘失败禁止上层关闭 Turn 或终态化 Run，对账暂停必须保持可恢复语义；
// 未命中特殊分类时返回 nil，交由调用方按普通失败或其余分类收尾。
func classifyBatchSpecialError(err error) error {
	var persistence *toolResultPersistenceError
	if errors.As(err, &persistence) {
		return err
	}
	if errors.Is(err, ports.ErrWaitingReconcile) {
		return err
	}
	return nil
}

// executeDelegateTask 经装配的委派回调执行子任务并再同步父投影；未装配
// 委派回调的运行（含子运行自身）失败关闭，不虚构子代理。
func (loop *Loop) executeDelegateTask(ctx context.Context, projection *domain.RunProjection, input RunInput, call ports.ToolCall) tools.Result {
	if loop.Delegator == nil {
		return tools.Failure("not_wired", "delegate_task is not configured for this runtime")
	}
	result := redactToolResult(loop.Delegator(ctx, input.RunID, call.Arguments))
	if err := loop.resyncProjection(ctx, projection); err != nil {
		return tools.Failure("delegate_resync_failed", err.Error())
	}
	return result
}

// resyncProjection 重载本 run Journal 尾部并把外部追加的事实逐事件应用到
// 投影；外部事件违反当前运行状态机时失败关闭。本 Loop 仍是自身事件的唯一
// 调度者，再同步只吸收家族写入的委托/完成/steering 事实，不重放已应用事件。
// 外部事实不经本 Loop 的 OnEvent sink 广播——流式视图由写入方与 Journal
// 投影（Inspect/进度视图）共同承担，Journal 仍是唯一事实源。
func (loop *Loop) resyncProjection(ctx context.Context, projection *domain.RunProjection) error {
	events, err := loop.Journal.Load(ctx, projection.ID)
	if err != nil {
		return fmt.Errorf("reload journal for resync: %w", err)
	}
	for _, event := range events {
		if event.Seq <= projection.LastSeq {
			continue
		}
		if event.RunID != projection.ID {
			return fmt.Errorf("resync encountered an event of foreign run %s", event.RunID)
		}
		if err := projection.Apply(event); err != nil {
			return fmt.Errorf("apply external event %s after sequence %d: %w", event.Type, projection.LastSeq, err)
		}
	}
	return nil
}
