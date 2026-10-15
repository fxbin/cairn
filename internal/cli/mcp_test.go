package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestMCPListWritesNDJSONWithoutConnecting 列表是纯配置查询，stdout 只能包含结构化状态。
func TestMCPListWritesNDJSONWithoutConnecting(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	workspace := t.TempDir()
	filename := filepath.Join(workspace, "config.yaml")
	content := "version: 2\nprovider:\n  base_url: https://provider.test/v1\n  model: test-model\nmcp:\n  servers:\n    - name: docs\n      transport: streamable-http\n      url: " + server.URL + "\n"
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"mcp", "list", "--workspace", workspace, "--config", filename, "--json"}); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var status struct {
		Name     string `json:"name"`
		Exposure string `json:"exposure"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &status); err != nil || status.Name != "docs" || status.Exposure != "deferred" || requests.Load() != 0 {
		t.Fatalf("status=%#v requests=%d error=%v stdout=%s", status, requests.Load(), err, stdout.String())
	}
}
