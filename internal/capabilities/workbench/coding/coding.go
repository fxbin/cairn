// coding.go 定义 Coding 工作台的装配合同：稳定身份、版本与标准检查目录。
// 工作台不是执行引擎——这里只提供不透明检查身份的命名约定，命令仍由通用
// 验证引擎经 Policy、审批与沙箱执行。Git、Diff 与语言语义只留在本包。
//
// Author: fxbin
package coding

const (
	// WorkbenchID 是 Coding 工作台的稳定身份；不作为任务或会话身份使用。
	WorkbenchID = "coding"
	// WorkbenchVersion 是工作台装配合同的版本。
	WorkbenchVersion = 1
	// CheckBuild 构建检查身份。
	CheckBuild = "build"
	// CheckTest 测试检查身份。
	CheckTest = "test"
	// CheckLint 静态检查身份。
	CheckLint = "lint"
	// CheckDiff 差异检查身份。
	CheckDiff = "diff"
)

// StandardChecks 返回固定顺序的标准检查身份副本。它只提供命名约定，
// 不授予执行权限；实际命令由工作区配置的 verification.commands[].id 绑定。
func StandardChecks() []string {
	return []string{CheckBuild, CheckTest, CheckLint, CheckDiff}
}
