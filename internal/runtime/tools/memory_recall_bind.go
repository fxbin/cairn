// memory_recall_bind.go 定义工具实例的显式回调合同，不保存进程级绑定。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
)

// MemoryExplainFunc 由应用层装配：将封闭请求映射为 EvidenceLedger JSON。
type MemoryExplainFunc func(ctx context.Context, request MemoryRecallRequest) (json.RawMessage, error)
