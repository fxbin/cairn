// studio_bootstrap.go 为进程入口提供启动期桥接：本机凭据装载与正式评测
// 可信子进程重启。Studio 层不得直接依赖 credential 与 sandboxeval，入口
// 只调用这里的门面。
// Author: fxbin
package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/fxbin/cairn/internal/credential"
	"github.com/fxbin/cairn/internal/sandboxeval"
	"github.com/fxbin/cairn/internal/userdir"
	"github.com/fxbin/cairn/internal/workflow"
)

// UserConfigRoot 返回用户配置根；桌面端不得直接依赖 userdir。
func UserConfigRoot() (string, error) {
	return userdir.Root()
}

// UserDirPath 解析用户配置根下的命名目录；Studio 层经此访问用户资源。
func UserDirPath(name string) (string, error) {
	return userdir.Path(name)
}

// trustedEvaluationCommand 判断参数是否进入唯一需要可信重启的正式评测入口。
func trustedEvaluationCommand(arguments []string) bool {
	return len(arguments) >= 3 && arguments[0] == "memory" && arguments[1] == "candidate" && arguments[2] == "evaluate"
}

// LaunchTrustedEvaluationChild 从空环境重启当前二进制，只复制最小非敏感
// 白名单并注入专用沙箱 Key；父进程不解析或持久化 Key 值。
func LaunchTrustedEvaluationChild(arguments []string) (bool, int) {
	if !trustedEvaluationCommand(arguments) || os.Getenv(sandboxeval.EvaluationChildMarkerEnv) == "1" {
		return false, 0
	}
	providerKey := os.Getenv(sandboxeval.SandboxProviderKeyEnv)
	environment, err := sandboxeval.BuildEvaluationEnvironment(os.LookupEnv, sandboxeval.EvaluationProcessAllowlist(), providerKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "trusted evaluation launch failed:", err)
		return true, 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "trusted evaluation launch failed:", err)
		return true, 1
	}
	environment = append(environment, sandboxeval.EvaluationChildMarkerEnv+"=1")
	command := exec.Command(executable, arguments...)
	command.Env = environment
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return true, exitError.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "trusted evaluation child failed:", err)
		return true, 1
	}
	return true, 0
}

// LoadLocalCredentials 按 ADR-0015/0016 在进程启动时装载两级本机凭据文
// 件：当前工作区 .agent/keys.env 优先，用户级凭据文件按同名变量补缺；已
// 显式导出的环境变量优先、文件值不覆盖。装载失败只在 stderr 输出不含任
// 何密钥值的警告并继续，环境变量主路径不受影响。正式评测子进程使用空环
// 境白名单重启，不经此路径。
func LoadLocalCredentials() {
	if _, err := credential.LoadEnvFile(credential.KeysFileName); err != nil {
		fmt.Fprintln(os.Stderr, "本机凭据文件未装载:", err)
	}
	userKeys, err := credential.UserKeysPath()
	if err != nil {
		return
	}
	if _, err := credential.LoadEnvFile(userKeys); err != nil {
		fmt.Fprintln(os.Stderr, "用户级凭据文件未装载:", err)
	}
}

// WorkflowID 将字符串转换为工作流标识。
func WorkflowID(raw string) workflow.ID { return workflow.ID(raw) }
