// open_location.go 实现「打开位置」多目标探测与启动：Finder/终端/IDE/终端模拟器。
// 路径解析与 OpenInEditor 同边界；目标缺失则列表不含该项，不伪造。
// Author: fxbin
package desktop

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// OpenLocationTarget 是可打开的宿主目标。
type OpenLocationTarget struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
}

// listOpenLocationTargets 按平台探测常见宿主；顺序固定，便于 UI 稳定。
func listOpenLocationTargets() []OpenLocationTarget {
	if runtime.GOOS == "windows" {
		return []OpenLocationTarget{
			{ID: "explorer", Label: "资源管理器", Available: true},
			{ID: "terminal", Label: "终端", Available: true},
			{ID: "vscode", Label: "VS Code", Available: commandExists("code")},
			{ID: "cursor", Label: "Cursor", Available: commandExists("cursor")},
			{ID: "idea", Label: "IntelliJ IDEA", Available: windowsAppExists("idea64.exe") || commandExists("idea")},
		}
	}
	// Unix：macOS / Linux
	targets := []OpenLocationTarget{
		{ID: "finder", Label: "Finder", Available: runtime.GOOS == "darwin"},
		{ID: "terminal", Label: "Terminal", Available: true},
		{ID: "vscode", Label: "VS Code", Available: commandExists("code") || runtime.GOOS == "darwin"},
		{ID: "cursor", Label: "Cursor", Available: commandExists("cursor")},
		{ID: "idea", Label: "IntelliJ IDEA", Available: commandExists("idea") || macAppExists("IntelliJ IDEA.app")},
		{ID: "ghostty", Label: "Ghostty", Available: macAppExists("Ghostty.app") || commandExists("ghostty")},
	}
	return targets
}

// OpenInLocation 将工作区或相对路径交给指定目标；target 未知失败关闭。
func OpenInLocation(workspace, relative, target string) error {
	_, resolved, _, err := resolveReadableRelative(workspace, relative)
	if err != nil {
		return err
	}
	path := resolved
	if !fileExists(path) && !dirExists(path) {
		return errors.New("path does not exist")
	}
	// Finder 在 macOS 上定位文件；其它目标打开文件或目录。
	switch strings.TrimSpace(target) {
	case "finder":
		if runtime.GOOS != "darwin" {
			return errors.New("finder is only available on macOS")
		}
		return exec.Command("open", "-R", path).Start()
	case "explorer":
		if runtime.GOOS != "windows" {
			return errors.New("explorer is only available on Windows")
		}
		return exec.Command("explorer", "/select,", path).Start()
	case "terminal":
		return openInTerminal(path)
	case "vscode":
		return openWithEditorCLI("code", path)
	case "cursor":
		return openWithEditorCLI("cursor", path)
	case "idea":
		return openWithEditorCLI("idea", path)
	case "ghostty":
		return openInGhostty(path)
	default:
		return errors.New("unknown open location target")
	}
}

func openInTerminal(path string) error {
	switch runtime.GOOS {
	case "darwin":
		// Terminal.app：打开路径所在目录
		dir := path
		if fileExists(path) {
			dir = filepath.Dir(path)
		}
		return exec.Command("open", "-a", "Terminal", dir).Start()
	case "windows":
		return exec.Command("cmd", "/c", "start", "cmd", "/k", "cd", "/d", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

func openWithEditorCLI(binary, path string) error {
	if commandExists(binary) {
		return exec.Command(binary, path).Start()
	}
	if runtime.GOOS == "darwin" {
		app := map[string]string{"code": "Visual Studio Code.app", "cursor": "Cursor.app", "idea": "IntelliJ IDEA.app"}[binary]
		if app != "" && macAppExists(app) {
			return exec.Command("open", "-a", app, path).Start()
		}
	}
	return errors.New(binary + " is not installed")
}

func openInGhostty(path string) error {
	dir := path
	if fileExists(path) {
		dir = filepath.Dir(path)
	}
	if macAppExists("Ghostty.app") {
		return exec.Command("open", "-a", "Ghostty", "--args", "--working-directory="+dir).Start()
	}
	if commandExists("ghostty") {
		return exec.Command("ghostty", "--working-directory="+dir).Start()
	}
	return errors.New("ghostty is not installed")
}

func macAppExists(appName string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	info, err := exec.LookPath("/Applications/" + appName)
	if err == nil {
		return fileExists(info) || dirExists(info)
	}
	return dirExists("/Applications/" + appName)
}

func windowsAppExists(name string) bool {
	return commandExists(name)
}
