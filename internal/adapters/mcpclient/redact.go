// Package mcpclient 在外部结果落盘前精确移除本次会话实际使用的凭据。
// Author: fxbin
package mcpclient

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/credential"
)

// secretFilter 保存私有内存中的实际凭据；刷新和 HTTP 请求可并发添加，值不落盘。
type secretFilter struct {
	mu     sync.RWMutex
	values []string
}

// add 收录非空值并优先匹配长值，防止短值替换后残留长凭据的后缀。
func (filter *secretFilter) add(values ...string) {
	filter.mu.Lock()
	defer filter.mu.Unlock()
	for _, value := range values {
		if value == "" {
			continue
		}
		found := false
		for _, existing := range filter.values {
			if value == existing {
				found = true
				break
			}
		}
		if !found {
			filter.values = append(filter.values, value)
		}
	}
	sort.Slice(filter.values, func(i, j int) bool { return len(filter.values[i]) > len(filter.values[j]) })
}

// text 先移除已知值，再复用凭据模式脱敏，无法从返回值恢复实际密钥。
func (filter *secretFilter) text(value string) string {
	if filter != nil {
		filter.mu.RLock()
		for _, secret := range filter.values {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
		filter.mu.RUnlock()
	}
	return string(artifact.Redact([]byte(value)))
}

// json 逐个解码字符串 token 后脱敏并重新转义，保留 JSON 数字和对象结构。
// schema 的凭据式属性名仍保留原有类型，避免把合法的属性定义替换为字符串。
func (filter *secretFilter) json(content []byte) []byte {
	var output bytes.Buffer
	pendingSensitive := false
	for start := 0; start < len(content); {
		if content[start] != '"' {
			if content[start] != ' ' && content[start] != '\t' && content[start] != '\n' && content[start] != '\r' && content[start] != ':' {
				pendingSensitive = false
			}
			output.WriteByte(content[start])
			start++
			continue
		}
		end := start + 1
		for end < len(content) {
			if content[end] == '\\' {
				end += 2
				continue
			}
			if content[end] == '"' {
				end++
				break
			}
			end++
		}
		if end > len(content) {
			return []byte(filter.text(string(content)))
		}
		var value string
		if json.Unmarshal(content[start:end], &value) != nil {
			return []byte(filter.text(string(content)))
		}
		next := end
		for next < len(content) && (content[next] == ' ' || content[next] == '\t' || content[next] == '\n' || content[next] == '\r') {
			next++
		}
		isKey := next < len(content) && content[next] == ':'
		sanitized := filter.text(value)
		if !isKey && pendingSensitive {
			sanitized = "[REDACTED]"
		}
		pendingSensitive = isKey && credential.Key(value) && !strings.Contains(strings.ToLower(value), "tokens")
		encoded, _ := json.Marshal(sanitized)
		output.Write(encoded)
		start = end
	}
	return output.Bytes()
}
