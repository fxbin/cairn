package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/credential"
)

func TestDesktopProviderCredentialIsolationAndAdmission(t *testing.T) {
	reviewHome(t)
	const keyName = "CAIRN_DESKTOP_TEST_KEY"
	t.Setenv(keyName, "")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("Authorization") != "Bearer credential-"+request.Model {
			t.Error("workspace credential crossed request boundary")
			w.WriteHeader(401)
			return
		}
		if len(request.Tools) != 1 || request.Tools[0].Function.Name != "contract_probe" {
			t.Error("desktop did not request the two-leg probe")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		isSecond := len(request.Messages) > 1
		if !isSecond && request.Model != "no-tool" {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"probe\",\"type\":\"function\",\"function\":{\"name\":\"contract_probe\",\"arguments\":\"{\\\"value\\\":7}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	api := NewAPI(app.Service{}, nil, nil)
	ctx := context.Background()
	stateDir := t.TempDir()
	workspaces := []string{t.TempDir(), t.TempDir()}
	var first ProviderProbeView
	for index, workspace := range workspaces {
		model := fmt.Sprintf("workspace-%d", index)
		if err := credential.UpsertEnvFile(filepath.Join(workspace, credential.KeysFileName), keyName, "credential-"+model); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("version: 2\nprovider:\n  id: custom\n  admission: required\n  base_url: %s\n  model: %s\n  api_key_env: %s\n  capability:\n    tool_calls: true\n    streaming_usage: true\n", server.URL, model, keyName)
		if err := os.WriteFile(config.DefaultPath(workspace), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		view, err := api.ProbeProvider(ctx, WorkspaceRequest{Workspace: workspace, StateDir: stateDir})
		if err != nil || view.Result != "pass" || view.ToolRoundtrip != "ok" || !view.UsageStreamed || view.EvidenceFingerprint == "" || view.ConfigFingerprint == "" || view.RequestProfileFingerprint == "" {
			t.Fatalf("probe failed: %+v %v", view, err)
		}
		report, err := api.Doctor(ctx, WorkspaceRequest{Workspace: workspace, StateDir: stateDir})
		if err != nil || !report.Provider.APIKeyPresent || report.Provider.Protocol != config.ProtocolOpenAIChatCompletions || report.Admission.Admitted {
			t.Fatalf("doctor credential/admission mismatch: %v", err)
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "credential-"+model) || os.Getenv(keyName) != "" {
			t.Fatal("diagnostics or process environment leaked a file credential")
		}
		if index == 0 {
			first = view
		}
	}
	request := ProviderAdmissionRequest{Workspace: workspaces[0], StateDir: stateDir, EvidenceFingerprint: first.EvidenceFingerprint, ConfigFingerprint: first.ConfigFingerprint, RequestProfileFingerprint: first.RequestProfileFingerprint, Reason: "用户复核两轮工具探测"}
	request.Workspace = workspaces[1]
	if _, err := api.ConfirmProviderAdmission(ctx, request); err == nil {
		t.Fatal("cross-workspace evidence accepted")
	}
	request.Workspace = workspaces[0]
	path := config.DefaultPath(request.Workspace)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(original), "model: workspace-0", "model: changed", 1)
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ConfirmProviderAdmission(ctx, request); err == nil {
		t.Fatal("stale configuration accepted")
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ConfirmProviderAdmission(ctx, request); err != nil {
		t.Fatal(err)
	}
	report, err := api.Doctor(ctx, WorkspaceRequest{Workspace: request.Workspace, StateDir: stateDir})
	if err != nil || !report.Admission.Admitted {
		t.Fatalf("confirmed decision did not reach run gate: %v", err)
	}
	if _, err := api.ConfirmProviderAdmission(ctx, request); err == nil {
		t.Fatal("duplicate confirmation created a competing chain head")
	}
	if calls.Load() != 4 {
		t.Fatalf("confirmation or doctor unexpectedly called provider: %d", calls.Load())
	}
	// 仅文本成功不能被桌面升级为工具能力准入。
	noToolWorkspace := t.TempDir()
	if err := credential.UpsertEnvFile(filepath.Join(noToolWorkspace, credential.KeysFileName), keyName, "credential-no-tool"); err != nil {
		t.Fatal(err)
	}
	noToolBody := strings.Replace(string(original), "model: workspace-0", "model: no-tool", 1)
	if err := os.WriteFile(config.DefaultPath(noToolWorkspace), []byte(noToolBody), 0600); err != nil {
		t.Fatal(err)
	}
	noTool, err := api.ProbeProvider(ctx, WorkspaceRequest{Workspace: noToolWorkspace, StateDir: stateDir})
	if err != nil || noTool.Result != "inconclusive" {
		t.Fatalf("text-only probe: %+v %v", noTool, err)
	}
	request = ProviderAdmissionRequest{Workspace: noToolWorkspace, StateDir: stateDir, EvidenceFingerprint: noTool.EvidenceFingerprint, ConfigFingerprint: noTool.ConfigFingerprint, RequestProfileFingerprint: noTool.RequestProfileFingerprint, Reason: "不应接受"}
	if _, err := api.ConfirmProviderAdmission(ctx, request); err == nil {
		t.Fatal("text-only evidence was admitted")
	}
	report, err = api.Doctor(ctx, WorkspaceRequest{Workspace: noToolWorkspace, StateDir: stateDir})
	if err != nil || report.Admission.Admitted {
		t.Fatal("rejected confirmation changed admission state")
	}

}

func TestDesktopProviderCredentialPathEscapeRejected(t *testing.T) {
	reviewHome(t)
	const key = "CAIRN_DESKTOP_ESCAPE_KEY"
	t.Setenv(key, "")
	workspace, external := t.TempDir(), t.TempDir()
	reviewConfig(t, workspace)
	if err := credential.UpsertEnvFile(filepath.Join(external, "keys.env"), key, "not-for-this-workspace"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "keys.env"), filepath.Join(workspace, credential.KeysFileName)); err != nil {
		t.Fatal(err)
	}
	path := config.DefaultPath(workspace)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(body), "REVIEW_KEY", key, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(app.Service{}, nil, nil)
	if _, err := api.Doctor(context.Background(), WorkspaceRequest{Workspace: workspace, StateDir: t.TempDir()}); err == nil {
		t.Fatal("escaped credential path was read")
	}
}
