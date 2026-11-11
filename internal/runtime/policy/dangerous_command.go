// dangerous_command.go 识别危险命令并给出重写提示；在 bypass 权限模式下
// 仍拒绝，保证无人值守会话不被破坏。只做判定，不执行。
// Author: fxbin
package policy

import "strings"

// DangerousCommand 是一条危险命令判定与重写建议。
type DangerousCommand struct {
	ToolName    string `json:"tool_name"`
	Argv        string `json:"argv"`
	Reason      string `json:"reason"`
	RewriteHint string `json:"rewrite_hint"`
}

// dangerousPatterns 是需拦截的命令模式。
var dangerousPatterns = []struct {
	pattern string
	reason  string
	rewrite string
}{
	{"rm -rf /", "递归删除根目录", "请删除具体路径，避免 rm -rf /"},
	{"rm -rf ~", "递归删除家目录", "请删除具体子目录，避免 rm -rf ~"},
	{"rm -rf *", "通配递归删除", "请列出目标目录后逐个删除"},
	{"mkfs", "格式化文件系统", "不要在 Agent 中执行 mkfs"},
	{"dd if=", "低层磁盘写入", "不要在 Agent 中执行 dd"},
	{"shutdown", "关闭系统", "请人工执行 shutdown"},
	{"reboot", "重启系统", "请人工执行 reboot"},
	{":(){", "fork 炸弹", "禁止执行 fork 炸弹"},
}

// DetectDangerousCommand 判定 argv 是否危险；返回 nil 表示安全。
func DetectDangerousCommand(toolName, argv string) *DangerousCommand {
	lower := strings.ToLower(argv)
	for _, item := range dangerousPatterns {
		if strings.Contains(lower, item.pattern) {
			return &DangerousCommand{
				ToolName:    toolName,
				Argv:        argv,
				Reason:      item.reason,
				RewriteHint: item.rewrite,
			}
		}
	}
	return nil
}

// GuardDangerousCommand 在 bypass 权限下仍拒绝危险命令。
func GuardDangerousCommand(toolName, argv, writeMode, execMode string) (*DangerousCommand, bool) {
	dangerous := DetectDangerousCommand(toolName, argv)
	if dangerous == nil {
		return nil, true
	}
	// bypass（allow）模式也不放行
	if execMode == "allow" || writeMode == "allow" {
		return dangerous, false
	}
	return dangerous, false
}
