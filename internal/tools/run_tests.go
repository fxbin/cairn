// run_tests.go 提供 run_tests 只读执行器：在工作区内跑测试命令，
// 解析退出码与失败用例，返回结构化结果供模型与 UI 引用。不改代码。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/ports"
)

// runTestsTimeout 限制单次测试命令墙钟时间。
const runTestsTimeout = 10 * time.Minute

// runTestsMaxOutput 限制写入 Result 的原始输出字节。
const runTestsMaxOutput = 256 * 1024

// runTestsMaxFailures 限制结构化失败列表长度。
const runTestsMaxFailures = 50

// RunTests 在工作区执行测试 argv 并结构化汇总结果。
type RunTests struct{ Workspace Workspace }

// SideEffect 声明 run_tests 启动子进程但不改工作区源文件；按 process 分类以便审批。
func (RunTests) SideEffect() SideEffectClass { return SideEffectProcess }

// Spec 返回 run_tests 的受限 JSON 参数定义。
func (tool RunTests) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "run_tests",
		Description: "Run a workspace test command (argv, no shell) and return structured results: exit code, duration, pass/fail counts, and failure samples parsed from go test / vitest / jest style output. Prefer go test ./... or your project's test script. Does not modify files.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["argv"],"properties":{"argv":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},"cwd":{"type":"string"},"timeout_ms":{"type":"integer","minimum":1000,"maximum":600000}}}`),
	}
}

// testFailure 是一条被解析出的失败用例。
type testFailure struct {
	Name    string `json:"name"`
	Message string `json:"message,omitempty"`
}

// runTestsResult 是 run_tests 的稳定数据形状。
type runTestsResult struct {
	Argv       []string      `json:"argv"`
	CWD        string        `json:"cwd,omitempty"`
	ExitCode   int           `json:"exit_code"`
	DurationMS int64         `json:"duration_ms"`
	Passed     int           `json:"passed"`
	Failed     int           `json:"failed"`
	Skipped    int           `json:"skipped"`
	Failures   []testFailure `json:"failures"`
	Framework  string        `json:"framework,omitempty"`
	OutputTail string        `json:"output_tail"`
	Truncated  bool          `json:"truncated"`
}

// runTestsArguments 是 run_tests 的内部解码参数。
type runTestsArguments struct {
	Argv      []string `json:"argv"`
	CWD       string   `json:"cwd"`
	TimeoutMS int      `json:"timeout_ms"`
}

var (
	reGoFail     = regexp.MustCompile(`(?m)^--- FAIL: (\S+)`)
	reGoPass     = regexp.MustCompile(`(?m)^--- PASS: (\S+)`)
	reGoSkip     = regexp.MustCompile(`(?m)^--- SKIP: (\S+)`)
	reGoSummary  = regexp.MustCompile(`(?m)^(ok|FAIL)\s+\S+`)
	reVitestFail = regexp.MustCompile(`(?m)^\s*(?:×|✗|FAIL)\s+(.+?)(?:\s+\(\d+.*)?$`)
	reJestFail   = regexp.MustCompile(`(?m)^\s*●\s+(.+)$`)
)

