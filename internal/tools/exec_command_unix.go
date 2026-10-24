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
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/artifact"
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
	// ExecBackendSeatbeltHost 是 host+macOS Seatbelt 的展示名。
	ExecBackendSeatbeltHost ExecBackend = "host+seatbelt"
)

// ExecCommand 是仅在工作区内按 argv 启动并可整体取消的进程工具。
type ExecCommand struct {
	Workspace     Workspace
	EnvAllow      map[string]struct{}
	Grace         time.Duration
	Backend       ExecBackend
	DockerImage   string
	DockerMaxPIDs int
	// HostConfined 为 true 时 host 后端套 OS 原生沙箱（macOS Seatbelt）。
	// 与 Codex/DSH 同构：默认约束网络与工作区外写入。
	HostConfined bool
	// HostWrite 跟随 Policy.Write：false 时 seatbelt 仅允许临时目录写入。
	HostWrite bool
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
// 执行边界由 ExecRunner 策略（host / seatbelt / docker）决定。
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
	runner, err := runnerFor(tool.executionBackend(), tool.Workspace, tool.HostConfined, tool.HostWrite, tool.DockerImage, tool.dockerPIDLimit())
	if err != nil {
		return Failure("invalid_backend", err.Error())
	}
	return runner.Run(ctx, ExecRunRequest{
		Arguments:        arguments,
		WorkingDirectory: workingDirectory,
		RelativeCWD:      relativeCWD,
		Workspace:        tool.Workspace,
		EnvAllow:         tool.EnvAllow,
		HostWrite:        tool.HostWrite,
		Grace:            tool.Grace,
	})
}

// executionBackend 将零值工具保持为兼容的宿主机后端；实际配置必须在装配层严格校验。
func (tool ExecCommand) executionBackend() ExecBackend {
	if tool.Backend == "" {
		return ExecBackendHost
	}
	return tool.Backend
}

// validateDockerConfiguration 复核工具直连场景的 Docker 配置，避免绕过 Config 校验。
func (tool ExecCommand) validateDockerConfiguration() error {
	return validateDockerImageAndPIDs(tool.DockerImage, tool.dockerPIDLimit())
}

// validateDockerImageAndPIDs 校验镜像名与 PID 配额（Runner 共用）。
func validateDockerImageAndPIDs(image string, maxPIDs int) error {
	if image == "" || image != strings.TrimSpace(image) || strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\r\n\x00") {
		return errors.New("docker sandbox requires a valid docker image")
	}
	if maxPIDs < 1 || maxPIDs > 1024 {
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
	return dockerRunArgumentsFor(tool.Workspace, tool.DockerImage, tool.dockerPIDLimit(), arguments, relativeCWD, containerName)
}

// dockerRunArgumentsFor 是策略层共用的 docker argv 构造。
func dockerRunArgumentsFor(workspace Workspace, image string, maxPIDs int, arguments ExecArguments, relativeCWD, containerName string) []string {
	containerCWD := "/workspace"
	if relativeCWD != "." {
		containerCWD = path.Join(containerCWD, filepath.ToSlash(relativeCWD))
	}
	command := []string{"run", "--rm", "--name", containerName, "--label", "cairn.exec.id=" + containerName}
	// 以宿主用户身份运行容器：可写 bind mount 内产出的文件不会变成
	// root 属主，同时与 cap-drop ALL 一起关闭 setuid 提权面。
	command = append(command, "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()))
	command = append(command, DockerSandboxBaseArgs(maxPIDs, workspace.Root, containerCWD)...)
	keys := make([]string, 0, len(arguments.Env))
	for key := range arguments.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		command = append(command, "--env", key+"="+arguments.Env[key])
	}
	command = append(command, image)
	return append(command, arguments.Argv...)
}

// dockerCommand 创建不继承宿主机环境的 Docker CLI 进程；容器环境仅由
// dockerRunArguments 中明确编码的 arguments.Env 提供。
func (tool ExecCommand) dockerCommand(dockerBinary string, arguments ExecArguments, relativeCWD, containerName string) *exec.Cmd {
	return dockerRunCommand(dockerBinary, tool.Workspace, tool.DockerImage, tool.dockerPIDLimit(), arguments, relativeCWD, containerName)
}

// dockerRunCommand 是策略层共用的 docker 进程构造。
func dockerRunCommand(dockerBinary string, workspace Workspace, image string, maxPIDs int, arguments ExecArguments, relativeCWD, containerName string) *exec.Cmd {
	command := exec.Command(dockerBinary, dockerRunArgumentsFor(workspace, image, maxPIDs, arguments, relativeCWD, containerName)...)
	command.Dir = workspace.Root
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
	return removeDockerContainerByID(dockerBinary, containerName)
}

