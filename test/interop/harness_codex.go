//go:build interop

// 本文件属于 interop 标签档：它驱动真实 Codex CLI 进程，依赖外部 CLI
// 安装与进程负载，因此不进入默认门禁。
package interop

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexStubKeyEnv 是桩模型占位密钥的环境变量名；值是固定占位串。
const codexStubKeyEnv = "CODEX_INTEROP_STUB_KEY"

// codexStubKeyValue 是桩模型的占位密钥值。
const codexStubKeyValue = "interop-stub-key-not-a-credential"

// CodexHarness 驱动 Codex CLI 作为对照实验的一侧。Codex 通过自定义
// model_providers 指向本地桩模型，不需要任何真实账号凭据。
type CodexHarness struct {
	// Home 是隔离的 CODEX_HOME；会话与配置都写在这里。
	Home string
	// StubBaseURL 是桩模型基地址。
	StubBaseURL string
	// Model 是投递给桩模型的模型名。
	Model string
	// Sandbox 是 Codex 的沙箱模式；受控运行使用 workspace-write 以便
	// 观察真实的工作区副作用。
	Sandbox string
	// ApprovalPolicy 是 Codex 的审批策略。
	ApprovalPolicy string
}

// NewCodexHarness 构造 Codex 外壳并写入隔离配置。
func NewCodexHarness(home string, stubBaseURL string) (*CodexHarness, error) {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("create codex home: %w", err)
	}
	harness := &CodexHarness{
		Home: home, StubBaseURL: stubBaseURL, Model: "interop-model",
		Sandbox: "workspace-write", ApprovalPolicy: "never",
	}
	config := fmt.Sprintf(`model = %q
model_provider = "interop_stub"
approval_policy = %q
sandbox_mode = %q

[model_providers.interop_stub]
name = "interop-stub"
base_url = %q
wire_api = "responses"
env_key = %q
supports_websockets = false
`, harness.Model, harness.ApprovalPolicy, harness.Sandbox, stubBaseURL+"/v1", codexStubKeyEnv)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o644); err != nil {
		return nil, fmt.Errorf("write codex config: %w", err)
	}
	return harness, nil
}

// Name 返回外壳标识。
func (harness *CodexHarness) Name() string { return "codex" }

// Version 返回 Codex 版本号。
func (harness *CodexHarness) Version() string {
	outcome, err := runProcess(context.Background(), HarnessRun{
		Command: "codex", Args: []string{"--version"}, Dir: harness.Home, Timeout: 30 * time.Second,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(outcome.Stdout)
}

// Run 执行一次受控运行。
func (harness *CodexHarness) Run(ctx context.Context, run HarnessRun) (HarnessResult, error) {
	args := []string{
		"exec", "--json", "--skip-git-repo-check",
		"--cd", run.Workspace,
		"--sandbox", harness.Sandbox,
	}
	args = append(args, run.Args...)
	args = append(args, run.Task)
	env := append([]string{
		"CODEX_HOME=" + harness.Home,
		codexStubKeyEnv + "=" + codexStubKeyValue,
	}, run.Env...)
	outcome, err := runProcess(ctx, HarnessRun{
		Command: "codex", Args: args, Dir: run.Workspace, Env: env,
		Timeout: run.Timeout, InterruptAfter: run.InterruptAfter, Stdin: run.Stdin,
	})
	if err != nil {
		return HarnessResult{}, err
	}
	result := HarnessResult{
		ExitCode: outcome.ExitCode, Stdout: outcome.Stdout, Stderr: outcome.Stderr,
		Interrupted: outcome.Interrupted, Duration: outcome.Duration,
		Usage: parseCodexUsage(outcome.Stdout), Events: parseCodexEvents(outcome.Stdout),
		Extra: map[string]string{},
	}
	result.SessionHandle = parseCodexThreadID(outcome.Stdout)
	return result, nil
}

// codexEvent 是 Codex NDJSON 事件流的最小投影。
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Command string `json:"command"`
		Text    string `json:"text"`
	} `json:"item"`
	Usage *struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
		TotalTokens       int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseCodexEvents 解析 Codex 事件类型序列。
func parseCodexEvents(stdout string) []string {
	var events []string
	for _, event := range decodeCodexEvents(stdout) {
		label := event.Type
		if event.Type == "item.completed" && event.Item.Type != "" {
			label = "item.completed:" + event.Item.Type
		}
		events = append(events, label)
	}
	return events
}

// parseCodexThreadID 从事件流中取出线程标识，供恢复观测使用。
func parseCodexThreadID(stdout string) string {
	for _, event := range decodeCodexEvents(stdout) {
		if event.ThreadID != "" {
			return event.ThreadID
		}
	}
	return ""
}

// parseCodexUsage 汇总 Codex 报告的 token 用量。
func parseCodexUsage(stdout string) Usage {
	usage := Usage{}
	for _, event := range decodeCodexEvents(stdout) {
		if event.Usage == nil {
			continue
		}
		usage.InputTokens += event.Usage.InputTokens
		usage.OutputTokens += event.Usage.OutputTokens
		usage.TotalTokens += event.Usage.TotalTokens
		usage.Reported = true
	}
	return usage
}

// decodeCodexEvents 逐行解析 Codex 的 NDJSON 输出。
func decodeCodexEvents(stdout string) []codexEvent {
	var events []codexEvent
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events
}

// Resume 凭线程标识恢复一次 Codex 运行。Codex 的恢复入口是同一条命令的
// resume 子命令，会话记录留在隔离的 CODEX_HOME 里。
//
// 注意：resume 子命令的旗标集合比 exec 窄——没有 --cd，也没有 --sandbox；
// 工作目录只能由进程工作目录给出，沙箱模式只能来自配置或已存会话。
// 复用同一套参数拼装会在恢复入口直接被参数解析拒绝。
func (harness *CodexHarness) Resume(ctx context.Context, run HarnessRun, handle string) (HarnessResult, error) {
	args := []string{
		"exec", "resume", "--json", "--skip-git-repo-check",
		"--model", harness.Model,
		handle,
	}
	args = append(args, run.Args...)
	args = append(args, run.Task)
	env := append([]string{
		"CODEX_HOME=" + harness.Home,
		codexStubKeyEnv + "=" + codexStubKeyValue,
	}, run.Env...)
	outcome, err := runProcess(ctx, HarnessRun{
		Command: "codex", Args: args, Dir: run.Workspace, Env: env,
		Timeout: run.Timeout, InterruptAfter: run.InterruptAfter,
	})
	if err != nil {
		return HarnessResult{}, err
	}
	result := HarnessResult{
		ExitCode: outcome.ExitCode, Stdout: outcome.Stdout, Stderr: outcome.Stderr,
		Interrupted: outcome.Interrupted, Duration: outcome.Duration,
		Usage: parseCodexUsage(outcome.Stdout), Events: parseCodexEvents(outcome.Stdout),
		Extra: map[string]string{},
	}
	result.SessionHandle = parseCodexThreadID(outcome.Stdout)
	if result.SessionHandle == "" {
		result.SessionHandle = handle
	}
	return result, nil
}
