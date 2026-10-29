//go:build darwin

// seatbelt_darwin.go 在 macOS 上用 sandbox-exec 包裹 host 后端命令：
// 默认拒绝网络与工作区外写入，写权限跟随 Policy.Write 档位。
// 临时目录写规则不得覆盖工作区父目录，否则"写入工作区父目录"这类
// 越界写会在工作区位于临时目录之内时被放行。
// 与 Codex/DSH 同构：OS 原生沙箱 + 工作区写 + 越界审批。
// Author: fxbin
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SeatbeltExecutable 是 macOS 自带的沙箱入口。
const SeatbeltExecutable = "/usr/bin/sandbox-exec"

// SeatbeltProfileVersion 是 seatbelt 配置文件版本头。
const SeatbeltProfileVersion = "1"

// seatbeltAlwaysWritableRoots 是始终允许写入的目录根：临时目录写规则覆盖
// 这些路径及其子路径。工作区父目录恰好是其中之一时不能再叠加拒绝规则，
// 否则临时写（编译器与测试缓存）会被一并拒绝。
var seatbeltAlwaysWritableRoots = []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}

// SeatbeltAvailable 判断 seatbelt 入口是否存在。
func SeatbeltAvailable() bool {
	info, err := os.Stat(SeatbeltExecutable)
	return err == nil && !info.IsDir()
}

// SeatbeltProfile 生成 sandbox-exec 配置文本：默认拒绝，按档位放行。
func SeatbeltProfile(options SeatbeltOptions) (string, error) {
	if strings.TrimSpace(options.WorkspaceRoot) == "" {
		return "", fmt.Errorf("workspace root is required for seatbelt profile")
	}
	root := filepath.Clean(options.WorkspaceRoot)
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("workspace root must be absolute")
	}
	// Seatbelt 对路径中的空格敏感；先失败关闭而不是生成错误 profile。
	if strings.ContainsAny(root, " \t\n\"") {
		return "", fmt.Errorf("workspace root must not contain whitespace or quotes")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "(version %s)\n", SeatbeltProfileVersion)
	b.WriteString("(deny default)\n")
	// 进程与基础只读面
	b.WriteString("(allow process-exec)\n")
	b.WriteString("(allow process-fork)\n")
	b.WriteString("(allow process-info-pidinfo)\n")
	b.WriteString("(allow process-info-pidfdinfo)\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow mach-lookup)\n")
	b.WriteString("(allow file-ioctl)\n")
	b.WriteString("(allow file-read-metadata)\n")
	b.WriteString("(allow file-read*)\n")
	b.WriteString("(allow file-write-data (literal \"/dev/null\"))\n")
	b.WriteString("(allow file-write-data (literal \"/dev/stdout\"))\n")
	b.WriteString("(allow file-write-data (literal \"/dev/stderr\"))\n")
	// 临时目录：编译器/测试缓存常用。内核按符号链接解析后的规范路径匹配
	// subpath 规则，macOS 用户级临时目录实际位于 /private/var/folders
	//（/var 是指向 /private/var 的符号链接），仅写 /var/folders 在真实系统
	// 上不可达，必须同时放行规范路径。
	b.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/var/folders\"))\n")
	b.WriteString("(allow file-write* (subpath \"/private/var/folders\"))\n")
	// 工作区父目录：临时目录写规则覆盖整棵临时子树，工作区位于临时目录
	// 之内时"写入工作区父目录"会被这条规则放行。这里显式拒绝父目录子树；
	// seatbelt 按最后匹配者生效，父目录拒绝在前、工作区放行在后，工作区内
	// 的写入仍然可用，越出工作区的写入（含父目录）被拒。
	if boundary := workspaceParentWriteBoundary(root); boundary != "" {
		fmt.Fprintf(&b, "(deny file-write* (subpath \"%s\"))\n", boundary)
	}
	if options.WriteMode == SeatbeltWriteWorkspace {
		fmt.Fprintf(&b, "(allow file-write* (subpath \"%s\"))\n", root)
	}
	if options.AllowNetwork {
		b.WriteString("(allow network*)\n")
	} else {
		b.WriteString("(deny network*)\n")
	}
	return b.String(), nil
}

// workspaceParentWriteBoundary 返回必须拒绝写入的工作区父目录。父目录本身
// 是文件系统根或始终可写目录（临时目录根）时不施加该边界：拒绝它们会连带
// 拒绝临时写与设备文件写，边界因此退化为临时目录写规则本身。
func workspaceParentWriteBoundary(root string) string {
	parent := filepath.Dir(root)
	if parent == root || parent == string(filepath.Separator) {
		return ""
	}
	for _, alwaysWritable := range seatbeltAlwaysWritableRoots {
		if parent == alwaysWritable {
			return ""
		}
	}
	return parent
}

// WrapSeatbelt 将 argv 包成 sandbox-exec 调用；profile 为空时失败关闭。
func WrapSeatbelt(profilePath string, argv []string) ([]string, error) {
	if strings.TrimSpace(profilePath) == "" {
		return nil, fmt.Errorf("seatbelt profile path is required")
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("argv is required")
	}
	wrapped := []string{SeatbeltExecutable, "-f", profilePath, "--"}
	return append(wrapped, argv...), nil
}

// HostWriteModeFromPolicy 将写策略字符串映射到 seatbelt 写档。
func HostWriteModeFromPolicy(writePolicy string) SeatbeltWriteMode {
	switch strings.ToLower(strings.TrimSpace(writePolicy)) {
	case "deny":
		return SeatbeltWriteNone
	default:
		return SeatbeltWriteWorkspace
	}
}
