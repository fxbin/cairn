//go:build darwin || linux

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// ExecCommand 是仅在工作区内按 argv 启动并可整体取消的进程工具。
type ExecCommand struct {
	Workspace Workspace
	EnvAllow  map[string]struct{}
	Grace     time.Duration
}

// ExecArguments 是 exec_command 的受限执行参数。
type ExecArguments struct {
	Argv           []string          `json:"argv"`
	CWD            string            `json:"cwd"`
	TimeoutMS      int               `json:"timeout_ms"`
	MaxOutputBytes int               `json:"max_output_bytes"`
	Env            map[string]string `json:"env"`
}

// SideEffect 声明 ExecCommand 会产生进程级副作用。
func (tool ExecCommand) SideEffect() SideEffectClass { return SideEffectProcess }

// Spec 返回 exec_command 的受限 JSON 参数定义。
func (tool ExecCommand) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "exec_command", Description: "Run an approved argv command inside the workspace. Use a workspace-relative cwd; /workspace is accepted as the workspace root alias.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["argv"],"properties":{"argv":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},"cwd":{"type":"string","description":"Workspace-relative directory; /workspace denotes its root."},"timeout_ms":{"type":"integer","minimum":1,"maximum":3600000},"max_output_bytes":{"type":"integer","minimum":1,"maximum":1048576},"env":{"type":"object","additionalProperties":{"type":"string"}}}}`)}
}

