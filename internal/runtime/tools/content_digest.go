// Package tools 统一提供文本内容的稳定摘要。
// Author: fxbin
package tools

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentDigest 为文本生成带算法前缀的 SHA-256 摘要，供会话输入和内容比较使用。
func ContentDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}
