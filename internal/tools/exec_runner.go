// exec_runner.go 定义执行边界策略（Strategy）：host、seatbelt、docker
// 三种 Runner 共享同一 exec_command 合同；ExecCommand 只负责解析与选择。
// Author: fxbin
package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
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

// ExecRunner 是执行边界策略：准备命令、返回 Result、可选清理。
type ExecRunner interface {
	// ID 返回写入 Journal 的后端标识。
	ID() ExecBackend
	// Run 在已解析工作目录内执行；失败关闭，不跨后端降级。
	Run(ctx context.Context, request ExecRunRequest) Result
}

// HostRunner 直接在宿主机工作区启动 argv（无 OS 沙箱）。
type HostRunner struct{}

// ID 返回 host 后端名。
func (HostRunner) ID() ExecBackend { return ExecBackendHost }

// Run 启动宿主机进程。
func (HostRunner) Run(ctx context.Context, request ExecRunRequest) Result {
	command := exec.Command(request.Arguments.Argv[0], request.Arguments.Argv[1:]...)
	command.Dir = request.WorkingDirectory
	command.Env = mergedEnvironment(request.EnvAllow, request.Arguments.Env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return runProcess(ctx, request.Arguments, ExecBackendHost, command, nil, request.Grace)
}

// SeatbeltRunner 在宿主机执行并套 macOS Seatbelt。
type SeatbeltRunner struct{}

// ID 返回 host+seatbelt。
func (SeatbeltRunner) ID() ExecBackend { return ExecBackendSeatbeltHost }

// Run 包装 argv 后执行；缺 sandbox-exec 时失败关闭。
func (SeatbeltRunner) Run(ctx context.Context, request ExecRunRequest) Result {
	if !SeatbeltAvailable() {
		return Failure("sandbox_unavailable", "host confinement requires sandbox-exec")
	}
	writeMode := SeatbeltWriteWorkspace
	if !request.HostWrite {
		writeMode = SeatbeltWriteNone
	}
	profile, err := SeatbeltProfile(SeatbeltOptions{
		WorkspaceRoot: request.Workspace.Root,
		WriteMode:     writeMode,
		AllowNetwork:  false,
	})
	if err != nil {
		return Failure("invalid_backend", err.Error())
	}
	file, err := os.CreateTemp("", "cairn-seatbelt-*.sb")
	if err != nil {
		return Failure("sandbox_unavailable", err.Error())
	}
	profilePath := file.Name()
	if _, err := file.WriteString(profile); err != nil {
		_ = file.Close()
		_ = os.Remove(profilePath)
		return Failure("sandbox_unavailable", err.Error())
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(profilePath)
		return Failure("sandbox_unavailable", err.Error())
	}
	wrapped, err := WrapSeatbelt(profilePath, request.Arguments.Argv)
	if err != nil {
		_ = os.Remove(profilePath)
		return Failure("invalid_backend", err.Error())
	}
	command := exec.Command(wrapped[0], wrapped[1:]...)
	command.Dir = request.WorkingDirectory
	command.Env = mergedEnvironment(request.EnvAllow, request.Arguments.Env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return runProcess(ctx, request.Arguments, ExecBackendSeatbeltHost, command, func() error {
		return os.Remove(profilePath)
	}, request.Grace)
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
func (runner DockerRunner) Run(ctx context.Context, request ExecRunRequest) Result {
	if err := validateDockerImageAndPIDs(runner.Image, runner.MaxPIDs); err != nil {
		return Failure("invalid_backend", err.Error())
	}
	if strings.Contains(runner.Workspace.Root, ",") {
		return Failure("invalid_path", "docker sandbox does not support a workspace path containing a comma")
	}
	dockerBinary, err := exec.LookPath("docker")
	if err != nil {
		return Failure("sandbox_unavailable", fmt.Sprintf("docker sandbox is unavailable: %v", err))
	}
	name := dockerContainerName()
	if runner.ContainerID != nil {
		name = runner.ContainerID()
	}
	command := dockerRunCommand(dockerBinary, runner.Workspace, runner.Image, runner.MaxPIDs, request.Arguments, request.RelativeCWD, name)
	return runProcess(ctx, request.Arguments, ExecBackendDocker, command, func() error {
		return removeDockerContainerByID(dockerBinary, name)
	}, request.Grace)
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
func runProcess(ctx context.Context, arguments ExecArguments, backend ExecBackend, command *exec.Cmd, cleanup func() error, grace time.Duration) Result {
	return runCommandShared(ctx, arguments, backend, command, cleanup, grace)
}
