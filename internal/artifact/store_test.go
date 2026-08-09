package artifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRedactsBeforePersistingAndUsesStrictPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := New(root, 64)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put([]byte("token=top-secret-value\nBearer abcdefghijklmnop\nkeep"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ref.TruncatedPreview, "top-secret") || !strings.Contains(ref.TruncatedPreview, "[REDACTED]") {
		t.Fatalf("preview leaked secret: %#v", ref)
	}
	digest := strings.TrimPrefix(ref.SHA256, "sha256:")
	content, err := os.ReadFile(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "top-secret") || !strings.Contains(string(content), "[REDACTED]") {
		t.Fatalf("artifact leaked secret: %q", content)
	}
	info, err := os.Stat(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact permissions = %v, %v", info.Mode().Perm(), err)
	}
}

func TestStoreContentAddressingDeduplicates(t *testing.T) {
	store, err := New(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Put([]byte("same"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put([]byte("same"), "text/plain")
	if err != nil || first.SHA256 != second.SHA256 {
		t.Fatalf("refs = %#v %#v, %v", first, second, err)
	}
}

func TestStoreRejectsArtifactsAboveConfiguredLimit(t *testing.T) {
	store, err := New(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	store.MaxBytes = 3
	if _, err := store.Put([]byte("four"), "text/plain"); err == nil {
		t.Fatal("oversized artifact was persisted")
	}
}

func TestRedactJSONReplacesCredentialValuesWithoutBreakingJSON(t *testing.T) {
	redacted := RedactJSON([]byte(`{"token":"very-secret","nested":{"api_key":"also-secret"},"message":"Bearer abcdefghijklmnop"}`))
	if strings.Contains(string(redacted), "secret") || !strings.Contains(string(redacted), "[REDACTED]") {
		t.Fatalf("redaction failed: %s", redacted)
	}
	var decoded map[string]any
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("redaction broke JSON: %v", err)
	}
}

func TestRedactJSONPreservesTokenUsageAndBudgetCounters(t *testing.T) {
	redacted := RedactJSON([]byte(`{"token":"very-secret","session_token":"also-secret","usage":{"prompt_tokens":123,"completion_tokens":45,"total_tokens":168},"budget":{"max_input_tokens":250000,"max_output_tokens":50000}}`))
	if strings.Contains(string(redacted), "very-secret") || strings.Contains(string(redacted), "also-secret") {
		t.Fatalf("redaction leaked credential: %s", redacted)
	}
	var decoded struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Budget struct {
			MaxInputTokens  int `json:"max_input_tokens"`
			MaxOutputTokens int `json:"max_output_tokens"`
		} `json:"budget"`
	}
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Usage.PromptTokens != 123 || decoded.Usage.CompletionTokens != 45 || decoded.Usage.TotalTokens != 168 || decoded.Budget.MaxInputTokens != 250000 || decoded.Budget.MaxOutputTokens != 50000 {
		t.Fatalf("token facts were redacted: %#v", decoded)
	}
}
