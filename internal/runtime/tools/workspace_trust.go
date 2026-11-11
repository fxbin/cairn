// workspace_trust.go 实现工作区信任标记：不信任目录禁止写配置/敏感文件，
// 与 Gemini CLI「untrusted folder」语义一致。标记存于工作区隐藏文件。
// Author: fxbin
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
)

// TrustMarkerFile 是工作区信任标记文件名。
const TrustMarkerFile = ".cairn-trusted"

// UntrustedMarkerFile 显式标记工作区为不信任；无标记视为信任（向后兼容）。
const UntrustedMarkerFile = contract.UntrustedMarkerFile

// WorkspaceTrust 表示工作区信任状态。
type WorkspaceTrust struct {
	Root    string `json:"root"`
	Trusted bool   `json:"trusted"`
}

// IsTrusted 检查工作区是否有信任标记。
func IsTrusted(root string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(root, TrustMarkerFile))
	return err == nil && info.Mode().IsRegular()
}

// TrustWorkspace 写入信任标记；root 必须存在。
func TrustWorkspace(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("workspace root is required")
	}
	marker := filepath.Join(root, TrustMarkerFile)
	return os.WriteFile(marker, []byte("trusted\n"), 0o644)
}

// UntrustWorkspace 移除信任标记。
func UntrustWorkspace(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("workspace root is required")
	}
	err := os.Remove(filepath.Join(root, TrustMarkerFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// GuardTrustedWrite 在不信任工作区拒绝写操作。
func GuardTrustedWrite(root, operation string) error {
	if IsTrusted(root) {
		return nil
	}
	return fmt.Errorf("workspace %q is untrusted; %s requires trust (create %s)", root, operation, TrustMarkerFile)
}

// WriteUntrustedMarker 写入显式拒绝标记。
func WriteUntrustedMarker(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("workspace root is required")
	}
	return os.WriteFile(filepath.Join(root, UntrustedMarkerFile), []byte("untrusted\n"), 0o644)
}

// RemoveUntrustedMarker 移除显式拒绝标记。
func RemoveUntrustedMarker(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("workspace root is required")
	}
	err := os.Remove(filepath.Join(root, UntrustedMarkerFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
