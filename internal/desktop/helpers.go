package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/userdir"
)

// 本文件放置桥接层内部辅助：路径解析、配置装载与小型校验，避免把宿主细节
// 泄漏到 DTO 或前端。

func errInvalid(message string) error {
	return errors.New(message)
}

func userConfigRoot() (string, error) {
	return userdir.Root()
}

// resolveStateDir 使用显式状态目录或平台默认值。
func resolveStateDir(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	return app.DefaultStateDir()
}

// loadConfiguration 与 CLI 相同顺序装载工作区配置（默认路径）。
func loadConfiguration(workspace, stateDir string) (config.Config, error) {
	_ = stateDir
	if strings.TrimSpace(workspace) == "" {
		current, err := os.Getwd()
		if err != nil {
			return config.Config{}, err
		}
		workspace = current
	}
	path := config.ResolveDefaultPath(workspace)
	return config.Load(path, config.Overrides{})
}

// portsApproval 将前端审批请求映射为一次性 ports.Approval。
func portsApproval(request ResumeRequest) ports.Approval {
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		if request.Approve {
			reason = "approved via desktop UI"
		} else {
			reason = "denied via desktop UI"
		}
	}
	return ports.Approval{
		Fingerprint: request.Fingerprint,
		Approve:     request.Approve,
		Reason:      reason,
	}
}

// domainOperationID 校验并转换操作标识。
func domainOperationID(raw string) domain.OperationID {
	return domain.OperationID(strings.TrimSpace(raw))
}

// previewResponse 截断长回复用于状态预览。
func previewResponse(response string) string {
	const max = 240
	runes := []rune(strings.TrimSpace(response))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "…"
}

// resultState 从合同结果或错误推断展示状态。
func resultState(outcome app.RunResult, err error) string {
	if outcome.Result != nil && outcome.Result.State != "" {
		return outcome.Result.State
	}
	if err != nil {
		return "failed"
	}
	return "completed"
}

// absWorkspace 保证工作区为绝对路径。
func absWorkspace(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errInvalid("workspace is required")
	}
	return filepath.Abs(path)
}
