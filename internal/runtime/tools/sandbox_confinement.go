// sandbox_confinement.go 给"沙箱规则无法生成"的失败关闭错误附上稳定原因码。错误文本
// 原样保留给命令结果；原因码不含路径，供运行前的只读诊断在不暴露本地路径的前提下，
// 提前说明命令会在启动前被拒绝。
//
// [INPUT]: 规则推导各处产生的错误。
// [OUTPUT]: 稳定原因码与对应的处置提示；可被 errors.As 识别的错误类型。
// [POS]: 纯类型与归类函数，不执行进程、不写文件。
// [PROTOCOL]: Update this header on change, then check AGENTS.md
//
// Author: fxbin
package tools

import (
	"errors"
	"fmt"
)

// SandboxConfinementReason 是沙箱规则无法生成时的稳定原因码。
type SandboxConfinementReason string

const (
	// SandboxConfinementUnavailable 表示平台没有原生沙箱，或入口程序不存在。
	SandboxConfinementUnavailable SandboxConfinementReason = "sandbox_unavailable"
	// SandboxConfinementWorkspaceRoot 表示工作区根缺失、不是绝对路径，或含无法写进规则的字符。
	SandboxConfinementWorkspaceRoot SandboxConfinementReason = "workspace_root_invalid"
	// SandboxConfinementTempDir 表示当前进程的临时目录不能在沙箱内放行：过宽、覆盖只读档下
	// 的工作区、无法核对主目录、含无法引用的字符，或落在沙箱保留的路径之下。
	SandboxConfinementTempDir SandboxConfinementReason = "temp_dir_rejected"
	// SandboxConfinementPlanFailed 表示规则推导因其他原因失败。
	SandboxConfinementPlanFailed SandboxConfinementReason = "sandbox_plan_failed"
)

// SandboxConfinementAdvice 返回原因码对应的处置提示；不含路径，可以直接展示。没有
// 提示的原因码返回空串。
func SandboxConfinementAdvice(reason SandboxConfinementReason) string {
	switch reason {
	case SandboxConfinementUnavailable:
		return "the OS sandbox for host confinement is unavailable on this platform"
	case SandboxConfinementWorkspaceRoot:
		return "workspace root must be an absolute path without whitespace or quotes"
	case SandboxConfinementTempDir:
		return "point TMPDIR at a dedicated directory, or unset the custom TMPDIR"
	case SandboxConfinementPlanFailed:
		return "sandbox arguments could not be derived"
	}
	return ""
}

// sandboxConfinementError 给失败关闭的错误附上原因码；错误文本与原因互不影响。
type sandboxConfinementError struct {
	reason  SandboxConfinementReason
	message string
}

// Error 返回原始错误文本。
func (err *sandboxConfinementError) Error() string { return err.message }

// newSandboxConfinementError 按格式构造带原因码的错误，文本与直接格式化时相同。
func newSandboxConfinementError(reason SandboxConfinementReason, format string, arguments ...any) error {
	return &sandboxConfinementError{reason: reason, message: fmt.Sprintf(format, arguments...)}
}

// sandboxConfinementReasonOf 把规则推导的错误归入原因码；没有错误时返回空串。
func sandboxConfinementReasonOf(err error) SandboxConfinementReason {
	if err == nil {
		return ""
	}
	var classified *sandboxConfinementError
	if errors.As(err, &classified) {
		return classified.reason
	}
	if errors.Is(err, errOSSandboxUnavailable) {
		return SandboxConfinementUnavailable
	}
	return SandboxConfinementPlanFailed
}
