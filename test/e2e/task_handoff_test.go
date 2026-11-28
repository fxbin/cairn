//go:build e2e

// [INPUT]: 真实 CLI、记录型受控 SSE、Go 验证命令和显式开启任务交接的隔离配置。
// [OUTPUT]: 验证返工 Worker 的模型请求带上任务交接摘要，且修复、审查与验收照常完成。
// [POS]: 按 e2e 标签运行。模型响应来自本地夹具。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件验证任务交接摘要经真实 CLI 进程进入返工 Worker 的模型输入。
package e2e_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/cli"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// handoffObservation 汇总脚本化 Provider 看到的请求：返工请求有几条、其中带交接摘要
// 的有几条，以及非返工请求有没有误带。
type handoffObservation struct {
	mu                    sync.Mutex
	reworkRequests        int
	reworkWithHandoff     int
	reworkWithSessionCard int
	otherRequests         int
	otherWithHandoff      int
}

// handoffObservingProvider 与 autonomousProvider 的回应相同，另外记录交接摘要出现在
// 哪些请求里。
func handoffObservingProvider(t *testing.T, observed *handoffObservation) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var call struct {
			Messages []ports.Message `json:"messages"`
		}
		if json.NewDecoder(request.Body).Decode(&call) != nil || len(call.Messages) == 0 {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		isReview, isRework, hasHandoff, hasSessionCard := false, false, false, false
		for _, message := range call.Messages {
			if message.Role != "user" {
				continue
			}
			isReview = isReview || strings.Contains(message.Content, "冻结验证上下文")
			isRework = isRework || strings.Contains(message.Content, "前次固定程序检查")
			hasHandoff = hasHandoff || strings.Contains(message.Content, contract.TaskHandoffHeading)
			hasSessionCard = hasSessionCard || strings.Contains(message.Content, "最终回复:")
		}
		observed.mu.Lock()
		if isRework {
			observed.reworkRequests++
			if hasHandoff {
				observed.reworkWithHandoff++
			}
			if hasSessionCard {
				observed.reworkWithSessionCard++
			}
		} else {
			observed.otherRequests++
			if hasHandoff {
				observed.otherWithHandoff++
			}
		}
		observed.mu.Unlock()
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

// TestAutonomousReworkWorkerReceivesTaskHandoff 锁定显式开启交接后的真实链路：返工
// Worker 的每一次模型请求都带上交接摘要，不再带上一运行的最终回复；其余请求都不带；
// 修复、独立审查与显式验收照常完成。
func TestAutonomousReworkWorkerReceivesTaskHandoff(t *testing.T) {
	workspace, state, cache := t.TempDir(), t.TempDir(), t.TempDir()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	observed := &handoffObservation{}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configuration := baseSignalConfig(handoffObservingProvider(t, observed)) + fmt.Sprintf(
		"\npolicy:\n  write: ask\n  exec: allow\n  env_allowlist: [PATH, GOCACHE, GOENV, GOPROXY, GOSUMDB, GOTOOLCHAIN, CGO_ENABLED]\nverification:\n  commands:\n    - id: test\n      argv: [%q, test, ./...]\n      env:\n        GOCACHE: %q\n        GOENV: 'off'\n        GOPROXY: 'off'\n        GOSUMDB: 'off'\n        GOTOOLCHAIN: local\n        CGO_ENABLED: '0'\ncontext:\n  task_handoff:\n    mode: rework\n", goBinary, cache)
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
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
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
	if second.Workflow.WorkflowID != first.Workflow.WorkflowID || second.Workflow.LastRetry == nil || len(second.Workflow.Steps[0].Attempts) != 2 {
		t.Fatal("the task did not retain the failed check and start a rework attempt")
	}
	// 返工 Worker 已经发过模型请求：此时就能断言交接摘要进了它的输入，不必等到流程走完。
	observed.mu.Lock()
	reworkRequests, reworkWithHandoff, reworkWithSessionCard := observed.reworkRequests, observed.reworkWithHandoff, observed.reworkWithSessionCard
	observed.mu.Unlock()
	if reworkRequests == 0 || reworkWithHandoff != reworkRequests {
		t.Fatalf("rework requests = %d, with handoff = %d", reworkRequests, reworkWithHandoff)
	}
	if reworkWithSessionCard != 0 {
		t.Fatalf("%d rework requests still carried the previous run's final response", reworkWithSessionCard)
	}
	fixture.approve(t, second, 0)
	reviewed := fixture.get(t, created.ID)
	report := reviewed.Workflow.Verification
	if reviewed.State != "active" || report == nil || report.Evaluation.Verdict != contract.VerdictPassed || report.ReviewRunID == "" {
		t.Fatalf("task did not reach explicit acceptance after the rework: %+v", report)
	}
	fixture.run(t, 0, "task", "complete", string(created.ID), "--revision", strconv.FormatUint(reviewed.Revision, 10),
		"--run", string(report.ReviewRunID))
	completed := fixture.get(t, created.ID)
	if completed.State != "completed" || completed.Completion == nil {
		t.Fatal("explicit acceptance did not complete the task")
	}
	observed.mu.Lock()
	defer observed.mu.Unlock()
	if observed.otherWithHandoff != 0 || observed.otherRequests == 0 {
		t.Fatalf("handoff leaked into %d of %d non-rework requests", observed.otherWithHandoff, observed.otherRequests)
	}
	if observed.reworkWithHandoff != observed.reworkRequests {
		t.Fatalf("rework requests = %d, with handoff = %d", observed.reworkRequests, observed.reworkWithHandoff)
	}
}
