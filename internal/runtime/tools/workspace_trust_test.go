// workspace_trust_test 锁定工作区信任标记与写保护。
//
// Author: fxbin
package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWorkspaceTrustLifecycle 锁定信任/取消/写保护。
func TestWorkspaceTrustLifecycle(t *testing.T) {
	root := t.TempDir()
	if IsTrusted(root) {
		t.Fatal("新工作区应不信任")
	}
	if err := GuardTrustedWrite(root, "config"); err == nil {
		t.Fatal("不信任时应拒绝写")
	}
	if err := TrustWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if !IsTrusted(root) {
		t.Fatal("标记后应信任")
	}
	if err := GuardTrustedWrite(root, "config"); err != nil {
		t.Fatalf("信任后应允许写: %v", err)
	}
	if err := UntrustWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if IsTrusted(root) {
		t.Fatal("取消后应不信任")
	}
	if _, err := os.Stat(filepath.Join(root, TrustMarkerFile)); !os.IsNotExist(err) {
		t.Fatal("标记应已移除")
	}
}
