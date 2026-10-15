//go:build darwin || linux

// Package tools 为 MCP stdio 复用工作区、环境、进程组与 Docker 隔离边界。
// Author: fxbin
package tools

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MCP 进程清理保持默认宽限和最终退出确认的有限等待。
const (
	mcpDefaultTerminationGrace   = 2 * time.Second
	mcpProcessExitConfirmTimeout = 5 * time.Second
)

// MCPProcess 暴露单次授权操作的双向流；关闭必须确认进程组和容器已经停止。
type MCPProcess struct {
	Reader     io.ReadCloser
	Writer     io.WriteCloser
	command    *exec.Cmd
	done       chan struct{}
	grace      time.Duration
	cleanup    func() error
	once       sync.Once
	closeError error
}

// StartMCP 在完整授权链后启动显式 argv；额外环境必须已由服务器配置解析为引用。
// Docker 保留既有安全参数，凭据只通过进程环境传入，不进入 Docker argv。
func (tool ExecCommand) StartMCP(argv []string, cwd string, environment map[string]string) (*MCPProcess, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("MCP stdio command is required")
	}
	workingDirectory, relativeCWD, err := tool.Workspace.Resolve(cwd)
	if err != nil {
		return nil, errors.New("MCP cwd is outside the workspace")
	}
	info, err := os.Stat(workingDirectory)
	if err != nil || !info.IsDir() {
		return nil, errors.New("MCP cwd is not a workspace directory")
	}
	var command *exec.Cmd
	var cleanup func() error
	switch tool.executionBackend() {
	case ExecBackendHost:
		command = exec.Command(argv[0], argv[1:]...)
		command.Dir = workingDirectory
		command.Env = mergedEnvironment(tool.EnvAllow, environment)
	case ExecBackendDocker:
		if strings.Contains(tool.Workspace.Root, ",") {
			return nil, errors.New("MCP Docker workspace cannot contain a comma")
		}
		if err := tool.validateDockerConfiguration(); err != nil {
			return nil, err
		}
		dockerBinary, err := exec.LookPath("docker")
		if err != nil {
			return nil, errors.New("MCP Docker sandbox is unavailable")
		}
		containerName := dockerContainerName()
		arguments := tool.dockerRunArguments(ExecArguments{Argv: argv}, relativeCWD, containerName)
		arguments = append(arguments[:1], append([]string{"--interactive"}, arguments[1:]...)...)
		keys := make([]string, 0, len(environment))
		for key := range environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		// 环境选项插在镜像和服务器 argv 前，值仅存在于 Docker CLI 的私有环境。
		imageIndex := len(arguments) - len(argv) - 1
		options := make([]string, 0, 2*len(keys))
		for _, key := range keys {
			options = append(options, "--env", key)
		}
		arguments = append(arguments[:imageIndex], append(options, arguments[imageIndex:]...)...)
		command = exec.Command(dockerBinary, arguments...)
		command.Dir = tool.Workspace.Root
		command.Env = mergedEnvironment(nil, environment)
		cleanup = func() error { return tool.removeDockerContainer(dockerBinary, containerName) }
	default:
		return nil, errors.New("invalid MCP process backend")
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stderr = io.Discard
	command.WaitDelay = tool.Grace
	reader, err := command.StdoutPipe()
	if err != nil {
		return nil, errors.New("create MCP stdout failed")
	}
	writer, err := command.StdinPipe()
	if err != nil {
		_ = reader.Close()
		return nil, errors.New("create MCP stdin failed")
	}
	if err := command.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, errors.New("start MCP process failed")
	}
	grace := tool.Grace
	if grace <= 0 {
		grace = mcpDefaultTerminationGrace
	}
	process := &MCPProcess{Reader: reader, Writer: writer, command: command, done: make(chan struct{}), grace: grace, cleanup: cleanup}
	go func() { _ = command.Wait(); close(process.done) }()
	return process, nil
}

// Close 先关闭输入，再以 TERM/KILL 回收整个进程组；失败不能被解释为取消成功。
func (process *MCPProcess) Close() error {
	process.once.Do(func() {
		_ = process.Writer.Close()
		select {
		case <-process.done:
		case <-time.After(process.grace):
		}
		_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGTERM)
		select {
		case <-process.done:
		case <-time.After(process.grace):
		}
		_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGKILL)
		select {
		case <-process.done:
		case <-time.After(mcpProcessExitConfirmTimeout):
			process.closeError = errors.New("MCP process exit is unconfirmed")
		}
		_ = process.Reader.Close()
		if process.cleanup != nil {
			if err := process.cleanup(); err != nil {
				process.closeError = errors.Join(process.closeError, err)
			}
		}
	})
	return process.closeError
}
