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

// defaultMemoryExplain 读取当前回调。
func defaultMemoryExplain() MemoryExplainFunc {
	memoryExplainMu.RLock()
	defer memoryExplainMu.RUnlock()
	return memoryExplain
}
