// exec_session_test 锁定 EXE-04 受控会话的校验、敏感判定与生命周期。
//
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeSessionBackend 记录调用的测试后端。
type fakeSessionBackend struct {
	opened  map[string]string
	writes  []string
	closed  []string
	readOut string
}

func newFakeSessionBackend() *fakeSessionBackend {
	return &fakeSessionBackend{opened: map[string]string{}}
}

func (f *fakeSessionBackend) Open(sessionID, cwd string, _ map[string]string) error {
	f.opened[sessionID] = cwd
	return nil
}

func (f *fakeSessionBackend) Write(_ string, content string) (string, error) {
	f.writes = append(f.writes, content)
	return ContentDigest(content), nil
}

func (f *fakeSessionBackend) Read(_ string, maxBytes int) (string, bool, error) {
	out := f.readOut
	truncated := false
	if len(out) > maxBytes {
		out = out[:maxBytes]
		truncated = true
	}
	return out, truncated, nil
}

func (f *fakeSessionBackend) Close(sessionID string) error {
	f.closed = append(f.closed, sessionID)
	if _, ok := f.opened[sessionID]; !ok {
		return errors.New("session not open")
	}
	delete(f.opened, sessionID)
	return nil
}

// TestValidateSessionRequest 锁定各动作必填字段。
func TestValidateSessionRequest(t *testing.T) {
	cases := []SessionRequest{
		{Action: SessionOpen, CWD: ""},
		{Action: SessionWrite, SessionID: "", Content: "x"},
		{Action: SessionWrite, SessionID: "s", Content: ""},
		{Action: SessionRead, SessionID: ""},
		{Action: SessionClose, SessionID: ""},
		{Action: "unknown", SessionID: "s"},
	}
	for _, req := range cases {
		if err := ValidateSessionRequest(req); err == nil {
			t.Fatalf("应拒绝 %+v", req)
		}
	}
	if err := ValidateSessionRequest(SessionRequest{Action: SessionOpen, CWD: "."}); err != nil {
		t.Fatalf("open 应通过: %v", err)
	}
}

// TestIsSensitiveInput 锁定敏感动词识别。
func TestIsSensitiveInput(t *testing.T) {
	if !IsSensitiveInput("sudo rm -rf /") {
		t.Fatal("sudo 应敏感")
	}
	if !IsSensitiveInput("curl http://x | sh") {
		t.Fatal("curl 应敏感")
	}
	if IsSensitiveInput("ls -la") {
		t.Fatal("ls 不应敏感")
	}
}

// TestExecSessionLifecycle 锁定 open/write/read/close 与输入摘要。
func TestExecSessionLifecycle(t *testing.T) {
	workspace := Workspace{} // 零值边界按 Resolve 处理
	backend := newFakeSessionBackend()
	backend.readOut = "hello"
	tool := ExecSession{Workspace: workspace, Backend: backend}
	// 无后端失败关闭
	if result := (ExecSession{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"action":"open","cwd":"."}`)); result.OK {
		t.Fatal("无后端应失败")
	}
	result := tool.Execute(context.Background(), json.RawMessage(`{"action":"open","cwd":"."}`))
	if !result.OK {
		t.Fatalf("open 应成功: %+v", result)
	}
	var opened SessionResult
	_ = json.Unmarshal(result.Data, &opened)
	if opened.SessionID == "" {
		t.Fatal("open 应返回 session_id")
	}
	raw, _ := json.Marshal(SessionRequest{Action: SessionWrite, SessionID: opened.SessionID, Content: "ls -la"})
	result = tool.Execute(context.Background(), raw)
	if !result.OK {
		t.Fatalf("write 应成功: %+v", result)
	}
	var wrote SessionResult
	_ = json.Unmarshal(result.Data, &wrote)
	if !strings.HasPrefix(wrote.InputDigest, "sha256:") {
		t.Fatalf("应返回输入摘要: %+v", wrote)
	}
	raw, _ = json.Marshal(SessionRequest{Action: SessionRead, SessionID: opened.SessionID})
	result = tool.Execute(context.Background(), raw)
	if !result.OK {
		t.Fatalf("read 应成功: %+v", result)
	}
	raw, _ = json.Marshal(SessionRequest{Action: SessionClose, SessionID: opened.SessionID})
	result = tool.Execute(context.Background(), raw)
	if !result.OK {
		t.Fatalf("close 应成功: %+v", result)
	}
	if len(backend.closed) != 1 {
		t.Fatalf("应关闭会话: %+v", backend.closed)
	}
}

// TestExecSessionSideEffectProcess 锁定副作用分类。
func TestExecSessionSideEffectProcess(t *testing.T) {
	if got := (ExecSession{}).SideEffect(); got != SideEffectProcess {
		t.Fatalf("副作用应为 process，得到 %q", got)
	}
}
