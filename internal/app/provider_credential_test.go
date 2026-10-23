package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/credential"
	"github.com/fxbin/cairn/internal/modelcatalog"
)

func TestSetupUsesWorkspaceCredentialWithoutExport(t *testing.T) {
	const key = "CAIRN_SETUP_FILE_KEY"
	t.Setenv("HOME", t.TempDir())
	t.Setenv(key, "")
	workspace := t.TempDir()
	if err := credential.UpsertEnvFile(filepath.Join(workspace, credential.KeysFileName), key, "workspace-key-only"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service := SetupService{
		CatalogLoader: func(modelcatalog.Options) (*modelcatalog.Catalog, error) { return nil, errors.New("offline") },
		EndpointProber: func(_ context.Context, _, _, apiKey string) (*ProbeObservations, error) {
			calls++
			if apiKey != "workspace-key-only" {
				t.Error("probe did not receive workspace credential")
			}
			return &ProbeObservations{UsageStreamed: true, StopReason: "stop"}, nil
		},
	}
	result, err := service.Run(context.Background(), SetupInput{Workspace: workspace, BaseURL: "https://example.test/v1", Model: "setup-model", APIKeyEnv: key})
	if err != nil || !result.ProbeRan || calls != 1 {
		t.Fatalf("workspace setup did not probe: %v", err)
	}
	if os.Getenv(key) != "" {
		t.Fatal("setup exported workspace key into global environment")
	}
}

func TestSetupRejectsKeyValueWithoutEcho(t *testing.T) {
	const secret = "sk-local-sensitive-value"
	service := NewSetupService()
	_, err := service.Run(context.Background(), SetupInput{Workspace: t.TempDir(), BaseURL: "https://example.test/v1", Model: "model", APIKeyEnv: secret})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("key value was accepted as reference or echoed in error")
	}
}
