package credential

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUserRootCredentials 验证用户凭据根、显式环境优先和私有权限要求。
func TestUserRootCredentials(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, home)
	}
	const key = "CAIRN_USER_ROOT_TEST_KEY"
	t.Setenv(key, "")
	path, err := UserKeysPath()
	if err != nil || path != filepath.Join(home, ".cairn", "keys.env") {
		t.Fatalf("unexpected keys path: %v", err)
	}
	writeKeysFile(t, path, 0600, key+"=local-test-value\n")
	if _, err := LoadEnvFile(path); err != nil || os.Getenv(key) != "local-test-value" {
		t.Fatalf("user keys did not load: %v", err)
	}
	t.Setenv(key, "explicit")
	if _, err := LoadEnvFile(path); err != nil || os.Getenv(key) != "explicit" {
		t.Fatal("explicit environment priority lost")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnvFile(path); err == nil {
		t.Fatal("unsafe credentials accepted")
	}
}
