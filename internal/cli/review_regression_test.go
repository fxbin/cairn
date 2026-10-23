package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewModelCapabilityFiltersBeforeLimit(t *testing.T) {
	catalog := `{"x":{"models":{"a":{"name":"A","tool_call":false,"limit":{"context":1000,"output":100}},"b":{"name":"B","tool_call":true,"limit":{"context":1000,"output":100}}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(catalog)) }))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	args := []string{"provider", "models", "--provider", "x", "--catalog-url", server.URL, "--catalog-cache", filepath.Join(t.TempDir(), "catalog.json"), "--tool-call", "--limit", "1", "--json"}
	if exit := command.Run(context.Background(), args); exit != ExitOK {
		t.Fatalf("exit=%d %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"id":"b"`) {
		t.Fatalf("matching entry beyond unfiltered limit was lost: %s", stdout.String())
	}
	if exit := command.Run(context.Background(), []string{"provider", "models", "--limit", "-1"}); exit != ExitUsage {
		t.Fatalf("invalid limit must fail as usage, got %d", exit)
	}
}

func TestReviewTemplateRenderAndPackageToggleEmitNDJSON(t *testing.T) {
	root := t.TempDir()
	templates, err := tools.NewTemplateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := templates.Save(tools.TemplateDocument{Name: "hello", Parameters: []tools.TemplateParameter{{Name: "name", Required: true}}, Body: "Hello {{name}}"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"template", "render", "hello", "--root", root, "--param", "name=world", "--json"}); exit != ExitOK {
		t.Fatalf("render exit=%d: %s", exit, stderr.String())
	}
	if !json.Valid(bytes.TrimSpace(stdout.Bytes())) || !strings.Contains(stdout.String(), "Hello world") {
		t.Fatalf("render did not emit JSON: %s", stdout.String())
	}
	storeRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(storeRoot, "demo"), 0700); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if exit := command.Run(context.Background(), []string{"package", "disable", "demo", "--root", storeRoot, "--json"}); exit != ExitOK {
		t.Fatalf("toggle exit=%d: %s", exit, stderr.String())
	}
	if !json.Valid(bytes.TrimSpace(stdout.Bytes())) {
		t.Fatalf("toggle did not emit JSON: %s", stdout.String())
	}
}

func TestReviewPresetWritesValidateAndAvoidTemporarySymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	original := []byte("version: 2\nprovider:\n  base_url: https://review.test/v1\n  model: base\n  api_key_env: REVIEW_KEY\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	args := []string{"provider", "presets", "save", "valid", "--base-url", "https://review.test/v1", "--model", "model", "--api-key-env", "REVIEW_KEY", "--protocol", "anthropic-messages", "--config", path}
	if exit := command.Run(context.Background(), args); exit != ExitOK {
		t.Fatalf("valid positional flags rejected: %d %s", exit, stderr.String())
	}
	cfg, err := config.Load(path, config.Overrides{})
	if err != nil || cfg.Provider.Presets["valid"].Protocol != config.ProtocolAnthropicMessages {
		t.Fatalf("wrong saved protocol: %+v %v", cfg.Provider.Presets, err)
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "unchanged" {
		t.Fatalf("preset followed temporary symlink: %q %v", raw, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	args = []string{"provider", "presets", "save", "invalid", "--base-url", "https://user:credential@review.test/v1", "--model", "model", "--api-key-env", "REVIEW_KEY", "--config", path}
	if exit := command.Run(context.Background(), args); exit == ExitOK {
		t.Fatal("invalid preset was persisted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected preset changed the original config")
	}
}

func TestReviewPresetUseEditsSelectedUserConfig(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	path, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := "version: 2\nprovider:\n  base_url: https://review.test/v1\n  model: base\n  api_key_env: REVIEW_KEY\n  presets:\n    alternate:\n      base_url: https://alternate.test/v1\n      model: alternate\n      api_key_env: REVIEW_KEY\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"provider", "use", "alternate", "--workspace", workspace}); exit != ExitOK {
		t.Fatalf("selected user config was not editable: %d %s", exit, stderr.String())
	}
	cfg, err := config.Load(path, config.Overrides{})
	if err != nil || cfg.Provider.Use != "alternate" {
		t.Fatalf("user selection not saved: %q %v", cfg.Provider.Use, err)
	}
	if _, err := os.Stat(config.DefaultPath(workspace)); !os.IsNotExist(err) {
		t.Fatal("unexpected workspace config shadow")
	}
}
