// task_handoff.go 在显式开启任务交接时，把任务上下文投影生成的交接摘要交给即将启动的
// Worker。它复用会话前情通道：交接文本按内容寻址制品化并在创建事件里引用，因此可以
// 回放；投影本身不缓存，也不是事实源。
//
// [INPUT]: 配置开关 context.task_handoff，以及任务的只读上下文投影（Service.TaskContext）。
// [OUTPUT]: 交给模型的交接文本，或表示“不注入”的空串；上下文含 blocked 字段时返回错误。
// [POS]: 应用层的只读装配。不改变任务状态、授权与运行终态；是否启动运行仍由调用方决定。
// [PROTOCOL]: Update this header on change, then check AGENTS.md
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
)

// ErrTaskHandoffUntrusted 表示任务上下文里有不可信的字段，拒绝在它之上启动 Worker。
var ErrTaskHandoffUntrusted = errors.New("task handoff refuses to start on an untrusted task context")

// taskHandoffRequest 描述一次交接请求的场景与前提。
type taskHandoffRequest struct {
	// target 是触发交接的场景，必须是具体场景而不是 all。
	target config.TaskHandoffMode
	// callerProvidesContext 表示调用方已经自带前情（显式的前驱运行）：此时不重复注入。
	callerProvidesContext bool
	// requireHistory 表示只在任务已有运行时才注入：全新任务没有可交接的东西。
	requireHistory bool
}

// text 生成本次请求的交接文本。开关未覆盖该场景、调用方自带前情，或（被要求时）任务
// 还没有历史，都返回空串表示不注入；读取失败或上下文含 blocked 字段时返回错误。
func (request taskHandoffRequest) text(ctx context.Context, service Service, scope TaskScope, id TaskID) (string, error) {
	handoff := scope.Config.Context.TaskHandoff
	if handoff == nil || !handoff.Mode.Covers(request.target) || request.callerProvidesContext {
		return "", nil
	}
	taskContext, err := service.TaskContext(ctx, scope, id)
	if err != nil {
		return "", err
	}
	return taskHandoffWithHistory(taskContext, request.requireHistory)
}

// taskHandoffWithHistory 必须先做 blocked 核验，再判断是否有可交接的历史。
// 不允许因为 runs 自身 blocked 或为空而跳过其它不可信字段的失败关闭。
func taskHandoffWithHistory(taskContext contract.TaskContextV1, requireHistory bool) (string, error) {
	handoff, err := taskHandoffPrompt(taskContext)
	if err != nil {
		return "", err
	}
	if requireHistory && (taskContext.Runs.Status != contract.TaskFactKnown || len(*taskContext.Runs.Value) == 0) {
		return "", nil
	}
	return handoff, nil
}

// taskHandoffPrompt 把投影变成交给模型的文本。任一字段落 blocked 时拒绝：不可信的
// 状态不该驱动一次运行。unknown 照常注入，交接摘要里有显式标记。
func taskHandoffPrompt(taskContext contract.TaskContextV1) (string, error) {
	if reason, blocked := taskContext.BlockedReason(); blocked {
		return "", fmt.Errorf("%w: %s", ErrTaskHandoffUntrusted, reason)
	}
	return contract.TaskHandoffHeading + contract.RenderTaskHandoff(taskContext), nil
}
