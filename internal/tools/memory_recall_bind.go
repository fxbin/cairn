// memory_recall_bind.go 提供 memory_recall 回调的进程内装配点。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"sync"
)

// MemoryExplainFunc 由应用层装配：将封闭请求映射为 EvidenceLedger JSON。
type MemoryExplainFunc func(ctx context.Context, request MemoryRecallRequest) (json.RawMessage, error)

var (
	memoryExplainMu sync.RWMutex
	memoryExplain   MemoryExplainFunc
)

// BindMemoryExplain 装配或清除召回回调；nil 表示断开。
func BindMemoryExplain(fn MemoryExplainFunc) {
	memoryExplainMu.Lock()
	memoryExplain = fn
	memoryExplainMu.Unlock()
}

// CurrentMemoryExplain 返回当前装配的召回回调；供嵌套运行（模型侧委派）
// 在子运行返回后恢复外层绑定，避免父 Loop 使用已关闭的子资源。
func CurrentMemoryExplain() MemoryExplainFunc {
	return defaultMemoryExplain()
}

// defaultMemoryExplain 读取当前回调。
func defaultMemoryExplain() MemoryExplainFunc {
	memoryExplainMu.RLock()
	defer memoryExplainMu.RUnlock()
	return memoryExplain
}
