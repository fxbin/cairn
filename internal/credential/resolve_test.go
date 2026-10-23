package credential

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveEnvPriorityAndIsolation(t *testing.T) {
	const name = "CAIRN_RESOLVE_TEST_KEY"
	t.Setenv(name, "")
	root := t.TempDir()
	first, second, user := filepath.Join(root, "a", "keys.env"), filepath.Join(root, "b", "keys.env"), filepath.Join(root, "user", "keys.env")
	for path, value := range map[string]string{first: "workspace-a", second: "workspace-b", user: "user-fallback"} {
		if err := UpsertEnvFile(path, name, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct{ path, expected string }{{first, "workspace-a"}, {second, "workspace-b"}, {filepath.Join(root, "missing"), "user-fallback"}, {first, "workspace-a"}} {
		got, err := ResolveEnv(name, check.path, user)
		if err != nil || got != check.expected {
			t.Fatalf("resolution mismatch: %v", err)
		}
		if os.Getenv(name) != "" {
			t.Fatal("file credential leaked into process environment")
		}
	}
	t.Setenv(name, "explicit-env")
	got, err := ResolveEnv(name, first, user)
	if err != nil || got != "explicit-env" {
		t.Fatal("explicit environment did not take priority")
	}
}

func TestResolveEnvRejectsInvalidWholeFile(t *testing.T) {
	const name = "CAIRN_RESOLVE_INVALID_KEY"
	t.Setenv(name, "")
	root := t.TempDir()
	path := filepath.Join(root, "keys.env")
	const secret = "must-not-escape"
	if err := os.WriteFile(path, []byte(name+"="+secret+"\ninvalid line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := ResolveEnv(name, path)
	if value != "" || err == nil || strings.Contains(err.Error(), secret) || os.Getenv(name) != "" {
		t.Fatal("invalid file was partially accepted or leaked material")
	}
	if err := os.WriteFile(path, []byte(name+"="+secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if value, err := ResolveEnv(name, path); err == nil || value != "" {
		t.Fatal("unsafe permissions were accepted")
	}
}
