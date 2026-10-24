// memory 系列子命令。
// Author: fxbin
package cli

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (command Command) memory(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "memory requires capture, recall, explain, inspect, revoke, consolidate, reject, candidate, promotion, or telemetry")
		return ExitUsage
	}
	switch arguments[0] {
	case "capture":
		return command.memoryCapture(ctx, arguments[1:])
	case "recall":
		return command.memoryRecall(ctx, arguments[1:])
	case "explain":
		return command.memoryExplain(ctx, arguments[1:])
	case "inspect":
		return command.memoryInspectDispatch(ctx, arguments[1:])
	case "revoke":
		return command.memoryRevoke(ctx, arguments[1:])
	case "consolidate":
		return command.memoryConsolidate(ctx, arguments[1:])
	case "reject":
		return command.memoryRejectDerived(ctx, arguments[1:])
	case "candidate":
		return command.memoryCandidate(ctx, arguments[1:])
	case "promotion":
		return command.memoryPromotion(ctx, arguments[1:])
	case "telemetry":
		return command.memoryTelemetry(ctx, arguments[1:])
	default:
		fmt.Fprintln(command.Stderr, "memory requires capture, recall, explain, inspect, revoke, consolidate, reject, candidate, promotion, or telemetry")
		return ExitUsage
	}
}

// memoryCandidate 分派演化候选的固定动作：创建必须由人工显式提供闭合变体
// 参数；评测驱动沙箱执行器并要求候选已就绪；拒绝使用封闭理由；作废与过期
// 为显式终态迁移；解释只读。不存在任何自动生成候选或晋升入口。
func evidenceRef(digest string) string {
	if digest == "" {
		return "pending"
	}
	return digest
}

// memoryConsolidate 在工作区锁内执行一次事务化巩固，并把提交的计划、派生候选与
// 被替代版本以人类或 NDJSON 视图输出；不接入运行时召回。
func parseUTCTimestamp(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC {
		return nil, errors.New("timestamp is not UTC RFC3339")
	}
	return &parsed, nil
}

// memoryCapture 解析固定 Run 身份与 TTL，从已校验终态 Journal 写入结构化候选。
func normalizeMemoryPositionals(arguments []string) []string {
	return normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {}, "--reason": {},
	})
}