// Execute 运行测试命令并解析结构化结果。
func (tool RunTests) Execute(ctx context.Context, raw json.RawMessage) Result {
	var args runTestsArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return Failure("invalid_arguments", "run_tests arguments must include argv")
	}
	if len(args.Argv) == 0 {
		return Failure("invalid_arguments", "argv is required")
	}
	for _, part := range args.Argv {
		if part == "" || strings.ContainsRune(part, 0) {
			return Failure("invalid_arguments", "argv entries must be non-empty and free of NUL")
		}
	}
	timeout := runTestsTimeout
	if args.TimeoutMS > 0 {
		timeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	cwd := tool.Workspace.Root
	if strings.TrimSpace(args.CWD) != "" {
		resolved, _, err := tool.Workspace.Resolve(args.CWD)
		if err != nil {
			return Failure("invalid_path", "cwd must be a workspace directory")
		}
		cwd = resolved
	}
	started := time.Now()
	output, exitCode, err := runTestsCommand(ctx, cwd, timeout, args.Argv)
	duration := time.Since(started).Milliseconds()
	if err != nil && output == "" {
		return Failure("test_run_failed", err.Error())
	}
	parsed := parseTestOutput(output)
	parsed.Argv = args.Argv
	if strings.TrimSpace(args.CWD) != "" {
		parsed.CWD = args.CWD
	}
	parsed.ExitCode = exitCode
	parsed.DurationMS = duration
	parsed.OutputTail = tailText(output, 8*1024)
	if len(output) > runTestsMaxOutput {
		parsed.Truncated = true
	}
	preview := fmt.Sprintf("exit=%d failed=%d passed=%d", parsed.ExitCode, parsed.Failed, parsed.Passed)
	if parsed.Failed > 0 {
		preview += " · " + parsed.Failures[0].Name
	}
	return Success(parsed, preview, int64(len(output)), "", parsed.Truncated)
}

// runTestsCommand 以 argv 执行测试并收集合并输出。
func runTestsCommand(ctx context.Context, dir string, timeout time.Duration, argv []string) (string, int, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	command.Dir = dir
	var combined limitedBuffer
	combined.limit = runTestsMaxOutput
	command.Stdout = &combined
	command.Stderr = &combined
	err := command.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return combined.String(), 124, fmt.Errorf("test command timed out after %s", timeout)
		} else {
			return combined.String(), 1, err
		}
	}
	return string(artifact.Redact([]byte(combined.String()))), exitCode, nil
}

// tailText 返回去掉首尾空白后的末尾片段，空输入为空串。
func tailText(content string, limit int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if limit > 0 && len(content) > limit {
		content = content[len(content)-limit:]
		if idx := strings.IndexByte(content, '\n'); idx >= 0 && idx < len(content)-1 {
			content = content[idx+1:]
		}
	}
	return content
}

// parseTestOutput 从常见测试框架输出提取计数与失败样本。
func parseTestOutput(output string) runTestsResult {
	result := runTestsResult{Failures: []testFailure{}}
	// 先识别更具体的前端框架，避免 FAIL 包名被 go 汇总行误判。
	if reVitestFail.MatchString(output) || strings.Contains(output, "vitest") {
		result.Framework = "vitest"
		for _, match := range reVitestFail.FindAllStringSubmatch(output, -1) {
			result.Failed++
			if len(result.Failures) < runTestsMaxFailures {
				result.Failures = append(result.Failures, testFailure{Name: strings.TrimSpace(match[1])})
			}
		}
		return result
	}
	if reJestFail.MatchString(output) {
		result.Framework = "jest"
		for _, match := range reJestFail.FindAllStringSubmatch(output, -1) {
			name := strings.TrimSpace(match[1])
			if name == "" {
				continue
			}
			result.Failed++
			if len(result.Failures) < runTestsMaxFailures {
				result.Failures = append(result.Failures, testFailure{Name: name})
			}
		}
		return result
	}
	if reGoFail.MatchString(output) || reGoPass.MatchString(output) || reGoSkip.MatchString(output) || reGoSummary.MatchString(output) {
		result.Framework = "go"
		for _, match := range reGoFail.FindAllStringSubmatch(output, -1) {
			result.Failed++
			if len(result.Failures) < runTestsMaxFailures {
				result.Failures = append(result.Failures, testFailure{Name: match[1]})
			}
		}
		result.Passed = len(reGoPass.FindAllStringSubmatch(output, -1))
		result.Skipped = len(reGoSkip.FindAllStringSubmatch(output, -1))
		return result
	}
	// 无法识别框架时仍返回退出码，不伪造通过/失败计数。
	if strings.Contains(strings.ToLower(output), "fail") {
		result.Framework = "unknown"
		result.Failed = 1
		result.Failures = append(result.Failures, testFailure{Name: "unparsed", Message: "output contained fail but no framework pattern matched"})
	}
	return result
}
