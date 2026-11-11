package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunTestsParsesGoFailure 覆盖 go test 失败解析。
func TestRunTestsParsesGoFailure(t *testing.T) {
	output := "=== RUN   TestFoo\n--- FAIL: TestFoo (0.01s)\n    foo_test.go:1: boom\nFAIL\n"
	parsed := parseTestOutput(output)
	if parsed.Framework != "go" || parsed.Failed != 1 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if len(parsed.Failures) != 1 || parsed.Failures[0].Name != "TestFoo" {
		t.Fatalf("failures = %+v", parsed.Failures)
	}
}

// TestRunTestsParsesVitestFailure 覆盖 vitest 风格失败。
func TestRunTestsParsesVitestFailure(t *testing.T) {
	output := "FAIL  src/a.test.ts > does thing\n × expected true (1ms)\n"
	parsed := parseTestOutput(output)
	if parsed.Framework != "vitest" || parsed.Failed < 1 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if !strings.Contains(parsed.Failures[0].Name, "a.test.ts") {
		t.Fatalf("failures = %+v", parsed.Failures)
	}
}

// TestRunTestsExecutesArgv 在临时脚本上跑通退出码与执行。
func TestRunTestsExecutesArgv(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "ok.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho --- PASS: TestOk\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1<<20, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tool := RunTests{Workspace: workspace}
	raw, _ := json.Marshal(map[string]any{"argv": []string{script}})
	result := tool.Execute(context.Background(), raw)
	if !result.OK {
		t.Fatalf("run_tests should succeed: %s", result.Preview)
	}
	var parsed runTestsResult
	if err := json.Unmarshal(result.Data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ExitCode != 0 || parsed.Passed != 1 {
		t.Fatalf("parsed = %+v", parsed)
	}
}

// TestLspDiagnosticsParseTsc 锁定 tsc 行格式解析。
func TestLspDiagnosticsParseTsc(t *testing.T) {
	output := "src/a.ts(12,5): error TS2322: Type 'null' is not assignable.\n"
	items := parseDiagnostics("tsc", output)
	if len(items) != 1 || items[0].Line != 12 || items[0].Severity != "error" {
		t.Fatalf("items = %+v", items)
	}
}

// TestLspDiagnosticsParseGoVet 锁定 go vet 行格式解析。
func TestLspDiagnosticsParseGoVet(t *testing.T) {
	output := "pkg/a.go:3:1: something wrong\n"
	items := parseDiagnostics("go_vet", output)
	if len(items) != 1 || items[0].File != "pkg/a.go" || items[0].Line != 3 {
		t.Fatalf("items = %+v", items)
	}
}

// TestLspDiagnosticsRequiresEngine 没有引擎时失败关闭。
func TestLspDiagnosticsRequiresEngine(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, nil, 1<<20, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tool := LspDiagnostics{Workspace: workspace}
	// 强制不存在的引擎路径场景由 plan 报错覆盖；auto 在空目录无 go.mod 时尝试 tsc。
	result := tool.Execute(context.Background(), json.RawMessage(`{"engine":"gopls"}`))
	// 本机可能有 gopls；若无则应失败关闭。
	if !result.OK && result.Code != "diagnostics_unavailable" && result.Code != "diagnostics_failed" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// TestBrowserToolWithoutDriverFailsClosed 锁定无驱动失败关闭。
func TestBrowserToolWithoutDriverFailsClosed(t *testing.T) {
	tool := Browser{}
	result := tool.Execute(context.Background(), json.RawMessage(`{"action":"open","url":"http://127.0.0.1"}`))
	if result.OK || result.Code != "not_configured" {
		t.Fatalf("result = %+v", result)
	}
}

// TestBrowserFakeDriverEval 覆盖 eval 分发。
func TestBrowserFakeDriverEval(t *testing.T) {
	fake := &fakeBrowserDriver{evalValue: `"ok"`}
	tool := Browser{Driver: fake}
	result := tool.Execute(context.Background(), json.RawMessage(`{"action":"eval","expression":"1+1"}`))
	if !result.OK {
		t.Fatalf("eval failed: %s", result.Preview)
	}
	var parsed browserResult
	if err := json.Unmarshal(result.Data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Value != `"ok"` {
		t.Fatalf("parsed = %+v", parsed)
	}
}

type fakeBrowserDriver struct {
	evalValue string
	navigated string
}

func (fake *fakeBrowserDriver) Navigate(_ context.Context, url string) error {
	fake.navigated = url
	return nil
}

func (fake *fakeBrowserDriver) Evaluate(_ context.Context, _ string) (string, error) {
	return fake.evalValue, nil
}

func (fake *fakeBrowserDriver) Screenshot(_ context.Context) ([]byte, error) {
	return []byte("png"), nil
}

func (fake *fakeBrowserDriver) Close() error { return nil }

// TestBrowserFakeDriverScreenshot 覆盖 screenshot 分发。
func TestBrowserFakeDriverScreenshot(t *testing.T) {
	fake := &fakeBrowserDriver{}
	tool := Browser{Driver: fake}
	result := tool.Execute(context.Background(), json.RawMessage(`{"action":"screenshot"}`))
	if !result.OK {
		t.Fatalf("screenshot failed: %s", result.Preview)
	}
}

// TestTodoBoardLifecycle 覆盖计划增改完成。
func TestTodoBoardLifecycle(t *testing.T) {
	board := NewTodoBoard()
	tool := Todo{Board: board}
	add := tool.Execute(context.Background(), json.RawMessage(`{"action":"add","id":"a","text":"write tests"}`))
	if !add.OK {
		t.Fatal(add.Preview)
	}
	tool.Execute(context.Background(), json.RawMessage(`{"action":"complete","id":"a"}`))
	items := board.Snapshot()
	if len(items) != 1 || items[0].Status != "done" {
		t.Fatalf("items = %+v", items)
	}
}
