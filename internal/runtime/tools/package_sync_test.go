package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageInstallPreservesTreeEntryBudget 使用清单允许的资源数量构造过多
// 嵌套目录，发布前仍须按目录和文件的共同预算失败关闭，且清理全部临时包。
func TestPackageInstallPreservesTreeEntryBudget(t *testing.T) {
	files := make(map[string]string, PackageMaxEntries)
	for index := 0; index < PackageMaxEntries; index++ {
		files[fmt.Sprintf("group-%03d/a/b/c/guide.md", index)] = "resource"
	}
	source := writePackageSource(t, "nested-pack", "1.0", files)
	store, err := NewPackageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InstallPackage(source); err == nil || err.Error() != "sync package failed" {
		t.Fatalf("oversized directory tree published: %v", err)
	}
	entries, err := os.ReadDir(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".pkg-install-") {
			t.Fatalf("failed publication retained package directory %q", entry.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(store.Root, "nested-pack")); !os.IsNotExist(err) {
		t.Fatalf("failed package became visible: %v", err)
	}
}

// TestPackageSyncTreeRejectsLinks 目录同步继续拒绝根内外链接，不能因迁移仅
// 按清单同步而遗漏临时包中的链接或其他未审查目录项。
func TestPackageSyncTreeRejectsLinks(t *testing.T) {
	directory, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := syncPackageTree(root); err == nil || err.Error() != "skill package links and special files are not supported" {
		t.Fatalf("linked tree not rejected: %v", err)
	}
}

// TestPackageSyncDirectoryUsesRootHandle 已关闭的根句柄必须失败，不能通过
// 根名称回退到绝对路径重新打开目录并伪造同步成功。
func TestPackageSyncDirectoryUsesRootHandle(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syncPackageDirectory(root, "."); err == nil || err.Error() != "open skill directory for sync failed" {
		t.Fatalf("closed root was reopened: %v", err)
	}
}
