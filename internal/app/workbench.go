// [INPUT]: 依赖 Coding 工作台的纯装配合同。
// [OUTPUT]: 向界面层暴露标准检查身份，不暴露工作台执行实现。
// [POS]: 应用门面按需转发工作台模板；命令与权限仍来自通用验证链。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接工作台模板与上层界面装配。
// Author: fxbin
package app

import "github.com/fxbin/cairn/internal/capabilities/workbench/coding"

// CodingCheckIDs 返回 Coding 工作台的标准检查身份副本，供 CLI 在用户显式
// 选择工作台模板时按位置绑定。它只提供命名约定，不授予执行权限。
func CodingCheckIDs() []string {
	return coding.StandardChecks()
}
