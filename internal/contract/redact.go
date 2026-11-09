// redact.go 提供文本与 JSON 载荷的凭据脱敏合同：事件持久化、工具结果和
// 制品写入共用同一实现，避免各入口产生不同的脱敏输出。
//
// Author: fxbin
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(bearer)\s+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\bsk-[a-zA-Z0-9_-]{12,}`),
}

// credentialAssignmentPattern 捕获带引号或足够长裸值的候选赋值；闭合引号值
// 保留既有长度阈值，未闭合到 EOF 的非空片段也脱敏，防止输出截断后漏值。
// 键名敏感性统一交给 SensitiveKey，避免文本、JSON、配置和工具各自维护词干。
var credentialAssignmentPattern = regexp.MustCompile(`(?i)\b[A-Za-z_][A-Za-z0-9_-]*["']?\s*[:=]\s*(?:"[^"]{4,}"|'[^']{4,}'|"[^"]+$|'[^']+$|[A-Za-z0-9+/=_-]{16,})`)

// credentialEnvAssignmentPattern 只捕获行首的“环境变量=值”赋值（.env 与 shell
// 导出风格）。与上面的通用赋值模式不同，它行锚定且不限值长度：短的
// DB_PASSWORD=hunter2 同样必须清除，而代码中带空格的比较或赋值
// （password == ""、token := x）不会落在行首裸赋值形态上而被误伤。
var credentialEnvAssignmentPattern = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)=([^[:space:]"']+)`)

// Redact 按已知凭据模式替换文本中可能泄露的敏感值。
func Redact(content []byte) []byte {
	clean := string(content)
	for _, pattern := range secretPatterns {
		clean = pattern.ReplaceAllStringFunc(clean, func(match string) string {
			if strings.HasPrefix(strings.ToLower(match), "bearer ") {
				return "Bearer [REDACTED]"
			}
			if index := strings.IndexAny(match, ":="); index >= 0 {
				return match[:index+1] + "[REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	clean = credentialAssignmentPattern.ReplaceAllStringFunc(clean, func(match string) string {
		index := strings.IndexAny(match, ":=")
		if index < 0 {
			return match
		}
		key := strings.Trim(strings.TrimSpace(match[:index]), "\"'")
		if !SensitiveKey(key) {
			return match
		}
		return match[:index+1] + "[REDACTED]"
	})
	clean = credentialEnvAssignmentPattern.ReplaceAllStringFunc(clean, func(match string) string {
		index := strings.Index(match, "=")
		if index < 0 || !SensitiveKey(match[:index]) {
			return match
		}
		return match[:index+1] + "[REDACTED]"
	})
	return []byte(clean)
}

// RedactJSON 保留有效 JSON 结构和数字精度，再递归清除凭据式键及字符串内容。
func RedactJSON(content []byte) []byte {
	if !json.Valid(content) {
		return Redact(content)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Redact(content)
	}
	scrubJSON(&value, "")
	encoded, err := json.Marshal(value)
	if err != nil {
		return Redact(content)
	}
	// 已完成递归脱敏的 value 重新编码后必定仍是 JSON。此处不能再把它交给
	// 面向任意文本的 Redact：源码字符串中的 credential-like 片段可能含有
	// 反斜杠，文本替换会破坏 JSON 转义并使后续事件无法持久化。
	return encoded
}

// EncodeRedactedJSON 将值序列化为脱敏后的事件载荷 JSON；失败返回错误，
// 不 panic，避免进程因单条载荷不可编码而崩溃并留下半截运行。
func EncodeRedactedJSON(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode event payload: %w", err)
	}
	return append(json.RawMessage(nil), RedactJSON(encoded)...), nil
}

// MustEncodeRedactedJSON 是测试与脚本化计划使用的便捷包装；生产路径应
// 优先使用 EncodeRedactedJSON 并处理错误。
func MustEncodeRedactedJSON(value any) json.RawMessage {
	raw, err := EncodeRedactedJSON(value)
	if err != nil {
		panic(err)
	}
	return raw
}

// scrubJSON 递归替换敏感键及字符串叶子节点中的凭据内容。
func scrubJSON(value *any, key string) {
	if sensitiveKey(key) {
		*value = "[REDACTED]"
		return
	}
	switch current := (*value).(type) {
	case map[string]any:
		for nestedKey, nested := range current {
			scrubJSON(&nested, nestedKey)
			current[nestedKey] = nested
		}
	case []any:
		for index, nested := range current {
			scrubJSON(&nested, "")
			current[index] = nested
		}
	case string:
		*value = string(Redact([]byte(current)))
	}
}

// sensitiveKey 判断规范化后的 JSON 键是否可能承载凭据；用量计数和预算上限
// 虽然包含 tokens 字样，但属于审计与预算事实，必须保留。
func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	return !strings.HasSuffix(key, "tokens") && SensitiveKey(key)
}
