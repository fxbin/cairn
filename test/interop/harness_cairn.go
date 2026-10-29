package interop

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// cairnExitApproval 是 Cairn CLI 表示"等待人工审批"的退出码。
const cairnExitApproval = 6

// cairnMaxApprovals 是单次受控运行允许的审批往返上限，避免异常配置下无限循环。
const cairnMaxApprovals = 16

// cairnAPIKeyEnv 是桩模型占位密钥的环境变量名；值是固定占位串，不是任何
// 真实账号凭据。
const cairnAPIKeyEnv = "CAIRN_INTEROP_STUB_KEY"

// cairnStubKeyValue 是桩模型的占位密钥值。
const cairnStubKeyValue = "interop-stub-key-not-a-credential"

// approvalFingerprintPattern 从 Cairn 的等待审批错误中提取绑定指纹。
var approvalFingerprintPattern = regexp.MustCompile(`approval required for [^ ]+ \((sha256:[0-9a-f]{64})\)`)

// digestOf 返回内容的 SHA-256 十六进制摘要。
func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// CairnHarness 驱动 Cairn 原生 CLI 作为对照实验的一侧。
type CairnHarness struct {
	// Binary 是 cairn 可执行文件路径。
	Binary string
	// StateRoot 是状态目录根；每次运行在其下开独立子目录。
	StateRoot string
	// StubBaseURL 是桩模型的基地址。
	StubBaseURL string
	// Model 是投递给桩模型的模型名。
	Model string
}

// NewCairnHarness 构造 Cairn 原生外壳。
func NewCairnHarness(binary string, stateRoot string, stubBaseURL string) *CairnHarness {
	return &CairnHarness{Binary: binary, StateRoot: stateRoot, StubBaseURL: stubBaseURL, Model: "interop-model"}
}

// Name 返回外壳标识。
func (harness *CairnHarness) Name() string { return "cairn-native" }

// Version 返回 Cairn 构建版本号。
func (harness *CairnHarness) Version() string {
	outcome, err := runProcess(context.Background(), HarnessRun{
		Command: harness.Binary, Args: []string{"--version"},
		Dir: harness.StateRoot, Timeout: 20 * time.Second,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(outcome.Stdout)
}

// WriteConfig 在工作区写入一次受控运行所需的最小配置：桩模型连接、
// 可选准入门与默认审核策略。
func (harness *CairnHarness) WriteConfig(workspace string) error {
	agentDir := filepath.Join(workspace, ".agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return fmt.Errorf("create agent dir: %w", err)
	}
	body := fmt.Sprintf(`version: 2
provider:
  id: custom
  admission: optional
  protocol: openai-chat-completions
  base_url: %s/v1
  model: %s
  api_key_env: %s
  capability:
    tool_calls: true
    streaming_usage: true
`, harness.StubBaseURL, harness.Model, cairnAPIKeyEnv)
	path := filepath.Join(agentDir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Run 执行一次受控运行。Cairn 的审批是带外流程：运行在需要审批时以退出码
// 6 结束并给出绑定指纹，驱动必须显式发起 resume --approve 才能继续。
// 驱动在此把这段往返折叠进同一次观测，并记录往返次数。
func (harness *CairnHarness) Run(ctx context.Context, run HarnessRun) (HarnessResult, error) {
	stateDir := filepath.Join(harness.StateRoot, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return HarnessResult{}, fmt.Errorf("create state dir: %w", err)
	}
	env := append([]string{cairnAPIKeyEnv + "=" + cairnStubKeyValue}, run.Env...)

	result := HarnessResult{Extra: map[string]string{}}
	approvals := 0
	current := HarnessRun{
		Command: harness.Binary,
		Args: []string{
			"run", "--json",
			"--workspace", run.Workspace,
			"--state-dir", stateDir,
			run.Task,
		},
		Dir: run.Workspace, Env: env,
		Timeout: run.Timeout, InterruptAfter: run.InterruptAfter,
	}
	for {
		outcome, err := runProcess(ctx, current)
		if err != nil {
			return HarnessResult{}, err
		}
		result.ExitCode = outcome.ExitCode
		result.Stdout += outcome.Stdout
		result.Stderr += outcome.Stderr
		result.Interrupted = outcome.Interrupted
		result.Duration += outcome.Duration
		result.Usage = mergeUsage(result.Usage, parseCairnUsage(outcome.Stdout))
		result.Events = append(result.Events, parseCairnEvents(outcome.Stdout)...)

		if outcome.ExitCode != cairnExitApproval {
			break
		}
		approvals++
		result.Extra["approval_round_trips"] = fmt.Sprint(approvals)
		fingerprint, runID, ok := parseApprovalRequest(outcome.Stderr, outcome.Stdout)
		if !ok {
			result.Extra["approval_parse"] = "failed"
			break
		}
		result.SessionHandle = runID
		current = HarnessRun{
			Command: harness.Binary,
			Args: []string{
				"resume", runID,
				"--approve", fingerprint,
				"--json",
				"--workspace", run.Workspace,
				"--state-dir", stateDir,
			},
			Dir:            run.Workspace,
			Env:            env,
			Timeout:        run.Timeout,
			InterruptAfter: run.InterruptAfter,
		}
		if approvals >= cairnMaxApprovals {
			result.Extra["approval_round_trips"] = fmt.Sprintf("%d (上限截断)", approvals)
			break
		}
	}
	if result.SessionHandle == "" {
		result.SessionHandle = parseCairnRunID(result.Stdout)
	}
	if _, ok := result.Extra["approval_round_trips"]; !ok {
		result.Extra["approval_round_trips"] = "0"
	}
	return result, nil
}

// parseApprovalRequest 从 stderr 与 stdout 中取出待审批指纹与运行标识。
func parseApprovalRequest(stderr string, stdout string) (string, string, bool) {
	match := approvalFingerprintPattern.FindStringSubmatch(stderr)
	if match == nil {
		return "", "", false
	}
	return match[1], parseCairnRunID(stdout), true
}

// cairnEventLine 是 Cairn NDJSON 输出里的一条事件包装。
type cairnEventLine struct {
	Type  string `json:"type"`
	RunID string `json:"run_id"`
	Event struct {
		Type    string          `json:"type"`
		RunID   string          `json:"run_id"`
		Payload json.RawMessage `json:"payload"`
	} `json:"event"`
}

// parseCairnEvents 解析 Cairn NDJSON 输出的事件类型序列。
func parseCairnEvents(stdout string) []string {
	var events []string
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var wrapper cairnEventLine
		if err := json.Unmarshal([]byte(line), &wrapper); err != nil {
			continue
		}
		if wrapper.Type == "event" && wrapper.Event.Type != "" {
			events = append(events, wrapper.Event.Type)
			continue
		}
		if wrapper.Type == "result" {
			events = append(events, "result")
		}
	}
	return events
}

// parseCairnUsage 汇总 Cairn 事件流里的 token 用量。
func parseCairnUsage(stdout string) Usage {
	usage := Usage{}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var wrapper cairnEventLine
		if err := json.Unmarshal([]byte(line), &wrapper); err != nil {
			continue
		}
		if wrapper.Event.Type != "model_response_finished" {
			continue
		}
		var payload struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(wrapper.Event.Payload, &payload); err != nil || payload.Usage == nil {
			continue
		}
		usage.InputTokens += payload.Usage.PromptTokens
		usage.OutputTokens += payload.Usage.CompletionTokens
		usage.TotalTokens += payload.Usage.TotalTokens
		usage.Reported = true
	}
	return usage
}

// parseCairnRunID 从 Cairn NDJSON 输出中取出运行标识。运行在等待审批时
// 没有结果行，运行标识只能从事件流中读取。
func parseCairnRunID(stdout string) string {
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var wrapper cairnEventLine
		if err := json.Unmarshal([]byte(line), &wrapper); err != nil {
			continue
		}
		if wrapper.Type == "result" && wrapper.RunID != "" {
			return wrapper.RunID
		}
		if wrapper.Event.RunID != "" {
			return wrapper.Event.RunID
		}
	}
	return ""
}

// mergeUsage 合并两段用量观测。
func mergeUsage(base Usage, extra Usage) Usage {
	if extra.Reported {
		base.Reported = true
		base.InputTokens += extra.InputTokens
		base.OutputTokens += extra.OutputTokens
		base.TotalTokens += extra.TotalTokens
		base.CostUSD += extra.CostUSD
	}
	return base
}

// BuildCairnBinary 从模块根编译 cairn 可执行文件到指定目录。
func BuildCairnBinary(ctx context.Context, moduleRoot string, outputDir string) (string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("create binary dir: %w", err)
	}
	binary := filepath.Join(outputDir, "cairn-interop")
	command := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/cairn")
	command.Dir = moduleRoot
	combined, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build cairn: %w: %s", err, strings.TrimSpace(string(combined)))
	}
	return binary, nil
}

