// 克隆、引导与私有 verifier 注入。
// Author: fxbin
package benchmark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
)

func applyPrivateVerifier(ctx context.Context, workspace, privateRoot, taskID string) (string, error) {
	root, err := filepath.Abs(privateRoot)
	if err != nil {
		return "", err
	}
	patchPath := filepath.Join(root, "verifiers", safePathComponent(taskID)+".patch")
	contents, err := os.ReadFile(patchPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("private verifier patch is required at %s", patchPath)
		}
		return "", fmt.Errorf("read private verifier patch: %w", err)
	}
	if len(contents) == 0 {
		return "", errors.New("private verifier patch is empty")
	}
	if err := runProcess(ctx, workspace, []string{"git", "apply", "--check", patchPath}); err != nil {
		return "", fmt.Errorf("private verifier check: %w", err)
	}
	if err := runProcess(ctx, workspace, []string{"git", "apply", patchPath}); err != nil {
		return "", fmt.Errorf("apply private verifier: %w", err)
	}
	digest := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// configurationForTask 冻结任务专属的 Docker 镜像和固定验证命令，拒绝 host 后端与审批暂停；
// 无论本机冻结配置声明何种 Preset，benchmark 尝试一律按 benchmark Preset 冻结，
// 使结果、指纹与 capability manifest 绑定真实执行形态而非误标为 coding。
func configurationForTask(base config.Config, task Task) (config.Config, error) {
	configuration := base
	configuration.Preset = config.PresetBenchmark
	configuration = config.WithPresetResolution(configuration, config.PresetBenchmark, "benchmark:runner")
	configuration.Sandbox.Backend = "docker"
	configuration.Sandbox.DockerImage = task.Runtime.DockerImage
	verificationEnvironment := make(map[string]string, len(task.Verification.Command.Env))
	for key, value := range task.Verification.Command.Env {
		verificationEnvironment[key] = value
		configuration.Policy.EnvAllow = append(configuration.Policy.EnvAllow, key)
	}
	configuration.Verification.VolatilePaths = append([]string(nil), task.Verification.VolatilePaths...)
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: append([]string(nil), task.Verification.Command.Argv...), CWD: task.Verification.Command.CWD, Timeout: "10m", MaxOutputBytes: 1024 * 1024, Env: verificationEnvironment}}
	if configuration.Policy.Write != "allow" || configuration.Policy.Exec != "allow" {
		return config.Config{}, errors.New("benchmark config requires policy.write=allow and policy.exec=allow for unattended audited runs")
	}
	if err := config.Validate(configuration); err != nil {
		return config.Config{}, err
	}
	return configuration, nil
}

// cloneAtBaseline 调用可替换的资格/正式 clone 实现；测试替身只能替换该
// 受控网络边界，生产默认使用本地优先的 mirrorCloneAtBaseline。
func (runner Runner) cloneAtBaseline(ctx context.Context, task Task, workspace, prepareProxy, workspacesRoot string) error {
	if runner.CloneAtBaseline != nil {
		return runner.CloneAtBaseline(ctx, task, workspace, prepareProxy)
	}
	return mirrorCloneAtBaseline(ctx, task, workspace, prepareProxy, workspacesRoot)
}

// applyPrivateVerifier 调用可替换的 evaluator-only verifier 注入，实现
// qualification 与正式尝试共享同一摘要契约而不共享工作区。
func (runner Runner) applyPrivateVerifier(ctx context.Context, workspace, privateRoot, taskID string) (string, error) {
	if runner.ApplyPrivateVerifier != nil {
		return runner.ApplyPrivateVerifier(ctx, workspace, privateRoot, taskID)
	}
	return applyPrivateVerifier(ctx, workspace, privateRoot, taskID)
}

// runBootstrap 调用可替换的无网络 Docker 预备步骤，保留正式执行时的
// fail-closed 默认实现，并让单元测试无需真实 Docker。
func (runner Runner) runBootstrap(ctx context.Context, workspace, image string, commands []Command) error {
	if runner.RunBootstrap != nil {
		return runner.RunBootstrap(ctx, workspace, image, commands)
	}
	return runBootstrap(ctx, workspace, image, commands)
}

// dockerImageAvailable 调用可替换的本地镜像检查，生产路径绝不隐式拉取镜像。
func (runner Runner) dockerImageAvailable(ctx context.Context, image string) error {
	if runner.DockerImageAvailable != nil {
		return runner.DockerImageAvailable(ctx, image)
	}
	return dockerImageAvailable(ctx, image)
}

