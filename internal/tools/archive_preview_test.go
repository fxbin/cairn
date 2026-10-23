// archive_preview_test 锁定 ZIP 条目列举与边界。
//
// Author: fxbin
package tools

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// TestPreviewZipEntries 锁定条目列举与上限。
func TestPreviewZipEntries(t *testing.T) {
	root := t.TempDir()
	zipPath := filepath.Join(root, "a.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, name := range []string{"x.txt", "dir/y.txt"} {
		w, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("hi"))
	}
	_ = writer.Close()
	_ = file.Close()
	result, err := PreviewZipEntries(root, "a.zip")
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if result.Total != 2 || len(result.Entries) != 2 {
		t.Fatalf("条目数不符: %+v", result)
	}
	if result.Entries[0].Name != "x.txt" {
		t.Fatalf("条目名不符: %+v", result.Entries)
	}
	// 越界
	if _, err := PreviewZipEntries(root, "../etc/passwd"); err == nil {
		t.Fatal("越界路径应拒绝")
	}
	// 非 zip
	_ = os.WriteFile(filepath.Join(root, "b.txt"), []byte("x"), 0o644)
	if _, err := PreviewZipEntries(root, "b.txt"); err == nil {
		t.Fatal("非 zip 应拒绝")
	}
}
