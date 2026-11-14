// send_message_bind.go 定义工具实例的显式回调合同，不保存进程级绑定。
// Author: fxbin
package tools

import (
	"context"
)

// MessageDeliverFunc 是应用层写入对端 Journal 的回调。
type MessageDeliverFunc func(ctx context.Context, toRunID, content, reason string) error
