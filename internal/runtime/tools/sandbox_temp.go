// sandbox_temp.go 决定原生沙箱始终可写的临时根：平台固定的系统临时根，加上当前进程
// 实际使用的临时目录。环境白名单默认把 TMPDIR 传给沙箱内的命令，可写名单必须与之一致；
// 否则继承自定义 TMPDIR 的命令会在创建工作目录或写临时文件时被沙箱拒绝。
//
// [INPUT]: 固定临时根、当前进程的临时目录环境（临时目录、主目录、规范路径解析）、工作区根与写入档位。
// [OUTPUT]: 可写临时根清单，或失败关闭的错误。
// [POS]: 只做路径与字符串判断，不执行进程、不写文件；读取环境与文件系统的动作由 sandboxTempInput 注入。
// [PROTOCOL]: Update this header on change, then check AGENTS.md
//
// Author: fxbin
package tools

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// sandboxCaseInsensitive 表示本平台的路径按不区分大小写比较。macOS 的 APFS 默认不区分
// 大小写，而规范路径解析不会统一大小写；按原样比较会让换个大小写拼写的同一目录绕过
// 过宽防护。在区分大小写的文件系统上这样比较只会多判一次过宽，方向安全。
var sandboxCaseInsensitive = runtime.GOOS == "darwin"

// sandboxTempInput 是推导临时根所需的外部输入。生产代码经 currentSandboxTempInput 取真实
// 实现；测试传入固定值，不需要改动任何包级状态。
type sandboxTempInput struct {
	// TempDir 返回当前进程使用的临时目录。
	TempDir func() string
	// HomeDir 返回当前用户主目录；取不到时返回错误。
	HomeDir func() (string, error)
	// ResolveDir 把目录解析为不含符号链接的规范路径；目标不存在或不是目录时返回错误。
	ResolveDir func(string) (string, error)
}

// sandboxTempScope 描述当前命令的写入范围，供过宽防护判断临时目录是否覆盖工作区。
type sandboxTempScope struct {
	// WorkspaceRoot 是工作区根的规范路径；为空表示没有工作区。
	WorkspaceRoot string
	// WorkspaceWritable 为 true 表示本次命令允许写工作区。
	WorkspaceWritable bool
	// ProvidedRoots 是沙箱自身已经提供可写的宿主路径。临时目录落在其下时不需要追加
	// 任何规则，也不做过宽与主目录核对：这里没有新增可写位置。
	ProvidedRoots []string
}

// currentSandboxTempInput 返回读取真实环境的输入。
func currentSandboxTempInput() sandboxTempInput {
	return sandboxTempInput{TempDir: os.TempDir, HomeDir: currentUserHomeDir, ResolveDir: resolveSandboxDir}
}

// currentUserHomeDir 依次从环境变量与用户数据库取主目录；两处都取不到时返回错误。
func currentUserHomeDir() (string, error) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home, nil
	}
	if current, err := user.Current(); err == nil && current.HomeDir != "" {
		return current.HomeDir, nil
	}
	return "", errors.New("current user home directory is unavailable")
}

// resolveSandboxDir 把目录解析为规范路径。沙箱内核按规范路径匹配写规则（macOS 的 /var
// 指向 /private/var），放行与过宽防护因此都基于规范路径。
func resolveSandboxDir(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return canonical, nil
}

// sandboxTempScopeOf 把写入档位与工作区根整理成过宽防护使用的形态。工作区根解析不了时
// 按原样比较：这是兜底而不是完整校验，原样路径含符号链接时可能漏判。生产路径上工作区根
// 已由 NewWorkspace 解析过，走不到这里；参数层的用例会传入并不存在的工作区根。
func sandboxTempScopeOf(workspaceRoot string, writable bool, input sandboxTempInput) sandboxTempScope {
	if strings.TrimSpace(workspaceRoot) == "" {
		return sandboxTempScope{}
	}
	canonical, err := input.ResolveDir(filepath.Clean(workspaceRoot))
	if err != nil {
		canonical = filepath.Clean(workspaceRoot)
	}
	return sandboxTempScope{WorkspaceRoot: canonical, WorkspaceWritable: writable}
}

