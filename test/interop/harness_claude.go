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

// claudeStubKeyValue 是桩模型的占位密钥值。
const claudeStubKeyValue = "interop-stub-key-not-a-credential"

// ClaudeHarness 驱动 Claude Code 作为对照实验的一侧。Claude Code 通过
// settings 的 env 块把 API 基址指向本地桩模型，不需要任何真实账号凭据。
type ClaudeHarness struct {
	// ConfigDir 是隔离的 CLAUDE_CONFIG_DIR；会话与设置都写在这里。
	ConfigDir string
	// SettingsPath 是本外壳使用的设置文件路径。
	SettingsPath string
	// StubBaseURL 是桩模型基地址。
	StubBaseURL string
	// Model 是投递给桩模型的模型名；必须是 Claude Code 能识别的模型别名。
	Model string
	// PermissionMode 是 Claude Code 的权限模式。
	PermissionMode string
	// PermissionPrompts 决定无人应答时权限提示如何处理。
	PermissionPrompts string
	// ExtraArgs 是追加到命令行的额外参数。
	ExtraArgs []string
}

// NewClaudeHarness 构造 Claude Code 外壳并写入隔离设置。
func NewClaudeHarness(configDir string, stubBaseURL string) (*ClaudeHarness, error) {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, fmt.Errorf("create claude config dir: %w", err)
	}
	harness := &ClaudeHarness{
		ConfigDir: configDir, StubBaseURL: stubBaseURL, Model: "sonnet",
		PermissionMode: "acceptEdits", PermissionPrompts: "none",
	}
	settingsPath := filepath.Join(configDir, "interop-settings.json")
	settings := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_BASE_URL": stubBaseURL,
			"ANTHROPIC_API_KEY":  claudeStubKeyValue,
		},
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("encode claude settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, encoded, 0o600); err != nil {
		return nil, fmt.Errorf("write claude settings: %w", err)
	}
	harness.SettingsPath = settingsPath
	return harness, nil
}

// Name 返回外壳标识。
func (harness *ClaudeHarness) Name() string { return "claude" }

