// send_message.go 实现父子 run 双向跟进消息工具：校验参数并经 Deliver
// 回调把消息交给应用层写入对端 Journal；不绕过 Policy，不启动外部进程。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/ports"
)

// SendMessageMaxBytes 限制单条跟进消息体大小，避免无界写入。
const SendMessageMaxBytes = 8 * 1024

// SendMessage 需要 Deliver 才能真正投递；未装配时执行失败关闭。
type SendMessage struct {
	Deliver func(ctx context.Context, toRunID, content, reason string) error
}

// Spec 返回工具声明。
func (SendMessage) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "send_message",
		Description: "向父/子 run 投递跟进消息（对端下一轮可见）。需要 to_run_id 与 content。",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["to_run_id","content"],"properties":{"to_run_id":{"type":"string","minLength":1},"content":{"type":"string","minLength":1},"reason":{"type":"string"}}}`),
	}
}

// SendMessageRequest 是工具入参。
type SendMessageRequest struct {
	ToRunID string `json:"to_run_id"`
	Content string `json:"content"`
	Reason  string `json:"reason"`
}

// SendMessageResult 是可写入 Journal 的结果载荷。
type SendMessageResult struct {
	ToRunID string `json:"to_run_id"`
	Bytes   int    `json:"bytes"`
}

// ErrMessageNotWired 表示未装配投递通道。
var ErrMessageNotWired = errors.New("send_message deliver channel is not wired")

// Execute 校验参数并调用 Deliver；任一步失败即失败关闭。
func (tool SendMessage) Execute(ctx context.Context, raw json.RawMessage) Result {
	var request SendMessageRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return Failure("invalid_arguments", "send_message 参数解析失败")
	}
	if strings.TrimSpace(request.ToRunID) == "" {
		return Failure("invalid_arguments", "to_run_id 不能为空")
	}
	content := strings.TrimSpace(request.Content)
	if content == "" {
		return Failure("invalid_arguments", "content 不能为空")
	}
	if !utf8.ValidString(content) {
		return Failure("invalid_arguments", "content 必须是合法 UTF-8")
	}
	if len(content) > SendMessageMaxBytes {
		return Failure("message_too_large", "content 超出上限")
	}
	deliver := tool.Deliver
	if deliver == nil {
		deliver = defaultMessageDeliver()
	}
	if deliver == nil {
		return Failure("not_wired", ErrMessageNotWired.Error())
	}
	if err := deliver(ctx, request.ToRunID, content, request.Reason); err != nil {
		return Failure("deliver_failed", err.Error())
	}
	payload, err := json.Marshal(SendMessageResult{ToRunID: request.ToRunID, Bytes: len(content)})
	if err != nil {
		return Failure("internal", err.Error())
	}
	return Result{OK: true, Code: "ok", Data: payload, Preview: "消息已投递给 " + request.ToRunID, Bytes: int64(len(content))}
}
