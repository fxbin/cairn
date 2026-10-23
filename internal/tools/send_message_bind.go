// send_message_bind.go 提供 send_message 投递回调的进程内装配点。
// Author: fxbin
package tools

import (
	"context"
	"sync"
)

// MessageDeliverFunc 是应用层写入对端 Journal 的回调。
type MessageDeliverFunc func(ctx context.Context, toRunID, content, reason string) error

var (
	messageDeliverMu sync.RWMutex
	messageDeliver   MessageDeliverFunc
)

// BindMessageDeliver 装配或清除投递回调；nil 表示断开。
func BindMessageDeliver(fn MessageDeliverFunc) {
	messageDeliverMu.Lock()
	messageDeliver = fn
	messageDeliverMu.Unlock()
}

// defaultMessageDeliver 读取当前回调。
func defaultMessageDeliver() MessageDeliverFunc {
	messageDeliverMu.RLock()
	defer messageDeliverMu.RUnlock()
	return messageDeliver
}
