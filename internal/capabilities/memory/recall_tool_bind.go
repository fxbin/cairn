// [INPUT]: 依赖封闭召回请求与JSON回执。
// [OUTPUT]: 提供MemoryExplainFunc实例回调合同。
// [POS]: 能力由Boot显式接线，不保存进程级默认回调。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package memory 承载受控记忆能力。
// Author: fxbin
package memory

import (
	"context"
	"encoding/json"
)

// MemoryExplainFunc 由应用层装配：将封闭请求映射为 EvidenceLedger JSON。
type MemoryExplainFunc func(ctx context.Context, request MemoryRecallRequest) (json.RawMessage, error)
