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
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fxbin/cairn/internal/credential"
	"github.com/fxbin/cairn/internal/ports"
)

// ExecBackend 标识 exec_command 的实际进程执行边界。
type ExecBackend string

const (
	// ExecBackendHost 在宿主机工作区内直接启动经过审批的 argv。
	ExecBackendHost ExecBackend = "host"
	// ExecBackendDocker 在显式指定的 Docker 容器内启动经过审批的 argv。
	ExecBackendDocker ExecBackend = "docker"
)

// ExecCommand 是仅在工作区内按 argv 启动并可整体取消的进程工具。
type ExecCommand struct {
	Workspace     Workspace
	EnvAllow      map[string]struct{}
	Grace         time.Duration
	Backend       ExecBackend
	DockerImage   string
	DockerMaxPIDs int
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

// Execute 在受限工作目录、环境、时限和输出上限内启动 argv 命令；Docker 后端
// 不可用或配置无效时失败关闭，绝不会降级为宿主机执行。
func (tool ExecCommand) Execute(ctx context.Context, raw json.RawMessage) Result {
	arguments, err := ParseExecArguments(raw)
	if err != nil {
		return Failure("invalid", err.Error())
	}
	if err := tool.validateEnvironment(arguments.Env); err != nil {
		return Failure("invalid", err.Error())
	}
	workingDirectory, relativeCWD, err := tool.Workspace.Resolve(arguments.CWD)
	if err != nil {
		return Failure("invalid_path", err.Error())
	}
	info, err := os.Stat(workingDirectory)
	if err != nil || !info.IsDir() {
		return Failure("invalid_path", "cwd must be a workspace directory")
	}
	switch tool.executionBackend() {
	case ExecBackendHost:
		return tool.executeHost(ctx, arguments, workingDirectory)
	case ExecBackendDocker:
		return tool.executeDocker(ctx, arguments, relativeCWD)
	default:
		return Failure("invalid_backend", "exec sandbox backend must be host or docker")
	}
}

// executionBackend 将零值工具保持为兼容的宿主机后端；实际配置必须在装配层严格校验。
func (tool ExecCommand) executionBackend() ExecBackend {
	if tool.Backend == "" {
		return ExecBackendHost
	}
	return tool.Backend
}

// executeHost 以受限环境和独立进程组运行宿主机 argv。
func (tool ExecCommand) executeHost(ctx context.Context, arguments ExecArguments, workingDirectory string) Result {
	command := exec.Command(arguments.Argv[0], arguments.Argv[1:]...)
	command.Dir = workingDirectory
	command.Env = mergedEnvironment(tool.EnvAllow, arguments.Env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return tool.runCommand(ctx, arguments, ExecBackendHost, command, nil)
}

// executeDocker 以固定安全选项运行 Docker；二进制、镜像或 daemon 不可用时该路径
// 只返回失败，不会触发原始 argv 的宿主机执行。
func (tool ExecCommand) executeDocker(ctx context.Context, arguments ExecArguments, relativeCWD string) Result {
	if err := tool.validateDockerConfiguration(); err != nil {
		return Failure("invalid_backend", err.Error())
	}
	if strings.Contains(tool.Workspace.Root, ",") {
		return Failure("invalid_path", "docker sandbox does not support a workspace path containing a comma")
	}
	dockerBinary, err := exec.LookPath("docker")
	if err != nil {
		return Failure("sandbox_unavailable", fmt.Sprintf("docker sandbox is unavailable: %v", err))
	}
	containerName := dockerContainerName()
	command := tool.dockerCommand(dockerBinary, arguments, relativeCWD, containerName)
	return tool.runCommand(ctx, arguments, ExecBackendDocker, command, func() error {
		return tool.removeDockerContainer(dockerBinary, containerName)
	})
}

// validateDockerConfiguration 复核工具直连场景的 Docker 配置，避免绕过 Config 校验。
func (tool ExecCommand) validateDockerConfiguration() error {
	if tool.DockerImage == "" || tool.DockerImage != strings.TrimSpace(tool.DockerImage) || strings.HasPrefix(tool.DockerImage, "-") || strings.ContainsAny(tool.DockerImage, " \t\r\n\x00") {
		return errors.New("docker sandbox requires a valid docker image")
	}
	if tool.dockerPIDLimit() < 1 || tool.dockerPIDLimit() > 1024 {
		return errors.New("docker sandbox max pids must be between 1 and 1024")
	}
	return nil
}

// dockerPIDLimit 为零值内部工具提供保守默认值；配置层仍要求显式的正数。
func (tool ExecCommand) dockerPIDLimit() int {
	if tool.DockerMaxPIDs == 0 {
		return 64
	}
	return tool.DockerMaxPIDs
}

// dockerRunArguments 构造无网络、只读根文件系统且仅挂载规范工作区的 Docker argv。
func (tool ExecCommand) dockerRunArguments(arguments ExecArguments, relativeCWD, containerName string) []string {
	containerCWD := "/workspace"
	if relativeCWD != "." {
		containerCWD = path.Join(containerCWD, filepath.ToSlash(relativeCWD))
	}
	command := []string{"run", "--rm", "--name", containerName}
	// 以宿主用户身份运行容器：可写 bind mount 内产出的文件不会变成
	// root 属主，同时与 cap-drop ALL 一起关闭 setuid 提权面。
	command = append(command, "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()))
	command = append(command, DockerSandboxBaseArgs(tool.dockerPIDLimit(), tool.Workspace.Root, containerCWD)...)
	keys := make([]string, 0, len(arguments.Env))
	for key := range arguments.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		command = append(command, "--env", key+"="+arguments.Env[key])
	}
	command = append(command, tool.DockerImage)
	return append(command, arguments.Argv...)
}

// dockerCommand 创建不继承宿主机环境的 Docker CLI 进程；容器环境仅由
// dockerRunArguments 中明确编码的 arguments.Env 提供。
func (tool ExecCommand) dockerCommand(dockerBinary string, arguments ExecArguments, relativeCWD, containerName string) *exec.Cmd {
	command := exec.Command(dockerBinary, tool.dockerRunArguments(arguments, relativeCWD, containerName)...)
	command.Dir = tool.Workspace.Root
	command.Env = []string{}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return command
}

// dockerContainerName 返回仅由本进程控制的短生命周期容器名称，供超时清理使用。
func dockerContainerName() string {
	return fmt.Sprintf("cairn-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// Docker 清理的超时、重试次数和查询输出均保持固定上限。
const (
	dockerCleanupTimeout    = 3 * time.Second
	dockerCleanupWaitDelay  = time.Second
	dockerCleanupAttempts   = 2
	dockerCleanupQueryBytes = 4096
)

// removeDockerContainer 在 docker run 客户端被取消后删除容器；删除失败时
// 必须经成功的本地查询确认容器已不存在，不能把 daemon 错误当作终止证据。
func (tool ExecCommand) removeDockerContainer(dockerBinary, containerName string) error {
	for attempt := 0; attempt < dockerCleanupAttempts; attempt++ {
		cleanupContext, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		command := exec.CommandContext(cleanupContext, dockerBinary, "rm", "--force", containerName)
		command.Env = []string{}
		command.WaitDelay = dockerCleanupWaitDelay
		err := command.Run()
		cancel()
		if err == nil {
			return nil
		}
	}
	absent, err := tool.dockerContainerAbsent(dockerBinary, containerName)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("docker container remains after cancellation cleanup")
	}
	return nil
}

// dockerContainerAbsent 只在有界 Docker 列表查询成功且输出为空时确认容器
// 不存在；名称过滤可能匹配额外容器，非空、截断或查询失败一律保守拒绝确认。
func (tool ExecCommand) dockerContainerAbsent(dockerBinary, containerName string) (bool, error) {
	queryContext, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	command := exec.CommandContext(queryContext, dockerBinary, "container", "ls", "--all", "--quiet", "--no-trunc", "--filter", "name="+containerName)
	command.Env = []string{}
	command.WaitDelay = dockerCleanupWaitDelay
	output := &limitedBuffer{limit: dockerCleanupQueryBytes}
	command.Stdout = output
	if err := command.Run(); err != nil {
		return false, errors.New("docker container state could not be queried after cancellation cleanup")
	}
	if output.truncated {
		return false, errors.New("docker container state query was truncated after cancellation cleanup")
	}
	return strings.TrimSpace(output.String()) == "", nil
}

// runCommand 启动已完成边界装配的进程，处理超时、进程组终止、可选外部清理及输出摘要。
func (tool ExecCommand) runCommand(ctx context.Context, arguments ExecArguments, backend ExecBackend, command *exec.Cmd, cleanup func() error) Result {
	commandContext, cancel := context.WithTimeout(ctx, time.Duration(arguments.TimeoutMS)*time.Millisecond)
	defer cancel()
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
	terminationUnconfirmed := false
	select {
	case waitErr = <-finished:
		cancelled = commandContext.Err() != nil
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
			var terminationConfirmed bool
			waitErr, terminationConfirmed = waitForExitAfterKill(finished, 5*time.Second)
			terminationUnconfirmed = !terminationConfirmed
		}
	}
	var cleanupErr error
	if cancelled && cleanup != nil {
		cleanupErr = cleanup()
		if terminationUnconfirmed {
			select {
			case waitErr = <-finished:
				terminationUnconfirmed = false
			default:
			}
		}
	}
	if terminationUnconfirmed {
		// Wait 仍可能持有 stdout/stderr 的异步写入协程；此处分支不得读取缓冲区，
		// 同时用专用结果码要求 Agent 保留 operation_started 供人工对账。
		return Failure(CodeTerminationUnconfirmed, "process termination could not be confirmed after SIGKILL")
	}
	if cleanupErr != nil {
		return Failure(CodeTerminationUnconfirmed, "docker container termination could not be confirmed: "+cleanupErr.Error())
	}
	duration := time.Since(started).Milliseconds()
	exitCode, signal := commandOutcome(waitErr)
	data := struct {
		Argv         []string `json:"argv"`
		Backend      string   `json:"backend"`
		CWD          string   `json:"cwd"`
		ExitCode     int      `json:"exit_code"`
		Signal       string   `json:"signal,omitempty"`
		DurationMS   int64    `json:"duration_ms"`
		Stdout       string   `json:"stdout"`
		Stderr       string   `json:"stderr"`
		OutputCapped bool     `json:"output_capped"`
	}{Argv: arguments.Argv, Backend: string(backend), CWD: filepath.ToSlash(arguments.CWD), ExitCode: exitCode, Signal: signal, DurationMS: duration, Stdout: stdout.String(), Stderr: stderr.String(), OutputCapped: stdout.truncated || stderr.truncated}
	if cancelled {
		return Result{Code: "cancelled", Preview: stringResult(data), Truncated: data.OutputCapped, Bytes: int64(stdout.Len() + stderr.Len())}
	}
	if waitErr != nil {
		return Result{Code: "nonzero_exit", Preview: stringResult(data), Truncated: data.OutputCapped, Bytes: int64(stdout.Len() + stderr.Len())}
	}
	return Success(data, stringResult(data), int64(stdout.Len()+stderr.Len()), "", data.OutputCapped)
}

// waitForExitAfterKill 在 SIGKILL 后有界等待 Wait 结果；布尔值仅在已确认 Wait
// 返回时为真，超时不能被解释为进程已经终止。
func waitForExitAfterKill(finished <-chan error, limit time.Duration) (error, bool) {
	select {
	case err := <-finished:
		return err, true
	case <-time.After(limit):
		return errors.New("process did not exit after SIGKILL"), false
	}
}

// exec_command 默认与上限是工具合同的硬边界：策略层与执行器共享同一组语义常量。
const (
	defaultExecTimeoutMS  = 120_000
	maxExecTimeoutMS      = 3_600_000
	defaultMaxOutputBytes = 256 * 1024
	maxAllowedOutputBytes = 1024 * 1024
	resultPreviewMaxBytes = 1024
)

// ParseExecArguments 校验命令参数并填充与执行器相同的默认值；策略层在审批前
// 调用它，确保审批始终绑定到将被实际执行的命令。
func ParseExecArguments(raw json.RawMessage) (ExecArguments, error) {
	arguments := ExecArguments{CWD: ".", TimeoutMS: defaultExecTimeoutMS, MaxOutputBytes: defaultMaxOutputBytes}
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
	if arguments.TimeoutMS < 1 || arguments.TimeoutMS > maxExecTimeoutMS || arguments.MaxOutputBytes < 1 || arguments.MaxOutputBytes > maxAllowedOutputBytes {
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
		if credential.Key(key) {
			return fmt.Errorf("environment variable %q is secret-like and cannot be exposed to a command", key)
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

// stringResult 将结果编码为长度受限的 JSON 预览文本。截断回退到最后一个
// 完整 UTF-8 边界并附加确定性标记，保证预览不会以损坏的多字节字符结尾，
// 且调用方可以识别被截断的预览而不尝试解析不完整的 JSON。
func stringResult(value any) string {
	encoded, _ := json.Marshal(value)
	if len(encoded) <= resultPreviewMaxBytes {
		return string(encoded)
	}
	const marker = "...[preview-truncated]"
	budget := resultPreviewMaxBytes
	for budget > 0 && continuationByte(encoded[budget]) {
		budget--
	}
	return string(encoded[:budget]) + marker
}

// continuationByte 判断字节是否是 UTF-8 多字节序列的续字节。
func continuationByte(b byte) bool { return b&0xC0 == 0x80 }
