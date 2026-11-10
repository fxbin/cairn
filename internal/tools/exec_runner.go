// exec_runner.go 定义执行边界策略（Strategy）：host、seatbelt、docker
// 三种 Runner 共享同一 exec_command 合同；ExecCommand 只负责解析与选择。
// Author: fxbin
package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/fxbin/cairn/internal/contract"
	"os/exec"
)

// ExecRunRequest 是 Runner 的统一执行输入。
type ExecRunRequest struct {
	Arguments        ExecArguments
	WorkingDirectory string
	RelativeCWD      string
	Workspace        Workspace
	EnvAllow         map[string]struct{}
	// HostWrite 供 seatbelt 写档使用。
	HostWrite bool
	// Grace 是取消后的终止宽限。
	Grace time.Duration
}

// sandboxTermination 描述 OS 沙箱包装层的取消投递方案。
//
// Linux bubblewrap 不转发信号，自身收到 SIGTERM 会立即退出，并由
// --die-with-parent 的 PR_SET_PDEATHSIG 连带 SIGKILL 内部 workload；若取消仍
// 按进程组投递，TerminateGrace 将没有可等待对象。支持子进程 PID 上报的包装
// 层因此把优雅取消直接作用于内部 workload，包装进程保持存活，直到内部进程
// 退出或宽限期结束后的进程组 SIGKILL 兜底。零值表示该平台沿用进程组语义。
type sandboxTermination struct {
	// childPIDFd 是包装层上报内部 workload PID 的描述符编号。
	childPIDFd int
	// extraFiles 是启动命令时要继承的上报描述符（ExtraFiles 从 fd 3 开始）。
	extraFiles []*os.File
	// afterStart 在进程启动后关闭父进程副本并异步解析上报的 PID。
	afterStart func()
	// graceful 向内部 workload 投递 SIGTERM；返回是否已成功投递。
	graceful func() bool
	// close 释放父进程持有的描述符，用于启动或包装失败路径。
	close func()
}

// ExecRunner 是执行边界策略：准备命令、返回 Result、可选清理。
type ExecRunner interface {
	// ID 返回写入 Journal 的后端标识。
	ID() ExecBackend
	// Run 在已解析工作目录内执行；失败关闭，不跨后端降级。
	Run(ctx context.Context, request ExecRunRequest) contract.ToolResult
}

// HostRunner 直接在宿主机工作区启动 argv（无 OS 沙箱）。
type HostRunner struct{}

// ID 返回 host 后端名。
func (HostRunner) ID() ExecBackend { return ExecBackendHost }

// Run 启动宿主机进程。
func (HostRunner) Run(ctx context.Context, request ExecRunRequest) contract.ToolResult {
	command := exec.Command(request.Arguments.Argv[0], request.Arguments.Argv[1:]...)
	command.Dir = request.WorkingDirectory
	command.Env = mergedEnvironment(request.EnvAllow, request.Arguments.Env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return runProcess(ctx, request.Arguments, ExecBackendHost, command, nil, request.Grace, nil)
}

// SeatbeltRunner 在宿主机执行并套 OS 约束（macOS Seatbelt / Linux bubblewrap）。
type SeatbeltRunner struct{}

// ID 返回 host+os-sandbox。
func (SeatbeltRunner) ID() ExecBackend { return ExecBackendSeatbeltHost }

// Run 包装 argv 后执行；缺少 OS 沙箱时失败关闭。
func (SeatbeltRunner) Run(ctx context.Context, request ExecRunRequest) contract.ToolResult {
	writeMode := SeatbeltWriteWorkspace
	if !request.HostWrite {
		writeMode = SeatbeltWriteNone
	}
	termination, terminationErr := newSandboxTermination()
	if terminationErr != nil {
		return contract.ToolFailure("sandbox_unavailable", terminationErr.Error())
	}
	options := SeatbeltOptions{
		WorkspaceRoot: request.Workspace.Root,
		WriteMode:     writeMode,
		AllowNetwork:  false,
	}
	if termination != nil {
		options.ChildPIDFd = termination.childPIDFd
	}
	wrapped, cleanup, err := ConfineArgv(options, request.Arguments.Argv)
	if err != nil {
		if termination != nil {
			termination.close()
		}
		return contract.ToolFailure("sandbox_unavailable", err.Error())
	}
	command := exec.Command(wrapped[0], wrapped[1:]...)
	command.Dir = request.WorkingDirectory
	command.Env = mergedEnvironment(request.EnvAllow, request.Arguments.Env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if termination != nil {
		command.ExtraFiles = termination.extraFiles
	}
	return runProcess(ctx, request.Arguments, ExecBackendSeatbeltHost, command, cleanup, request.Grace, termination)
}

// DockerRunner 在显式镜像容器内执行（无网络、只读根）。
type DockerRunner struct {
	Image       string
	MaxPIDs     int
	Workspace   Workspace
	ContainerID func() string
}

// ID 返回 docker 后端名。
func (DockerRunner) ID() ExecBackend { return ExecBackendDocker }

// Run 构造并启动 docker run；不可用时失败关闭。
func (runner DockerRunner) Run(ctx context.Context, request ExecRunRequest) contract.ToolResult {
	if err := validateDockerImageAndPIDs(runner.Image, runner.MaxPIDs); err != nil {
		return contract.ToolFailure("invalid_backend", err.Error())
	}
	if strings.Contains(runner.Workspace.Root, ",") {
		return contract.ToolFailure("invalid_path", "docker sandbox does not support a workspace path containing a comma")
	}
	dockerBinary, err := exec.LookPath("docker")
	if err != nil {
		return contract.ToolFailure("sandbox_unavailable", fmt.Sprintf("docker sandbox is unavailable: %v", err))
	}
	name := dockerContainerName()
	if runner.ContainerID != nil {
		name = runner.ContainerID()
	}
	command := dockerRunCommand(dockerBinary, runner.Workspace, runner.Image, runner.MaxPIDs, request.Arguments, request.RelativeCWD, name)
	return runProcess(ctx, request.Arguments, ExecBackendDocker, command, func() error {
		return removeDockerContainerByID(dockerBinary, name)
	}, request.Grace, nil)
}

// runnerFor 按后端与是否收紧 host 选择策略；未知后端失败关闭。
func runnerFor(backend ExecBackend, workspace Workspace, hostConfined, hostWrite bool, dockerImage string, dockerMaxPIDs int) (ExecRunner, error) {
	switch backend {
	case ExecBackendHost, ExecBackendSeatbeltHost:
		if hostConfined {
			return SeatbeltRunner{}, nil
		}
		return HostRunner{}, nil
	case ExecBackendDocker:
		return DockerRunner{
			Image:     dockerImage,
			MaxPIDs:   dockerMaxPIDs,
			Workspace: workspace,
			ContainerID: func() string {
				return dockerContainerName()
			},
		}, nil
	default:
		return nil, fmt.Errorf("exec sandbox backend must be host or docker")
	}
}

// runProcess 是 Runner 共享的进程生命周期实现（超时、进程组、清理）。
func runProcess(ctx context.Context, arguments ExecArguments, backend ExecBackend, command *exec.Cmd, cleanup func() error, grace time.Duration, termination *sandboxTermination) contract.ToolResult {
	return runCommandShared(ctx, arguments, backend, command, cleanup, grace, termination)
}