// cloneAtBaseline 通过 argv 固定从合同 URL 直接创建新的 clone 并检出不可变
// 基线提交（历史直连实现，保留为镜像路径的网络兜底语义参照与对照实现；
// 生产路径见 mirror.go 的 mirrorCloneAtBaseline）。
func cloneAtBaseline(ctx context.Context, task Task, workspace, prepareProxy string) error {
	if err := retryTransient(func() error {
		return runProcessWithProxy(ctx, "", []string{"git", "-c", "http.version=HTTP/1.1", "clone", "--filter=blob:none", "--no-checkout", task.Repository.URL, workspace}, prepareProxy)
	}); err != nil {
		return fmt.Errorf("clone %s: %w", task.Repository.Name, err)
	}
	if err := retryTransient(func() error {
		return runProcessWithProxy(ctx, workspace, []string{"git", "-c", "http.version=HTTP/1.1", "checkout", "--detach", task.Repository.BaselineCommit}, prepareProxy)
	}); err != nil {
		return fmt.Errorf("checkout baseline %s: %w", task.Repository.BaselineCommit, err)
	}
	return nil
}

// prepareRetryAttempts 与 prepareRetryBackoff 是准备阶段瞬态故障的重试参数；
// 上限保守以避免掩盖持续性的环境问题（最终仍按 environment_invalid 失败关闭）。
// 退避是变量以便测试将其置零。
const prepareRetryAttempts = 3

var prepareRetryBackoff = 5 * time.Second

// retryTransient 对返回错误的操作做固定次数重试；上下文取消时不重试。
func retryTransient(operation func() error) error {
	var err error
	for attempt := 0; attempt < prepareRetryAttempts; attempt++ {
		if err = operation(); err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if attempt+1 < prepareRetryAttempts {
			time.Sleep(prepareRetryBackoff)
		}
	}
	return err
}

// runBootstrap 在计费窗口之外按合同 argv 在任务专属 Docker 镜像中准备依赖；它
// 不经过 Agent 工具或模型上下文，也不把代理、模型密钥或宿主机 HOME 传给目标仓库代码。
func runBootstrap(ctx context.Context, workspace, image string, commands []Command) error {
	for index, command := range commands {
		arguments, err := bootstrapDockerArguments(workspace, image, command)
		if err != nil {
			return err
		}
		if err := runProcess(ctx, "", arguments); err != nil {
			return fmt.Errorf("bootstrap command %d: %w", index, err)
		}
	}
	return nil
}

// benchmarkDockerPIDLimit 是评测 bootstrap 的固定 PID 上限：与任务合同
// 一致，不接受调用方改写。
const benchmarkDockerPIDLimit = 64

// bootstrapDockerArguments 将合同的预备命令收敛为无网络、只读根文件系统的 Docker
// argv；工作区是唯一可写 bind mount，供任务镜像已经具备的本地编译缓存按需复用。
func bootstrapDockerArguments(workspace, image string, command Command) ([]string, error) {
	if _, err := commandDirectory(workspace, command.CWD); err != nil {
		return nil, err
	}
	if strings.Contains(workspace, ",") {
		return nil, errors.New("benchmark docker workspace path cannot contain a comma")
	}
	if image == "" || image != strings.TrimSpace(image) || strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\r\n\x00") {
		return nil, errors.New("benchmark bootstrap requires a valid task docker image")
	}
	containerCWD := "/workspace"
	if command.CWD != "." {
		containerCWD = path.Join(containerCWD, filepath.ToSlash(command.CWD))
	}
	arguments := []string{"docker", "run", "--rm"}
	arguments = append(arguments, tools.DockerSandboxBaseArgs(benchmarkDockerPIDLimit, workspace, containerCWD)...)
	arguments = append(arguments, image)
	return append(arguments, command.Argv...), nil
}

// dockerImageAvailable 确保 Agent 运行前镜像已经在本地，runner 不会隐式拉取镜像。
func dockerImageAvailable(ctx context.Context, image string) error {
	if err := runProcess(ctx, "", []string{"docker", "image", "inspect", image}); err != nil {
		return fmt.Errorf("required docker image %q is unavailable locally: %w", image, err)
	}
	return nil
}

// captureDiff 将工作区最终 diff 写到本地结果目录，并把稳定摘要放入结果 schema。
