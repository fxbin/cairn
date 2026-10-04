//go:build darwin || linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// TestLoopExecFailureFeedbackKeepsRedactedDataAndCheckpointTail 验证真实失败进程的消息、Journal 与摘要反馈一致脱敏。
func TestLoopExecFailureFeedbackKeepsRedactedDataAndCheckpointTail(t *testing.T) {
	provider, events := loopExecFeedbackFixture(t, "feedback", 8192)
	for _, event := range events {
		if strings.Contains(string(event.Payload), "exec-fixture-secret-value") || strings.Contains(string(event.Payload), "journal-fixture-password-value") {
			t.Fatalf("凭据 fixture 泄漏到 Journal 事件 %s", event.Type)
		}
		if event.Type == domain.EventOperationFailed {
			var payload struct {
				Result tools.Result `json:"result"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Result.Code != "nonzero_exit" || len(payload.Result.Data) == 0 {
				t.Error("失败操作终态缺少结构化输出")
			}
		}
	}
	found := false
	for _, message := range provider.requests[1].Messages {
		if message.Role != "tool" || message.ToolCallID != "failed-command" {
			continue
		}
		found = true
		if strings.Contains(message.Content, "exec-fixture-secret-value") || strings.Contains(message.Content, "journal-fixture-password-value") {
			t.Error("原始凭据 fixture 泄漏到模型工具消息")
		}
		var result tools.Result
		if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
			t.Fatal(err)
		}
		var data struct {
			ExitCode int    `json:"exit_code"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
		}
		if err := json.Unmarshal(result.Data, &data); err != nil {
			t.Errorf("模型消息没有失败 Data: %v", err)
		}
		if result.OK || result.Code != "nonzero_exit" || data.ExitCode != 7 || !strings.Contains(data.Stdout, "[REDACTED]") || !strings.Contains(data.Stderr, "[REDACTED]") || !strings.Contains(data.Stdout, "ASSERTION_STDOUT_TAIL") || !strings.Contains(data.Stderr, "ERROR_STDERR_TAIL") {
			t.Error("模型失败消息缺少完整脱敏输出或退出证据")
		}
		checkpoint := summarizeToolResult("exec_command", message.Content)
		if !strings.Contains(checkpoint, "ASSERTION_STDOUT_TAIL") || !strings.Contains(checkpoint, "ERROR_STDERR_TAIL") || strings.Contains(checkpoint, "exec-fixture-secret-value") || strings.Contains(checkpoint, "journal-fixture-password-value") {
			t.Error("只保留 preview 的检查点丢失尾部或泄漏凭据 fixture")
		}
	}
	if !found {
		t.Error("后续模型请求中没有失败工具消息")
	}
}

// TestLoopExecFeedbackCapturedQuotedCredential 在真实输出 cap 截到引号值中间时验证三类反馈出口。
func TestLoopExecFeedbackCapturedQuotedCredential(t *testing.T) {
	for _, test := range []struct {
		name      string
		mode      string
		fragments int
		public    bool
	}{
		{name: "double long", mode: "capture-double", fragments: 32},
		{name: "single long", mode: "capture-single", fragments: 32},
		{name: "double one", mode: "capture-double", fragments: 1},
		{name: "double two", mode: "capture-double", fragments: 2},
		{name: "double three", mode: "capture-double", fragments: 3},
		{name: "single one", mode: "capture-single", fragments: 1},
		{name: "single two", mode: "capture-single", fragments: 2},
		{name: "single three", mode: "capture-single", fragments: 3},
		{name: "public negative control", mode: "capture-public", fragments: 32, public: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const prefix = "PUBLIC_PREFIX\n"
			const assignment = "API_KEY=\""
			fragment := strings.Repeat("Z7X_CAPTURE_VALUE_", 100)[:test.fragments]
			provider, events := loopExecFeedbackFixture(t, test.mode, len(prefix)+len(assignment)+test.fragments)
			check := func(source string, result tools.Result) {
				var data struct {
					Stdout       string `json:"stdout"`
					OutputCapped bool   `json:"output_capped"`
				}
				if err := json.Unmarshal(result.Data, &data); err != nil {
					t.Fatal(err)
				}
				if result.Code != "nonzero_exit" || !result.Truncated || !data.OutputCapped {
					t.Fatalf("%s 没有真实截断失败证据", source)
				}
				checkpoint := summarizeToolResult("exec_command", string(mustJSON(result)))
				if test.public {
					if !strings.Contains(data.Stdout, fragment) || strings.Contains(data.Stdout, "[REDACTED]") {
						t.Errorf("%s 非敏感键被误伤", source)
					}
					return
				}
				if data.Stdout != prefix+"API_KEY=[REDACTED]" || strings.Contains(result.Preview, fragment) || strings.Contains(checkpoint, fragment) || !strings.Contains(result.Preview, "[REDACTED]") {
					t.Errorf("%s 的捕获边界泄漏值片段或补造了闭引号", source)
				}
			}
			journalResults := 0
			for _, event := range events {
				var result tools.Result
				switch event.Type {
				case domain.EventOperationFailed:
					var payload struct {
						Result tools.Result `json:"result"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					result = payload.Result
				case domain.EventToolResultAppended:
					if err := json.Unmarshal(event.Payload, &result); err != nil {
						t.Fatal(err)
					}
				default:
					continue
				}
				journalResults++
				check("Journal", result)
			}
			modelResults := 0
			for _, message := range provider.requests[1].Messages {
				if message.Role != "tool" {
					continue
				}
				var result tools.Result
				if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
					t.Fatal(err)
				}
				modelResults++
				check("模型消息", result)
			}
			if journalResults != 2 || modelResults != 1 {
				t.Fatalf("缺少边界出口证据: journal=%d model=%d", journalResults, modelResults)
			}
		})
	}
}

// loopExecFeedbackFixture 通过真实进程和脚本 Provider 运行工具轮次，返回 Journal 与后续模型请求。
func loopExecFeedbackFixture(t *testing.T, mode string, maximum int) (*scriptedProvider, []domain.Event) {
	t.Helper()
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	command := tools.ExecCommand{Workspace: workspace}
	registry, err := tools.NewRegistry(command)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(tools.ExecArguments{
		Argv: []string{executable, "-test.run=^TestLoopExecFeedbackProcess$", "--", mode},
		CWD:  ".", TimeoutMS: 5000, MaxOutputBytes: maximum,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "failed-command", Name: "exec_command", Arguments: arguments}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "received diagnostic feedback"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "command failure feedback")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "command failure feedback", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("后续模型请求数量错误: %d", len(provider.requests))
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return provider, events
}

// TestLoopExecFeedbackProcess 仅在子进程参数指定时生成带凭据 fixture 的长失败输出。
func TestLoopExecFeedbackProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if strings.HasPrefix(mode, "capture-") {
		quote, key := "\"", "API_KEY"
		if mode == "capture-single" {
			quote = "'"
		}
		if mode == "capture-public" {
			key = "MESSAGE"
		}
		fmt.Fprint(os.Stdout, "PUBLIC_PREFIX\n"+key+"="+quote+strings.Repeat("Z7X_CAPTURE_VALUE_", 100)+quote+"\n")
		fmt.Fprint(os.Stderr, "ERROR_STDERR_TAIL\n")
	} else if mode == "feedback" {
		fmt.Fprint(os.Stdout, strings.Repeat("stdout-progress\n", 120)+"API_KEY=\"exec-fixture-secret-value\"\nASSERTION_STDOUT_TAIL\n")
		fmt.Fprint(os.Stderr, strings.Repeat("stderr-progress\n", 120)+"DB_PASSWORD=\"journal-fixture-password-value\"\nERROR_STDERR_TAIL\n")
	} else {
		return
	}
	os.Exit(7)
}