// removeDockerContainerByID 是策略层共用的容器回收。
func removeDockerContainerByID(dockerBinary, containerName string) error {
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
	absent, err := dockerContainerAbsent(dockerBinary, containerName)
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
func dockerContainerAbsent(dockerBinary, containerName string) (bool, error) {
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

// execCommandOutput 保存已确认终止命令的退出事实和每路输出上限内的文本；
// Result.Bytes 独立记录原始捕获字节数，不因展示时去掉残缺 UTF-8 尾部而减少。
type execCommandOutput struct {
	Argv         []string `json:"argv"`
	Backend      string   `json:"backend"`
	CWD          string   `json:"cwd"`
	ExitCode     int      `json:"exit_code"`
	Signal       string   `json:"signal,omitempty"`
	DurationMS   int64    `json:"duration_ms"`
	Stdout       string   `json:"stdout"`
	Stderr       string   `json:"stderr"`
	OutputCapped bool     `json:"output_capped"`
}

// runCommand 启动已完成边界装配的进程，处理超时、进程组终止、可选外部清理及输出摘要。
// runCommand 启动已完成边界装配的进程；包装 runCommandShared。
func (tool ExecCommand) runCommand(ctx context.Context, arguments ExecArguments, backend ExecBackend, command *exec.Cmd, cleanup func() error) Result {
	return runCommandShared(ctx, arguments, backend, command, cleanup, tool.Grace)
}

// runCommandShared 启动已完成边界装配的进程，处理超时、进程组终止、可选外部清理及输出摘要。
func runCommandShared(ctx context.Context, arguments ExecArguments, backend ExecBackend, command *exec.Cmd, cleanup func() error, grace time.Duration) Result {
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
		waitGrace := grace
		if waitGrace <= 0 {
			waitGrace = 2 * time.Second
		}
		select {
		case waitErr = <-finished:
		case <-time.After(waitGrace):
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
	data := execCommandOutput{Argv: arguments.Argv, Backend: string(backend), CWD: filepath.ToSlash(arguments.CWD), ExitCode: exitCode, Signal: signal, DurationMS: duration, Stdout: stdout.String(), Stderr: stderr.String(), OutputCapped: stdout.truncated || stderr.truncated}
	result := Success(data, stringResult(data), int64(stdout.Len()+stderr.Len()), "", data.OutputCapped)
	if cancelled || waitErr != nil {
		result.OK = false
		result.Code = "nonzero_exit"
		if cancelled {
			result.Code = "cancelled"
		}
		result.Preview = execFailurePreview(data)
	}
	return result
}

// execFailurePreview 先完整脱敏捕获文本，再把两路诊断尾部置于预览前部；
// 尾部预算使退出事实和输出截断标记也能保留在上下文摘要的短预览中。
func execFailurePreview(data execCommandOutput) string {
	stderr, stderrClipped := execDiagnosticTail(string(artifact.Redact([]byte(data.Stderr))))
	stdout, stdoutClipped := execDiagnosticTail(string(artifact.Redact([]byte(data.Stdout))))
	return stringResult(struct {
		StderrTail    string `json:"stderr_tail,omitempty"`
		StdoutTail    string `json:"stdout_tail,omitempty"`
		ExitCode      int    `json:"exit_code"`
		Signal        string `json:"signal,omitempty"`
		OutputCapped  bool   `json:"output_capped"`
		TailTruncated bool   `json:"tail_truncated"`
		Backend       string `json:"backend"`
	}{StderrTail: stderr, StdoutTail: stdout, ExitCode: data.ExitCode, Signal: data.Signal, OutputCapped: data.OutputCapped, TailTruncated: stderrClipped || stdoutClipped, Backend: data.Backend})
}

// execDiagnosticTail 仅接收已脱敏文本，按 JSON 编码字节预算保留 UTF-8 尾部；
// 先限原文字节范围，避免长输出触发逐字符全量编码，返回是否省略了前部。
func execDiagnosticTail(content string) (string, bool) {
	start := max(0, len(content)-execFailureTailEncodedBytes)
	for start < len(content) && continuationByte(content[start]) {
		start++
	}
	for {
		tail := content[start:]
		encoded, _ := json.Marshal(tail)
		if len(encoded) <= execFailureTailEncodedBytes {
			return tail, start != 0
		}
		_, size := utf8.DecodeRuneInString(tail)
		start += size
	}
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
	// 每路 JSON 尾部占总预览预算的八分之一，兼顾两路诊断与退出事实。
	execFailureTailEncodedBytes = resultPreviewMaxBytes / 8
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

// String 返回已捕获文本，仅在输出被截断时去掉末尾未完成的 UTF-8 字符；
// 正常内容及原始捕获计数不变，避免 JSON 替换残片后膨胀越过输出上限。
func (buffer *limitedBuffer) String() string {
	content := buffer.buffer.Bytes()
	if buffer.truncated && len(content) != 0 {
		start := len(content) - 1
		for start > 0 && continuationByte(content[start]) {
			start--
		}
		if !utf8.FullRune(content[start:]) {
			content = content[:start]
		}
	}
	return string(content)
}

// Len 返回原始捕获字节数，展示时裁去残缺 UTF-8 尾部不改变计数。
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