// Resume 凭运行标识恢复一次 Cairn 运行。Cairn 的恢复入口是独立的 resume
// 命令；运行停在待审批态时，恢复必须带上绑定指纹，否则失败关闭。
func (harness *CairnHarness) Resume(ctx context.Context, run HarnessRun, handle string) (HarnessResult, error) {
	stateDir := filepath.Join(harness.StateRoot, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return HarnessResult{}, fmt.Errorf("create state dir: %w", err)
	}
	env := append([]string{cairnAPIKeyEnv + "=" + cairnStubKeyValue}, run.Env...)
	args := []string{"resume", handle, "--json", "--workspace", run.Workspace, "--state-dir", stateDir}
	outcome, err := runProcess(ctx, HarnessRun{
		Command: harness.Binary, Args: args, Dir: run.Workspace, Env: env,
		Timeout: run.Timeout, InterruptAfter: run.InterruptAfter,
	})
	if err != nil {
		return HarnessResult{}, err
	}
	result := HarnessResult{
		ExitCode: outcome.ExitCode, Stdout: outcome.Stdout, Stderr: outcome.Stderr,
		Interrupted: outcome.Interrupted, Duration: outcome.Duration,
		Usage: parseCairnUsage(outcome.Stdout), Events: parseCairnEvents(outcome.Stdout),
		Extra: map[string]string{"approval_round_trips": "0"},
	}
	if outcome.ExitCode == cairnExitApproval {
		fingerprint := approvalFingerprintPattern.FindStringSubmatch(outcome.Stderr)
		if len(fingerprint) < 2 {
			return result, fmt.Errorf("resume paused for approval without a readable fingerprint")
		}
		approved, approveErr := runProcess(ctx, HarnessRun{
			Command: harness.Binary,
			Args: []string{
				"resume", handle, "--approve", fingerprint[1], "--json",
				"--workspace", run.Workspace, "--state-dir", stateDir,
			},
			Dir: run.Workspace, Env: env, Timeout: run.Timeout,
		})
		if approveErr != nil {
			return result, approveErr
		}
		result.Stdout += approved.Stdout
		result.Stderr += approved.Stderr
		result.ExitCode = approved.ExitCode
		result.Usage = mergeUsage(result.Usage, parseCairnUsage(approved.Stdout))
		result.Events = append(result.Events, parseCairnEvents(approved.Stdout)...)
		result.Extra["approval_round_trips"] = "1"
	}
	result.SessionHandle = handle
	return result, nil
}
