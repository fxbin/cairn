// skill_audit.go 在显式请求时调用外部 skillshare audit；无二进制时返回
// unavailable，不假装通过。不执行被审计技能内容。
// Author: fxbin
package tools

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// skillAuditTimeout 限制外部审计命令。
const skillAuditTimeout = 60 * time.Second

// SkillAuditAvailable 判断 skillshare CLI 是否存在。
func SkillAuditAvailable() bool {
	_, err := exec.LookPath("skillshare")
	return err == nil
}

// RunSkillshareAudit 对指定源目录运行 skillshare audit；返回摘要文本。
func RunSkillshareAudit(ctx context.Context, source string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", errors.New("source directory is required")
	}
	binary, err := exec.LookPath("skillshare")
	if err != nil {
		return "", errors.New("skillshare CLI is not installed")
	}
	runCtx, cancel := context.WithTimeout(ctx, skillAuditTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, binary, "audit", source)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", errors.New("skillshare audit failed: " + detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}
