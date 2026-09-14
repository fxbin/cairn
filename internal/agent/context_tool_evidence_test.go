package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

func TestCheckpointKeepsSearchLocationsAndReadRange(t *testing.T) {
	cases := []struct {
		name, raw string
		required  []string
	}{
		{"search_text", `{"ok":true,"code":"ok","preview":"1 match(es)","data":{"matches":[{"path":"sample.go","line":2,"byte_offset":13,"preview":"target()"}]}}`, []string{"sample.go", "line=2", "byte_offset=13", "target()"}},
		{"read_file", `{"ok":true,"code":"ok","preview":"safe preview","sha256":"sha256:recorded","data":{"path":"sample.go","offset":13,"limit":100,"total_bytes":25,"content":"RAW_DATA_MUST_NOT_BE_COPIED"}}`, []string{"sample.go", "byte_range=[13,25)", "total_bytes=25", "sha256:recorded"}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			summary := summarizeMessage(ports.Message{Role: "tool", ToolCallID: "call", Content: item.raw}, map[string]string{"call": item.name})
			for _, text := range item.required {
				if !strings.Contains(summary, text) {
					t.Fatalf("missing %s in %s", text, summary)
				}
			}
			if strings.Contains(summary, "RAW_DATA_MUST_NOT_BE_COPIED") {
				t.Fatal("copied raw data")
			}
		})
	}
}

func TestCheckpointToolLocationsHandleLegacyAndInvalidEvidence(t *testing.T) {
	for _, item := range []struct {
		name, raw string
		forbidden string
	}{
		{"search_text", `{"ok":true,"data":{"matches":[{"path":"sample.go","line":2,"preview":"target"}]}}`, "byte_offset="},
		{"search_text", `{"ok":false,"data":{"matches":[{"path":"sample.go","line":2,"byte_offset":13}]}}`, "sample.go"},
		{"unknown_tool", `{"ok":true,"data":{"matches":[{"path":"sample.go","line":2,"byte_offset":13}]}}`, "sample.go"},
		{"read_file", `{"ok":true,"data":{"path":"sample.go","limit":10,"total_bytes":20}}`, "byte_range="},
		{"read_file", `{"ok":true,"data":{"path":"sample.go","offset":-1,"limit":10,"total_bytes":20}}`, "byte_range="},
		{"read_file", `{"ok":true,"data":{"path":"sample.go","offset":21,"limit":10,"total_bytes":20}}`, "byte_range="},
	} {
		summary := summarizeMessage(ports.Message{Role: "tool", ToolCallID: "call", Content: item.raw}, map[string]string{"call": item.name})
		if strings.Contains(summary, item.forbidden) {
			t.Fatalf("invented evidence: %s", summary)
		}
	}
}

func TestCheckpointSearchLocationsAreBoundedAndRedacted(t *testing.T) {
	type match struct {
		Path       string `json:"path"`
		Line       int    `json:"line"`
		ByteOffset int    `json:"byte_offset"`
		Preview    string `json:"preview"`
	}
	matches := make([]match, 20)
	for i := range matches {
		matches[i] = match{Path: "sample.go", Line: i + 1, ByteOffset: i * 20, Preview: "api_key=sk-abcdefghijklmnopqrstuvwxyz123456 " + strings.Repeat("padding", 100)}
	}
	result := struct {
		OK   bool `json:"ok"`
		Data struct {
			Matches []match `json:"matches"`
		} `json:"data"`
	}{OK: true}
	result.Data.Matches = matches
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	summary := summarizeMessage(ports.Message{Role: "tool", ToolCallID: "call", Content: string(raw)}, map[string]string{"call": "search_text"})
	if !strings.Contains(summary, "omitted_matches=") {
		t.Fatalf("missing truncation evidence: %s", summary)
	}
	if len(summary) > 1300 {
		t.Fatalf("unbounded evidence: %d", len(summary))
	}
	if strings.Contains(summary, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatal("secret survived truncation")
	}
}

func TestCheckpointReadLocationByteBoundaries(t *testing.T) {
	for _, item := range []struct {
		offset, limit, total int64
		expected             string
	}{
		{0, 10, 3, "byte_range=[0,3)"},
		{3, 10, 3, "byte_range=[3,3)"},
		{3, 0, 3, "byte_range=[3,3)"},
		{9223372036854775806, 9223372036854775807, 9223372036854775807, "byte_range=[9223372036854775806,9223372036854775807)"},
	} {
		raw := json.RawMessage(fmt.Sprintf(`{"path":"sample.txt","offset":%d,"limit":%d,"total_bytes":%d}`, item.offset, item.limit, item.total))
		got := summarizeMessage(ports.Message{Role: "tool", ToolCallID: "read", Content: fmt.Sprintf(`{"ok":true,"data":%s}`, raw)}, map[string]string{"read": "read_file"})
		if !strings.Contains(got, item.expected) {
			t.Fatalf("incorrect range: %s", got)
		}
	}
}

func TestSearchCheckpointLocationCanReopenMatch(t *testing.T) {
	root := t.TempDir()
	target := "needle 中文正文"
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("中文前文\r\n"+target+"\n尾部"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 4096, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result := (tools.SearchText{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"query":"needle"}`))
	if !result.OK {
		t.Fatalf("search: %#v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	summary := summarizeMessage(ports.Message{Role: "tool", ToolCallID: "search", Content: string(raw)}, map[string]string{"search": "search_text"})
	location := regexp.MustCompile(`byte_offset=([0-9]+)`).FindStringSubmatch(summary)
	if len(location) != 2 {
		t.Fatalf("checkpoint lost location: %s", summary)
	}
	offset, err := strconv.ParseInt(location[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(fmt.Sprintf(`{"path":"sample.txt","offset":%d,"limit":%d}`, offset, len(target)))
	read := (tools.ReadFile{Workspace: workspace}).Execute(context.Background(), args)
	var data struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(read.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !read.OK || data.Content != target {
		t.Fatalf("checkpoint location did not reopen target: %#v", read)
	}
}
