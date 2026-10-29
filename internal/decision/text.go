// Package decision 补充决策账本的文本边界校验：正文必须长度有界，且不得
// 携带凭据式内容、绝对路径、家目录引用或路径跳转片段。该文件只做拒绝，
// 不做静默改写，避免决策结论被悄悄篡改后仍冒充原始表述。
//
// Author: fxbin
package decision

import (
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
)

// containsCredentialMaterial 判断文本是否含有凭据式内容。判定复用制品存储
// 既有的脱敏探测：同一套模式同时用于写入前拦截与写入后脱敏，避免账本与
// 制品对“什么算凭据”给出两套答案。
func containsCredentialMaterial(value string) bool {
	return string(artifact.Redact([]byte(value))) != value
}

// containsAbsolutePath 判断文本是否含有绝对路径、家目录引用、盘符路径、
// UNC 路径或路径跳转片段。相对路径予以保留：它不泄露宿主目录布局。
func containsAbsolutePath(value string) bool {
	for _, token := range splitPathTokens(value) {
		if looksLikeAbsolutePath(token) {
			return true
		}
	}
	return false
}

// splitPathTokens 按空白与常见包裹符号切分文本，得到可独立判定的路径片段。
func splitPathTokens(value string) []string {
	return strings.FieldsFunc(value, isPathTokenSeparator)
}

// isPathTokenSeparator 判断字符是否用于切分路径片段。
func isPathTokenSeparator(character rune) bool {
	switch character {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>',
		'，', '。', '；', '：', '、', ',', ';', '！', '？':
		return true
	default:
		return false
	}
}

// looksLikeAbsolutePath 判断单个片段是否泄露宿主目录布局。
func looksLikeAbsolutePath(token string) bool {
	if token == "" {
		return false
	}
	if strings.HasPrefix(token, "~/") || strings.HasPrefix(token, "~\\") {
		return true
	}
	if strings.HasPrefix(token, "/") && len(token) > 1 {
		return true
	}
	if strings.HasPrefix(token, "\\\\") && len(token) > 2 {
		return true
	}
	if isWindowsDrivePath(token) {
		return true
	}
	if scheme, remainder, found := strings.Cut(token, "://"); found && scheme != "" {
		return looksLikeAbsolutePath(remainder)
	}
	return hasTraversalSegment(token)
}

// isWindowsDrivePath 判断片段是否形如盘符加根，例如 C:\Users 或 c:/tmp。
func isWindowsDrivePath(token string) bool {
	if len(token) < 3 {
		return false
	}
	if !isASCIILetter(token[0]) || token[1] != ':' {
		return false
	}
	return token[2] == '\\' || token[2] == '/'
}

// hasTraversalSegment 判断片段是否含路径跳转段，避免相对路径逃逸工作区。
func hasTraversalSegment(token string) bool {
	for _, segment := range strings.FieldsFunc(token, func(character rune) bool {
		return character == '/' || character == '\\'
	}) {
		if segment == ".." {
			return true
		}
	}
	return false
}

// isASCIILetter 判断字节是否为 ASCII 字母。
func isASCIILetter(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}
