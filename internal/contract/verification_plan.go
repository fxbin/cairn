// verification_plan.go 统一冻结验证命令的规范化摘要与只读计划结构。
//
// Author: fxbin
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// VerificationPlanVersion 是冻结验证计划制品的当前格式版本。
const VerificationPlanVersion = "v1"

// VerificationCommandSpec 保存冻结计划中的命令身份和参数；JSON 字段名沿用
// 原始计划制品，保证既有命令摘要逐位兼容。
type VerificationCommandSpec struct {
	ID        string          `json:"ID"`
	Arguments json.RawMessage `json:"Arguments"`
}

// VerificationPlanSpec 是验证计划制品的只读合同投影。它只在读取侧用于核对
// 提议和冻结命令，不授予执行权限。
type VerificationPlanSpec struct {
	Version             string                    `json:"version"`
	VerificationID      string                    `json:"verification_id"`
	ConfigFingerprint   string                    `json:"config_fingerprint"`
	LastMutationSeq     uint64                    `json:"last_mutation_seq"`
	CommandsFingerprint string                    `json:"commands_fingerprint"`
	Commands            []VerificationCommandSpec `json:"commands"`
}

// FingerprintVerificationCommands 先规范化各命令参数，再按既有结构计算整条
// 命令序列的 SHA-256；写入方和读取方共用此算法避免摘要口径漂移。
func FingerprintVerificationCommands(commands []VerificationCommandSpec) (string, error) {
	canonical := make([]VerificationCommandSpec, 0, len(commands))
	for index, command := range commands {
		arguments, err := canonicalVerificationArguments(command.Arguments)
		if err != nil {
			return "", fmt.Errorf("canonicalize verification command %d arguments: %w", index, err)
		}
		canonical = append(canonical, VerificationCommandSpec{ID: command.ID, Arguments: arguments})
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode verification commands: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// canonicalVerificationArguments 以保留数字字面量的 JSON 解码和重编码消除
// 参数对象键顺序差异；任何非法参数都使证据匹配失败关闭。
func canonicalVerificationArguments(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
