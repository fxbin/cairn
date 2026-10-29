package decision

// 本文件补充决策账本的文本边界校验：正文必须长度有界，且不得携带凭据式
// 内容、绝对路径、家目录引用或路径跳转片段。该文件只做拒绝，不做静默
// 改写，避免决策结论被悄悄篡改后仍冒充原始表述。

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/credential"
)

// 凭据形态模式：常见服务提供商的固定前缀与固定长度格式。这些形态不依赖
// 键名，裸写在正文里也必须被拒绝。
var credentialFormatPatterns = []*regexp.Regexp{
	// AWS 访问密钥 ID：AKIA 或 ASIA 前缀加 16 位大写字母数字。
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	// GitHub 令牌：ghp_、gho_、ghu_、ghs_、ghr_ 前缀。
	regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`),
	// GitHub 细粒度令牌：github_pat_ 前缀。
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	// Google API 密钥：AIza 前缀加 35 位。
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	// Slack 令牌：xox 前缀族。
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	// npm 访问令牌：npm_ 前缀。
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}\b`),
	// JSON Web Token：以 eyJ 开头的 base64url 段序列。
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}(?:\.[A-Za-z0-9_-]*)`),
	// PEM 私钥块。
	regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`),
}

// containsCredentialMaterial 判断文本是否含有凭据式内容。判定分三层：
// 一是复用制品存储的脱敏探测，覆盖 Bearer、sk- 前缀与键值赋值形态；二是
// 常见服务提供商的固定格式，裸写也命中；三是凭据词干后紧跟的裸值，
// 例如 password hunter2。
func containsCredentialMaterial(value string) bool {
	if string(artifact.Redact([]byte(value))) != value {
		return true
	}
	for _, pattern := range credentialFormatPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return containsCredentialWordValue(value)
}

// containsCredentialWordValue 判断文本是否出现凭据词干后紧跟的裸值。词干
// 判定复用 internal/credential 的既有规则，避免账本另立一套凭据名称清单；
// 值必须具备凭据形态（纯 ASCII、够长且同时含数字与字母，或足够长的字母
// 串），以免把“token 过期后不再冒充现行事实”这类正常表述误判为泄露。
func containsCredentialWordValue(value string) bool {
	tokens := credentialTokens(value)
	for index := 0; index+1 < len(tokens); index++ {
		word := strings.ToLower(tokens[index])
		// 以 tokens 结尾的字段多是计量口径，不是凭据名称，与制品存储的
		// 敏感键判定保持同一取舍。
		if strings.HasSuffix(word, "tokens") || !credential.Key(word) {
			continue
		}
		if isSecretLikeValue(tokens[index+1]) {
			return true
		}
	}
	return false
}

// isSecretLikeValue 判断片段是否像凭据值：只接受纯 ASCII，长度有下界，
// 且同时含有数字与字母；纯字母的长串也按凭据处理。
func isSecretLikeValue(token string) bool {
	if len(token) < 6 || len(token) > 256 {
		return false
	}
	hasDigit := false
	hasLetter := false
	for index := 0; index < len(token); index++ {
		character := token[index]
		switch {
		case character >= '0' && character <= '9':
			hasDigit = true
		case (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z'):
			hasLetter = true
		default:
			return false
		}
	}
	if hasDigit && hasLetter {
		return true
	}
	return len(token) >= 16 && hasLetter
}

// credentialTokens 把文本切成用于凭据词干匹配的词元；分隔符包括空白与
// 常见包裹符号、等号与冒号，使“password hunter2”“password: hunter2”与
// “password=hunter2”得到同样的词元序列。
func credentialTokens(value string) []string {
	return strings.FieldsFunc(value, isCredentialTokenSeparator)
}

// isCredentialTokenSeparator 判断字符是否用于切分凭据词元。
func isCredentialTokenSeparator(character rune) bool {
	switch character {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>',
		',', ';', '=', ':', '、', '，', '。', '；', '：', '！', '？':
		return true
	default:
		return false
	}
}

// containsControlRune 判断文本是否含有除换行与制表符以外的控制字符。
func containsControlRune(value string) bool {
	for _, character := range value {
		if character == '\n' || character == '\t' {
			continue
		}
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
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
