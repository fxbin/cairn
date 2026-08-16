// Package credential 提供跨配置、工具与制品边界一致的凭据名称识别规则。
package credential

import "strings"

// Key 判断名称是否含有常见凭据词干；调用方可在此基础上排除明确的
// 非敏感计数字段。名称会统一转为小写并把连字符规范化为下划线。
func Key(name string) bool {
	name = strings.ToLower(strings.ReplaceAll(name, "-", "_"))
	return strings.Contains(name, "token") || strings.Contains(name, "secret") ||
		strings.Contains(name, "password") || strings.Contains(name, "passwd") ||
		strings.Contains(name, "api_key") || strings.Contains(name, "apikey") ||
		strings.Contains(name, "credential") || strings.Contains(name, "access_key") ||
		strings.Contains(name, "private_key") || strings.HasSuffix(name, "_pat")
}
