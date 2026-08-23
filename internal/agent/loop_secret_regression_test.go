package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// TestContainsSensitiveToolCallBlocksCredentialFamily 逐向量锁定 P1 二轮修复的
// argv 结构化规则：对抗性复验登记的同族命令行式凭据（短口令标志、附着短标志、
// URL userinfo、短 bearer 头、查询串凭据键、裸环境赋值、词表外词干）都必须在
// 持久化之前被拒绝。
func TestContainsSensitiveToolCallBlocksCredentialFamily(t *testing.T) {
	vectors := []struct {
		name string
		argv []string
		want bool
	}{
		{name: "docker 分离短口令标志", argv: []string{"docker", "login", "-u", "admin", "-p", "hunter2", "reg.io"}, want: true},
		{name: "mysql 附着短口令标志", argv: []string{"mysql", "-phunter2", "-e", "SELECT 1"}, want: true},
		{name: "ssh-keygen 口令标志", argv: []string{"ssh-keygen", "-P", "hunter2", "-N", "newpass", "id_rsa"}, want: true},
		{name: "redis-cli 口令标志", argv: []string{"redis-cli", "-a", "hunter2"}, want: true},
		{name: "词表外词干 passphrase", argv: []string{"ssh", "--passphrase=hunter2", "host"}, want: true},
		{name: "词表外词干 client-key", argv: []string{"tool", "--client-key=abc123"}, want: true},
		{name: "URL userinfo 凭据", argv: []string{"git", "clone", "https://admin:hunter2@github.com/o/r.git"}, want: true},
		{name: "curl -u 用户口令", argv: []string{"curl", "-u", "admin:hunter2", "https://example.invalid/"}, want: true},
		{name: "curl --user 用户口令", argv: []string{"curl", "--user", "admin:hunter2", "https://example.invalid/"}, want: true},
		{name: "短 bearer 头", argv: []string{"curl", "-H", "Authorization: Bearer ab12cd3", "https://example.invalid/"}, want: true},
		{name: "查询串凭据键", argv: []string{"curl", "https://example.invalid/p?token=short1"}, want: true},
		{name: "裸环境赋值", argv: []string{"docker", "run", "-e", "PASSWORD=hunter2", "img:latest"}, want: true},
		{name: "规范长标志保持拒绝", argv: []string{"curl", "--password=short", "https://example.invalid/"}, want: true},
		{name: "无口令 docker 登录", argv: []string{"docker", "login", "--password-stdin", "reg.io"}, want: false},
		{name: "空口令 ssh-keygen", argv: []string{"ssh-keygen", "-N", `""`, "-f", "id_rsa"}, want: false},
		{name: "纯用户 userinfo", argv: []string{"git", "clone", "https://git@github.com/o/r.git"}, want: false},
		{name: "普通构建命令", argv: []string{"go", "test", "./..."}, want: false},
		{name: "普通查询串", argv: []string{"curl", "https://example.invalid/p?page=2"}, want: false},
		{name: "非凭据环境赋值", argv: []string{"env", "TERM=xterm", "make", "build"}, want: false},
		{name: "非凭据请求头", argv: []string{"curl", "-H", "Accept: application/json", "https://example.invalid/"}, want: false},
	}
	for _, vector := range vectors {
		arguments, err := json.Marshal(map[string]any{"argv": vector.argv})
		if err != nil {
			t.Fatal(err)
		}
		if got := containsSensitiveToolCall(ports.ToolCall{ID: "call", Name: "exec_command", Arguments: arguments}); got != vector.want {
			t.Fatalf("%s: containsSensitiveToolCall=%v, want %v (argv=%q)", vector.name, got, vector.want, vector.argv)
		}
	}
}

// TestContainsSensitiveToolCallBlocksOtherToolChannels 锁定 exec 之外的通道：
// apply_patch content 中的短裸凭据赋值（经 artifact 行锚定脱敏被 containsSecretJSON
// 判定为敏感）、携带凭据形态的工具名/调用 ID，都必须在持久化前被拒绝；
// search_text 的查询词保持可用（行锚定规则不命中标志式查询串）。
func TestContainsSensitiveToolCallBlocksOtherToolChannels(t *testing.T) {
	patchArguments := json.RawMessage(`{"path":".env","mode":"create","content":"DB_PASSWORD=hunter2\nPORT=8080\n"}`)
	if !containsSensitiveToolCall(ports.ToolCall{ID: "call", Name: "apply_patch", Arguments: patchArguments}) {
		t.Fatal("apply_patch content with bare credential assignment must be rejected")
	}
	if !containsSensitiveToolCall(ports.ToolCall{ID: "call_password=hunter2", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)}) {
		t.Fatal("tool call ID carrying a credential assignment must be rejected")
	}
	searchArguments := json.RawMessage(`{"query":"password="}`)
	if containsSensitiveToolCall(ports.ToolCall{ID: "call", Name: "search_text", Arguments: searchArguments}) {
		t.Fatal("credential-syntax search queries must stay usable")
	}
}

// TestLoopRejectsURLUserinfoCredentialBeforePersistence 事件级验证：URL userinfo
// 凭据在收集阶段被拒绝后，任何事件 payload 与后续模型请求都不得含有该值。
func TestLoopRejectsURLUserinfoCredentialBeforePersistence(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{
		{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "leak", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["git","clone","https://admin:hunter2@github.com/o/r.git"]}`)}},
		{Kind: ports.Done},
	}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "clone")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "clone", WorkspaceHash: "workspace"}); !errors.Is(err, errSensitiveToolArguments) {
		t.Fatalf("err=%v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), "hunter2") {
			t.Fatalf("url userinfo credential leaked into event: %#v", event)
		}
	}
	if len(provider.requests) != 1 {
		t.Fatalf("requests=%#v", provider.requests)
	}
}

// TestLoopTreatsStringEncodedToolArgumentsAsMalformed 锁定双重编码失败关闭：
// 字符串化的工具参数无法通过结构化敏感检查，必须按畸形参数拒绝并允许模型
// 纠正重试，且任何事件都不得回显原始字符串。
func TestLoopTreatsStringEncodedToolArgumentsAsMalformed(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	encoded := `"{\"argv\":[\"curl\",\"--password=hunter2\"]}"`
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "weird", Name: "exec_command", Arguments: json.RawMessage(encoded)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "recovered"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "task")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	result, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "task", WorkspaceHash: "workspace"})
	if err != nil || result.Response != "recovered" {
		t.Fatalf("run = %#v, %v", result, err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), "password=hunter2") || strings.Contains(string(event.Payload), encoded) {
			t.Fatalf("string-encoded arguments echoed into event: %#v", event)
		}
	}
	if len(provider.requests) != 2 {
		t.Fatalf("model must get exactly one corrective retry: %#v", provider.requests)
	}
}
