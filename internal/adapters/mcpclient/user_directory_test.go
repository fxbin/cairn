package mcpclient

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestUserRootOAuthStorage 验证统一 OAuth 根、私有存储、隔离目录与按身份退出。
func TestUserRootOAuthStorage(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, home)
	}
	directory, err := CredentialDirectory()
	if err != nil || directory != filepath.Join(home, ".cairn", "mcp-auth") {
		t.Fatalf("unexpected OAuth root: %v", err)
	}
	server := oauthServerConfig("https://example.invalid")
	if HasCredentials(directory, server) {
		t.Fatal("missing credentials reported as present")
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status created credential directory")
	}
	if err := writeTokenRecord(directory, server, oauthRecord(server)); err != nil {
		t.Fatal(err)
	}
	if !HasCredentials(directory, server) || HasCredentials(filepath.Join(t.TempDir(), "isolated"), server) {
		t.Fatal("credential directory isolation failed")
	}
	if info, err := os.Stat(credentialPath(directory, server)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file is not private")
	}
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if HasCredentials(directory, server) {
		t.Fatal("unsafe credential directory accepted")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	other := server
	other.Name += "-other"
	if err := writeTokenRecord(directory, other, oauthRecord(other)); err != nil {
		t.Fatal(err)
	}
	if err := Logout(t.Context(), directory, server); err != nil {
		t.Fatal(err)
	}
	if HasCredentials(directory, server) || !HasCredentials(directory, other) {
		t.Fatal("logout identity scope failed")
	}
}
