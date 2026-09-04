// Package main 提供 Cairn CLI 进程入口，并为正式候选评测建立空环境可信子进程。
//
// Author: fxbin
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/fxbin/cairn/internal/cli"
	"github.com/fxbin/cairn/internal/credential"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// trustedEvaluationCommand 判断参数是否进入唯一需要可信重启的正式评测入口。
func trustedEvaluationCommand(arguments []string) bool {
	return len(arguments) >= 3 && arguments[0] == "memory" && arguments[1] == "candidate" && arguments[2] == "evaluate"
}

// launchTrustedEvaluationChild 从空环境重启当前二进制，只复制最小非敏感
// 白名单并注入专用沙箱 Key；父进程不解析或持久化 Key 值。
func launchTrustedEvaluationChild(arguments []string) (bool, int) {
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

// loadLocalCredentials 按 ADR-0015/0016 在进程启动时装载两级本机凭据文
// 件：当前工作区 .agent/keys.env 优先，用户级凭据文件按同名变量补缺；已
// 显式导出的环境变量优先、文件值不覆盖。装载失败只在 stderr 输出不含任
// 何密钥值的警告并继续，环境变量主路径不受影响。正式评测子进程使用空环
// 境白名单重启，不经此路径。
func loadLocalCredentials() {
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

// main 建立可取消根上下文，并将两次中断分别映射为优雅取消和强制退出。
func main() {
	if handled, code := launchTrustedEvaluationChild(os.Args[1:]); handled {
		os.Exit(code)
	}
	loadLocalCredentials()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		<-signals
		cancel()
		// 第一次信号由 Loop 持久化为取消；第二次表示不再等待优雅清理。
		<-signals
		os.Exit(130)
	}()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
