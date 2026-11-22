//go:build e2e

// [INPUT]: 真实 CLI、受控 SSE、Go 验证命令和隔离持久状态。
// [OUTPUT]: 验证自主任务跨进程审批、返工、报告与显式验收。
// [POS]: 按 e2e 标签运行。模型响应来自本地夹具。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件验证自主任务经过真实 CLI 进程、工具、程序检查和独立审查。
package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/cli"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

const (
	brokenDouble = "package example\n\nfunc Double(n int) int { return n + 1 }\n"
	fixedDouble  = "package example\n\nfunc Double(n int) int { return n * 2 }\n"
)

// autonomousProvider 通过真实 OpenAI 适配器发出固定 Worker 与 Review 决策。
func autonomousProvider(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var call struct {
			Messages []ports.Message `json:"messages"`
			Tools    []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if json.NewDecoder(request.Body).Decode(&call) != nil || len(call.Messages) == 0 {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		isReview, isRework := false, false
		for _, message := range call.Messages {
			if message.Role == "user" {
				isReview = isReview || strings.Contains(message.Content, "冻结验证上下文")
				isRework = isRework || strings.Contains(message.Content, "前次固定程序检查")
			}
		}
		if isReview {
			if len(call.Tools) != 3 {
				t.Errorf("independent review expected exactly three file tools, got %d", len(call.Tools))
			}
			for _, tool := range call.Tools {
				switch tool.Function.Name {
				case "read_file", "list_files", "search_text":
				default:
					t.Errorf("independent review received %q", tool.Function.Name)
				}
			}
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		reason := "stop"
		if call.Messages[len(call.Messages)-1].Role != "tool" {
			name, arguments := "apply_patch", ""
			switch {
			case isReview:
				name, arguments = "read_file", `{"path":"double.go"}`
			case isRework:
				arguments = fmt.Sprintf(`{"path":"double.go","mode":"update","expected_sha256":"sha256:%x","replacements":[{"old":%q,"new":%q}]}`,
					sha256.Sum256([]byte(brokenDouble)), brokenDouble, fixedDouble)
			default:
				arguments = fmt.Sprintf(`{"path":"double.go","mode":"create","expected_sha256":"absent","content":%q}`, brokenDouble)
			}
			reason = "tool_calls"
			fmt.Fprintf(writer, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"fixture-tool\",\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n", name, arguments)
		} else {
			content := "代码已写入"
			if isReview {
				content = `{"findings":[]}`
			}
			fmt.Fprintf(writer, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", encode(content))
		}
		fmt.Fprintf(writer, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n", reason)
		fmt.Fprint(writer, "data: [DONE]\n\n")
		writer.(http.Flusher).Flush()
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// taskCLI 每次调用都使用新进程。工作区和状态路径由测试提供。
type taskCLI struct {
	binary, workspace, state, config string
}

func (fixture taskCLI) run(t *testing.T, expected int, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	args = append(args, "--workspace", fixture.workspace, "--state-dir", fixture.state, "--config", fixture.config, "--json")
	command := exec.CommandContext(ctx, fixture.binary, args...)
	command.Env = append(os.Environ(), "CAIRN_E2E_SIGNAL_KEY=e2e-task-fixture")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	var exit *exec.ExitError
	if expected == 0 && err != nil || expected != 0 && (!errors.As(err, &exit) || exit.ExitCode() != expected) {
		t.Fatalf("%v expected exit %d, got %v\n%s", args[:2], expected, err, stderr.String())
	}
	return output
}

func (fixture taskCLI) get(t *testing.T, id app.TaskID) app.TaskView {
	t.Helper()
	var view app.TaskView
	if err := json.Unmarshal(fixture.run(t, 0, "task", "get", string(id)), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func (fixture taskCLI) approve(t *testing.T, view app.TaskView, expected int) {
	t.Helper()
	if view.Workflow == nil || view.Workflow.PendingApproval == nil {
		t.Fatal("missing persisted approval")
	}
	instance := view.Workflow
	pending := instance.PendingApproval
	fixture.run(t, expected, "workflow", "approve", instance.WorkflowID, instance.Steps[instance.ResumeIndex].ID,
		"--seq", strconv.FormatUint(instance.Sequence, 10), "--reason", "approve this tool invocation",
		"--run", string(pending.RunID), "--operation", string(pending.OperationID), "--fingerprint", pending.Fingerprint)
}

func TestAutonomousCodingTaskAcrossCLIProcesses(t *testing.T) {
	workspace, state, cache := t.TempDir(), t.TempDir(), t.TempDir()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configuration := baseSignalConfig(autonomousProvider(t)) + fmt.Sprintf(
		"\npolicy:\n  write: ask\n  exec: allow\n  env_allowlist: [PATH, GOCACHE, GOENV, GOPROXY, GOSUMDB, GOTOOLCHAIN, CGO_ENABLED]\nverification:\n  commands:\n    - id: test\n      argv: [%q, test, ./...]\n      env:\n        GOCACHE: %q\n        GOENV: 'off'\n        GOPROXY: 'off'\n        GOSUMDB: 'off'\n        GOTOOLCHAIN: local\n        CGO_ENABLED: '0'\n", goBinary, cache)
	for name, body := range map[string]string{
		filepath.Join(workspace, "go.mod"): "module example.test/double\n\ngo 1.24\n",
		filepath.Join(workspace, "double_test.go"): `package example
import "testing"
func TestDouble(t *testing.T) {
	if Double(3) != 6 { t.Fatal("wrong result") }
}
`,
		configPath: configuration,
	} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := taskCLI{buildCairnBinary(t), workspace, state, configPath}
	var created app.TaskView
	output := fixture.run(t, cli.ExitApproval, "task", "create", "实现 Double 返回整数的两倍", "--accept", "通过 Go 单元测试",
		"--check", "test", "--criterion-id", "double-behavior", "--autonomous", "--rework-attempts", "1")
	if err := json.Unmarshal(output, &created); err != nil || created.ID == "" {
		t.Fatalf("creation did not report a durable Task: %v", err)
	}
	first := fixture.get(t, created.ID)
	fixture.approve(t, first, cli.ExitApproval)
	second := fixture.get(t, created.ID)
	if second.Workflow.WorkflowID != first.Workflow.WorkflowID || second.Workflow.LastRetry == nil ||
		len(second.Workflow.Steps[0].Attempts) != 2 || second.Workflow.Steps[1].Attempts[0].Output.Ref == "" {
		t.Fatal("the same task did not retain the failed check and a new attempt")
	}
	drift := filepath.Join(workspace, "changed-after-check.go")
	if err := os.WriteFile(drift, []byte("package example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.approve(t, second, cli.ExitFailed)
	unchanged := fixture.get(t, created.ID)
	if unchanged.Workflow.Sequence != second.Workflow.Sequence || unchanged.Workflow.PendingApproval == nil {
		t.Fatal("the drifted approval changed the workflow")
	}
	if err := os.Remove(drift); err != nil {
		t.Fatal(err)
	}
	fixture.approve(t, unchanged, 0)
	reviewed := fixture.get(t, created.ID)
	report := reviewed.Workflow.Verification
	if reviewed.State != "active" || report == nil || report.Evaluation.Verdict != contract.VerdictPassed ||
		report.ProgramRunID == "" || report.ReviewRunID == "" || report.WorkerRunID == "" {
		t.Fatalf("task did not reach explicit acceptance: %+v", report)
	}
	fixture.run(t, 0, "task", "complete", string(created.ID), "--revision", strconv.FormatUint(reviewed.Revision, 10),
		"--run", string(report.ReviewRunID))
	completed := fixture.get(t, created.ID)
	if completed.State != "completed" || completed.Completion == nil || len(completed.Progress.Remaining) != 0 ||
		completed.Completion.RunID != report.ReviewRunID || completed.Workflow.Sequence != reviewed.Workflow.Sequence {
		t.Fatal("explicit acceptance did not replay the same passed evidence")
	}
	content, err := os.ReadFile(filepath.Join(workspace, "double.go"))
	if err != nil || string(content) != fixedDouble {
		t.Fatalf("repair did not produce the tested Go source: %v", err)
	}
}
