// sensitive_key.go 把凭据名称词干判定收敛到合同层：配置装载、制品脱敏、
// 工具参数与决策账本共用同一份词表，避免各自维护导致漏判或误伤。
//
// Author: fxbin
package contract

import "strings"

// SensitiveKey 判断名称是否含有常见凭据词干；调用方可在此基础上排除明确的
// 非敏感计数字段。名称会统一转为小写并把连字符规范化为下划线。
// 词干刻意保持保守：auth/key/pw 等独立短词会误伤 author、primary_key、
// pwd/OLDPWD 等常见非凭据名称，只收录完整词或复合形式。
func SensitiveKey(name string) bool {
	name = strings.ToLower(strings.ReplaceAll(name, "-", "_"))
	return strings.Contains(name, "token") || strings.Contains(name, "secret") ||
		strings.Contains(name, "password") || strings.Contains(name, "passwd") ||
		strings.Contains(name, "passphrase") ||
		strings.Contains(name, "api_key") || strings.Contains(name, "apikey") ||
		strings.Contains(name, "credential") || strings.Contains(name, "access_key") ||
		strings.Contains(name, "private_key") || strings.Contains(name, "client_key") ||
		strings.Contains(name, "session_key") || strings.Contains(name, "signing_key") ||
		strings.Contains(name, "license_key") || strings.HasSuffix(name, "_pat")
}