// Version 返回 Claude Code 版本号。
func (harness *ClaudeHarness) Version() string {
	outcome, err := runProcess(context.Background(), HarnessRun{
		Command: "claude", Args: []string{"--version"}, Dir: harness.ConfigDir, Timeout: 30 * time.Second,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(outcome.Stdout)
}

// Run 执行一次受控运行。
func (harness *ClaudeHarness) Run(ctx context.Context, run HarnessRun) (HarnessResult, error) {
	args := []string{
		"--bare",
		"--print",
		"--verbose",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--permission-mode", harness.PermissionMode,
		"--permission-prompts", harness.PermissionPrompts,
		"--allowedTools", "Bash",
		"--settings", harness.SettingsPath,
		"--model", harness.Model,
	}
	args = append(args, harness.ExtraArgs...)
	args = append(args, run.Args...)
	args = append(args, run.Task)
	env := append([]string{"CLAUDE_CONFIG_DIR=" + harness.ConfigDir}, run.Env...)
	outcome, err := runProcess(ctx, HarnessRun{
		Command: "claude", Args: args, Dir: run.Workspace, Env: env,
		Timeout: run.Timeout, InterruptAfter: run.InterruptAfter, Stdin: run.Stdin,
	})
	if err != nil {
		return HarnessResult{}, err
	}
	result := HarnessResult{
		ExitCode: outcome.ExitCode, Stdout: outcome.Stdout, Stderr: outcome.Stderr,
		Interrupted: outcome.Interrupted, Duration: outcome.Duration,
		Usage: parseClaudeUsage(outcome.Stdout), Events: parseClaudeEvents(outcome.Stdout),
		Extra: map[string]string{},
	}
	result.SessionHandle = parseClaudeSessionID(outcome.Stdout)
	if denials := parseClaudePermissionDenials(outcome.Stdout); denials > 0 {
		result.Extra["permission_denials"] = fmt.Sprint(denials)
	}
	if reason := parseClaudeTerminalReason(outcome.Stdout); reason != "" {
		result.Extra["terminal_reason"] = reason
	}
	return result, nil
}

// claudeEvent 是 Claude Code stream-json 输出的最小投影。
type claudeEvent struct {
	Type           string `json:"type"`
	SessionID      string `json:"session_id"`
	Subtype        string `json:"subtype"`
	TerminalReason string `json:"terminal_reason"`
	IsError        bool   `json:"is_error"`
	Message        struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	TotalCostUSD      float64 `json:"total_cost_usd"`
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

// decodeClaudeEvents 逐行解析 Claude Code 的 JSON 输出。
func decodeClaudeEvents(stdout string) []claudeEvent {
	var events []claudeEvent
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event claudeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events
}

// parseClaudeEvents 解析 Claude Code 的事件类型序列。
func parseClaudeEvents(stdout string) []string {
	var events []string
	for _, event := range decodeClaudeEvents(stdout) {
		label := event.Type
		if event.Type == "assistant" {
			label = "assistant"
		}
		events = append(events, label)
	}
	return events
}

// parseClaudeSessionID 从输出中取出会话标识，供恢复观测使用。
func parseClaudeSessionID(stdout string) string {
	for _, event := range decodeClaudeEvents(stdout) {
		if event.SessionID != "" {
			return event.SessionID
		}
	}
	return ""
}

// parseClaudeUsage 汇总 Claude Code 报告的 token 用量与费用。
func parseClaudeUsage(stdout string) Usage {
	usage := Usage{}
	for _, event := range decodeClaudeEvents(stdout) {
		if event.Type != "result" {
			continue
		}
		usage.InputTokens += event.Usage.InputTokens
		usage.OutputTokens += event.Usage.OutputTokens
		usage.TotalTokens += event.Usage.InputTokens + event.Usage.OutputTokens +
			event.Usage.CacheCreationInputTokens + event.Usage.CacheReadInputTokens
		usage.CostUSD = event.TotalCostUSD
		usage.Reported = true
	}
	return usage
}

// parseClaudePermissionDenials 统计被拒绝的权限请求数量。
func parseClaudePermissionDenials(stdout string) int {
	count := 0
	for _, event := range decodeClaudeEvents(stdout) {
		count += len(event.PermissionDenials)
	}
	return count
}

// parseClaudeTerminalReason 取出结果行的终态原因。
func parseClaudeTerminalReason(stdout string) string {
	for _, event := range decodeClaudeEvents(stdout) {
		if event.Type == "result" && event.TerminalReason != "" {
			return event.TerminalReason
		}
	}
	return ""
}

// Resume 凭会话标识恢复一次 Claude Code 运行。恢复入口是同一条命令的
// --resume 旗标，会话记录留在隔离的 CLAUDE_CONFIG_DIR 里。
func (harness *ClaudeHarness) Resume(ctx context.Context, run HarnessRun, handle string) (HarnessResult, error) {
	args := []string{
		"--bare",
		"--print",
		"--verbose",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--permission-mode", harness.PermissionMode,
		"--permission-prompts", harness.PermissionPrompts,
		"--allowedTools", "Bash",
		"--settings", harness.SettingsPath,
		"--model", harness.Model,
		"--resume", handle,
	}
	args = append(args, harness.ExtraArgs...)
	args = append(args, run.Args...)
	args = append(args, run.Task)
	env := append([]string{"CLAUDE_CONFIG_DIR=" + harness.ConfigDir}, run.Env...)
	outcome, err := runProcess(ctx, HarnessRun{
		Command: "claude", Args: args, Dir: run.Workspace, Env: env,
		Timeout: run.Timeout, InterruptAfter: run.InterruptAfter,
	})
	if err != nil {
		return HarnessResult{}, err
	}
	result := HarnessResult{
		ExitCode: outcome.ExitCode, Stdout: outcome.Stdout, Stderr: outcome.Stderr,
		Interrupted: outcome.Interrupted, Duration: outcome.Duration,
		Usage: parseClaudeUsage(outcome.Stdout), Events: parseClaudeEvents(outcome.Stdout),
		Extra: map[string]string{},
	}
	result.SessionHandle = parseClaudeSessionID(outcome.Stdout)
	if result.SessionHandle == "" {
		result.SessionHandle = handle
	}
	return result, nil
}
