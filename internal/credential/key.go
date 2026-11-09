// Package credential 提供跨配置、工具与制品边界一致的凭据名称识别规则。
// 词表本身归合同层（contract.SensitiveKey），本包保留既有入口以兼容调用方。
package credential

import "github.com/fxbin/cairn/internal/contract"

// Key 判断名称是否含有常见凭据词干；规则与词表由合同层统一维护，
// 这里只做入口兼容，避免调用方因包边界调整而重复实现判定。
func Key(name string) bool {
	return contract.SensitiveKey(name)
}