// sandboxTempRoots 返回沙箱始终可写的临时根：固定根在前，必要时追加当前临时目录的规范
// 路径。传入的切片不会被修改；返回值的前 len(fixed) 项与 fixed 逐项相同。
//
// 临时目录已位于某个固定根之下时不追加，默认环境因此与没有这条规则时完全一致。无法
// 解析为绝对目录（相对路径、不存在、不是目录）时保持固定根不变：沙箱无从放行一个无法
// 确认的位置，行为与此前一致。能解析但过宽，或无法核对它是否吞掉用户主目录时失败关闭
// ——过宽的临时根会让写边界名存实亡，此时宁可让命令明确失败。
func sandboxTempRoots(fixed []string, input sandboxTempInput, scope sandboxTempScope) ([]string, error) {
	roots := make([]string, 0, len(fixed)+1)
	roots = append(roots, fixed...)
	tempDir := input.TempDir()
	if tempDir == "" || !filepath.IsAbs(tempDir) {
		return roots, nil
	}
	canonical, err := input.ResolveDir(filepath.Clean(tempDir))
	if err != nil {
		return roots, nil
	}
	if pathWithinAny(canonical, fixed) || pathWithinAny(canonical, scope.ProvidedRoots) {
		return roots, nil
	}
	if reason := broadTempRootReason(canonical, fixed, scope); reason != "" {
		return nil, fmt.Errorf("sandbox temp dir %q is too broad to be writable inside the sandbox: %s; point TMPDIR at a dedicated directory", tempDir, reason)
	}
	reason, unavailable := homeOverlapReason(canonical, input)
	if unavailable {
		return nil, fmt.Errorf("sandbox temp dir %q cannot be granted inside the sandbox: %s; set HOME to an absolute directory other than the filesystem root, or unset the custom TMPDIR", tempDir, reason)
	}
	if reason != "" {
		return nil, fmt.Errorf("sandbox temp dir %q is too broad to be writable inside the sandbox: %s; point TMPDIR at a dedicated directory", tempDir, reason)
	}
	return append(roots, canonical), nil
}

// broadTempRootReason 判断把 root 作为可写临时根是否过宽；过宽时返回原因，否则返回空串。
//
// 文件系统根与会吞掉某个固定根的目录都过宽。只读档下，覆盖工作区的临时目录同样过宽：
// 放行它会让工作区经临时目录规则变得可写。可写档下则由临时写规则管辖，与工作区位于
// 系统临时根之下时的既有行为一致——测试与评测框架通常把工作区建在临时目录之下，在那里
// 拒绝会让整套沙箱命令无法执行。
func broadTempRootReason(root string, fixed []string, scope sandboxTempScope) string {
	if filepath.Dir(root) == root {
		return "it is the filesystem root"
	}
	for _, candidate := range fixed {
		if !sameSandboxPath(candidate, root) && pathWithin(candidate, root) {
			return fmt.Sprintf("it contains the fixed temp root %s", candidate)
		}
	}
	if scope.WorkspaceRoot != "" && !scope.WorkspaceWritable && pathWithin(scope.WorkspaceRoot, root) {
		return "it covers the workspace " + scope.WorkspaceRoot + ", which this command must not write"
	}
	return ""
}

// homeOverlapReason 报告 root 与用户主目录的重叠。主目录取不到、为空、不是绝对路径或就是
// 文件系统根时返回 unavailable=true：此时无法核对，按失败关闭处理，原因里说明缺什么。
func homeOverlapReason(root string, input sandboxTempInput) (reason string, unavailable bool) {
	home, err := input.HomeDir()
	home = strings.TrimSpace(home)
	if err != nil || home == "" || !filepath.IsAbs(home) || filepath.Clean(home) == string(filepath.Separator) {
		return "the user home directory is missing, relative or the filesystem root, so overlap with it cannot be checked", true
	}
	canonicalHome, err := input.ResolveDir(filepath.Clean(home))
	if err != nil {
		canonicalHome = filepath.Clean(home)
	}
	if sameSandboxPath(root, canonicalHome) {
		return "it is the user home directory", false
	}
	if pathWithin(canonicalHome, root) {
		return "it contains the user home directory " + canonicalHome, false
	}
	return "", false
}

// sameSandboxPath 报告两个路径是否指向同一位置，比较口径与 pathWithin 一致。
func sameSandboxPath(left, right string) bool {
	if sandboxCaseInsensitive {
		return strings.EqualFold(left, right)
	}
	return left == right
}

// pathWithin 报告 path 是否位于 root 之下，含两者相等；root 是文件系统根时恒为真。
func pathWithin(path, root string) bool {
	if sandboxCaseInsensitive {
		path, root = strings.ToLower(path), strings.ToLower(root)
	}
	return path == root || root == "/" || strings.HasPrefix(path, root+"/")
}

// pathWithinAny 报告 path 是否位于 roots 中任一目录之下。
func pathWithinAny(path string, roots []string) bool {
	for _, root := range roots {
		if pathWithin(path, root) {
			return true
		}
	}
	return false
}
