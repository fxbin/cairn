package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageNamesRetainPortableBoundary 从真实解析和启停入口验证内容包
// 私有名称规则，禁止路径、非 ASCII 名称与超长输入进入存储操作。
func TestPackageNamesRetainPortableBoundary(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"a", true}, {"0", true}, {"demo-pack-12", true}, {strings.Repeat("a", PackageMaxNameChars), true},
		{"", false}, {"A", false}, {"a_b", false}, {"a--b", false}, {"-a", false}, {"a-", false},
		{"a/b", false}, {"../a", false}, {"a b", false}, {"a\n", false}, {"技能", false}, {strings.Repeat("a", PackageMaxNameChars+1), false},
	} {
		t.Run(fmt.Sprintf("%q", test.name), func(t *testing.T) {
			raw := []byte(fmt.Sprintf("version: 1\nname: %q\npackage: \"1.0\"\nresources:\n  - path: guide.md\n    sha256: %q\n", test.name, "sha256:"+strings.Repeat("0", 64)))
			manifest, err := ParsePackageManifest(raw)
			if test.valid {
				if err != nil || manifest.Name != test.name {
					t.Fatalf("valid name changed: %#v err=%v", manifest, err)
				}
				return
			}
			if err == nil || err.Error() != "invalid package name" {
				t.Fatalf("invalid name accepted or diagnostic changed: %v", err)
			}
			root := filepath.Join(t.TempDir(), "not-created")
			if err := (PackageStore{Root: root}).SetPackageEnabled(test.name, false); err == nil || err.Error() != "invalid package name" {
				t.Fatalf("invalid name entered store: %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid name touched store: %v", err)
			}
		})
	}
}

// TestContentDigestKeepsStableFormat 锁定导出摘要函数既有的算法、前缀与空文本语义。
func TestContentDigestKeepsStableFormat(t *testing.T) {
	for _, test := range []struct{ text, digest string }{
		{"", "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	} {
		if got := ContentDigest(test.text); got != test.digest {
			t.Fatalf("digest %q=%q want=%q", test.text, got, test.digest)
		}
	}
}