// Execute 在受限工作目录、环境、时限和输出上限内启动 argv 命令，并终止整个进程组。
func (tool ExecCommand) Execute(ctx context.Context, raw json.RawMessage) Result {
	arguments, err := ParseExecArguments(raw)
	if err != nil {
		return Failure("invalid", err.Error())
	}
	if err := tool.validateEnvironment(arguments.Env); err != nil {
		return Failure("invalid", err.Error())
	}
	workingDirectory, _, err := tool.Workspace.Resolve(arguments.CWD)
	if err != nil {
		return Failure("invalid_path", err.Error())
	}
	info, err := os.Stat(workingDirectory)
	if err != nil || !info.IsDir() {
		return Failure("invalid_path", "cwd must be a workspace directory")
	}
	commandContext, cancel := context.WithTimeout(ctx, time.Duration(arguments.TimeoutMS)*time.Millisecond)
	defer cancel()
	command := exec.Command(arguments.Argv[0], arguments.Argv[1:]...)
	command.Dir = workingDirectory
	command.Env = mergedEnvironment(tool.EnvAllow, arguments.Env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := &limitedBuffer{limit: arguments.MaxOutputBytes}
	stderr := &limitedBuffer{limit: arguments.MaxOutputBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	started := time.Now()
	if err := command.Start(); err != nil {
		return Failure("start", err.Error())
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	var waitErr error
	cancelled := false
	select {
	case waitErr = <-finished:
	case <-commandContext.Done():
		cancelled = true
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		grace := tool.Grace
		if grace <= 0 {
			grace = 2 * time.Second
		}
		select {
		case waitErr = <-finished:
		case <-time.After(grace):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			waitErr = <-finished
		}
	}
	duration := time.Since(started).Milliseconds()
	exitCode, signal := commandOutcome(waitErr)
	data := struct {
		Argv         []string `json:"argv"`
		CWD          string   `json:"cwd"`
		ExitCode     int      `json:"exit_code"`
		Signal       string   `json:"signal,omitempty"`
		DurationMS   int64    `json:"duration_ms"`
		Stdout       string   `json:"stdout"`
		Stderr       string   `json:"stderr"`
		OutputCapped bool     `json:"output_capped"`
	}{Argv: arguments.Argv, CWD: filepath.ToSlash(arguments.CWD), ExitCode: exitCode, Signal: signal, DurationMS: duration, Stdout: stdout.String(), Stderr: stderr.String(), OutputCapped: stdout.truncated || stderr.truncated}
	if cancelled {
		return Result{Code: "cancelled", Preview: stringResult(data), Truncated: data.OutputCapped, Bytes: int64(stdout.Len() + stderr.Len())}
	}
	if waitErr != nil {
		return Result{Code: "nonzero_exit", Preview: stringResult(data), Truncated: data.OutputCapped, Bytes: int64(stdout.Len() + stderr.Len())}
	}
	return Success(data, stringResult(data), int64(stdout.Len()+stderr.Len()), "", data.OutputCapped)
}

// ParseExecArguments 校验命令参数并填充与执行器相同的默认值；策略层在审批前
// 调用它，确保审批始终绑定到将被实际执行的命令。
func ParseExecArguments(raw json.RawMessage) (ExecArguments, error) {
	arguments := ExecArguments{CWD: ".", TimeoutMS: 120_000, MaxOutputBytes: 256 * 1024}
	if err := decodeArguments(raw, &arguments); err != nil {
		return ExecArguments{}, err
	}
	if len(arguments.Argv) == 0 || arguments.Argv[0] == "" {
		return ExecArguments{}, errors.New("argv requires a non-empty command")
	}
	for _, argument := range arguments.Argv {
		if argument == "" {
			return ExecArguments{}, errors.New("argv cannot contain empty values")
		}
	}
	if arguments.TimeoutMS < 1 || arguments.TimeoutMS > 3_600_000 || arguments.MaxOutputBytes < 1 || arguments.MaxOutputBytes > 1024*1024 {
		return ExecArguments{}, errors.New("timeout_ms or max_output_bytes is outside its allowed range")
	}
	arguments.CWD = normalizeWorkspaceAlias(arguments.CWD)
	return arguments, nil
}

// normalizeWorkspaceAlias 将模型常用的 /workspace 绝对别名收敛为受限的相对工作目录。
func normalizeWorkspaceAlias(cwd string) string {
	if cwd == "/workspace" {
		return "."
	}
	if strings.HasPrefix(cwd, "/workspace/") {
		return strings.TrimPrefix(cwd, "/workspace/")
	}
	return cwd
}

// validateEnvironment 拒绝未被 EnvAllow 明确允许的环境变量覆盖。
func (tool ExecCommand) validateEnvironment(environment map[string]string) error {
	for key := range environment {
		if _, allowed := tool.EnvAllow[key]; !allowed {
			return fmt.Errorf("environment variable %q is not allowed", key)
		}
	}
	return nil
}

// mergedEnvironment 从允许继承的环境变量与显式覆盖构造确定性的进程环境。
func mergedEnvironment(allowed map[string]struct{}, overrides map[string]string) []string {
	values := make(map[string]string)
	for key := range allowed {
		if value, exists := os.LookupEnv(key); exists {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

// commandOutcome 将命令等待错误转换为退出码和可选终止信号。
func commandOutcome(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return -1, ""
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok {
		return exitError.ExitCode(), ""
	}
	if status.Signaled() {
		return -1, status.Signal().String()
	}
	return status.ExitStatus(), ""
}

// limitedBuffer 收集子进程输出，并在到达上限后仍向调用者报告完整写入长度。
type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

// Write 最多保留限制内的内容，并标记被截断的后续输出。
func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if len(content) > remaining {
			_, _ = buffer.buffer.Write(content[:remaining])
			buffer.truncated = true
		} else {
			_, _ = buffer.buffer.Write(content)
		}
	} else {
		buffer.truncated = true
	}
	return len(content), nil
}

// String 返回当前已保留的输出文本。
func (buffer *limitedBuffer) String() string { return buffer.buffer.String() }

// Len 返回当前已保留的输出字节数。
func (buffer *limitedBuffer) Len() int { return buffer.buffer.Len() }

// stringResult 将结果编码为长度受限的 JSON 预览文本。
func stringResult(value any) string {
	encoded, _ := json.Marshal(value)
	if len(encoded) > 1024 {
		return string(encoded[:1024])
	}
	return string(encoded)
}
