package domain

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"
)

// RunID 唯一标识一次完整的 Agent 运行。
type RunID string

// TurnID 唯一标识一次运行中的模型推理轮次。
type TurnID string

// OperationID 唯一标识一次工具或验证命令操作。
type OperationID string

// AttemptID 唯一标识一次可恢复的模型请求尝试。
type AttemptID string

// NewID 使用时间戳和加密随机熵生成带指定前缀的标识符。
func NewID(prefix string) string {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		panic(fmt.Errorf("read random id entropy: %w", err))
	}
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UTC().UnixNano(), base64.RawURLEncoding.EncodeToString(entropy[:]))
}

// NewRunID 生成新的运行标识符。
func NewRunID() RunID { return RunID(NewID("run")) }

// NewTurnID 生成新的推理轮次标识符。
func NewTurnID() TurnID { return TurnID(NewID("turn")) }

// NewOperationID 生成新的工具操作标识符。
func NewOperationID() OperationID { return OperationID(NewID("op")) }

// NewAttemptID 生成新的模型请求尝试标识符。
func NewAttemptID() AttemptID { return AttemptID(NewID("attempt")) }
