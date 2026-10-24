//go:build darwin

// seatbelt_darwin.go 在 macOS 上用 sandbox-exec 包裹 host 后端命令：
// 默认拒绝网络与工作区外写入，写权限跟随 Policy.Write 档位。
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

// SeatbeltWriteMode 决定文件写入是否放开到工作区。
type SeatbeltWriteMode string

const (
	// SeatbeltWriteNone 仅允许临时目录写入（只读档）。
	SeatbeltWriteNone SeatbeltWriteMode = "none"
	// SeatbeltWriteWorkspace 允许写工作区与临时目录。
	SeatbeltWriteWorkspace SeatbeltWriteMode = "workspace"
)

// SeatbeltOptions 描述一次 host 命令的 seatbelt 约束。
type SeatbeltOptions struct {
	WorkspaceRoot string
	WriteMode     SeatbeltWriteMode
	AllowNetwork  bool
}

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
	// 临时目录：编译器/测试缓存常用
	b.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/var/folders\"))\n")
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
