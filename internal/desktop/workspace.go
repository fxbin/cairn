package desktop

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/app"
)

// 本文件提供工作区与仓库状态的只读观察，以及 UI 偏好（最近项目/会话元数据）
// 的本地存储。偏好不得替代 Journal 事实，也不得删除运行历史。

// preferencesFile 是用户目录下的 GUI 偏好文件名。
const preferencesFile = "gui-preferences.json"

// WorkspaceObserve 观察工作区目录、配置存在性与 Git 状态。不创建目录、不写入。
func WorkspaceObserve(path string) (WorkspaceStatus, error) {
	status := WorkspaceStatus{Path: path, StateDir: app.DefaultStateDir()}
	if strings.TrimSpace(path) == "" {
		return status, errors.New("workspace path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return status, err
	}
	status.Path = abs
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return status, nil
	}
	status.Exists = true
	status.IsWorkspace = true
	status.HasAgentDir = dirExists(filepath.Join(abs, ".agent"))
	status.HasConfig = fileExists(filepath.Join(abs, ".agent", "config.yaml"))
	status.ConfigPath = filepath.Join(abs, ".agent", "config.yaml")
	status.GitRoot, status.Branch, status.Dirty = gitStatus(abs)
	return status, nil
}

// OpenInEditor 尝试使用系统打开路径；仅接受已存在的工作区内文件/目录。
// 需要调用方先通过路径边界校验；此处再做存在性检查，不绕过 Policy。
func OpenInEditor(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("path is required")
	}
	if !fileExists(path) && !dirExists(path) {
		return errors.New("path does not exist")
	}
	// macOS open / Linux xdg-open / Windows start
	var cmd *exec.Cmd
	switch {
	case commandExists("open"):
		cmd = exec.Command("open", path)
	case commandExists("xdg-open"):
		cmd = exec.Command("xdg-open", path)
	default:
		return errors.New("no system opener available")
	}
	return cmd.Start()
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func gitStatus(workspace string) (root, branch string, dirty bool) {
	// 只读 git 命令，失败时不猜测归属与状态。
	rootBytes, err := runGit(workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", false
	}
	root = strings.TrimSpace(string(rootBytes))
	branchBytes, err := runGit(workspace, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		branch = strings.TrimSpace(string(branchBytes))
	}
	statusBytes, err := runGit(workspace, "status", "--porcelain")
	if err == nil {
		dirty = len(strings.TrimSpace(string(statusBytes))) > 0
	}
	return root, branch, dirty
}

func runGit(workspace string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = workspace
	return cmd.Output()
}
