// 受限进程与代理环境。
// Author: fxbin
package benchmark

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (runner Runner) now() time.Time {
	if runner.Now != nil {
		return runner.Now().UTC()
	}
	return time.Now().UTC()
}

// cairnRevision 获取当前 Cairn 代码版本，缺失 Git 元数据时拒绝产生不可追溯结果。
func cairnRevision(root string) (string, error) {
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = current
	}
	output, err := runProcessOutput(context.Background(), root, []string{"git", "rev-parse", "HEAD"})
	if err != nil {
		return "", fmt.Errorf("resolve Cairn revision: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// gitRevision 返回 disposable clone 当前检出的完整提交，用于验证它确为所声明的基线。
func gitRevision(ctx context.Context, workspace string) (string, error) {
	output, err := runProcessOutput(ctx, workspace, []string{"git", "rev-parse", "HEAD"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// commandDirectory 将合同 cwd 解析为 clone 内路径，并再次检查目录存在性。
func commandDirectory(workspace, relative string) (string, error) {
	if err := validateRelativePath(relative); err != nil {
		return "", err
	}
	directory := filepath.Join(workspace, relative)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("command cwd %q is not a workspace directory", relative)
	}
	return directory, nil
}

// runProcess 执行受 runner 控制的 argv，并在失败时返回截断的非敏感 stderr 摘要。
func runProcess(ctx context.Context, directory string, argv []string) error {
	_, err := runProcessOutput(ctx, directory, argv)
	return err
}

// runProcessWithProxy 只向用户显式指定的无凭据 loopback proxy 传递预备网络流量。
func runProcessWithProxy(ctx context.Context, directory string, argv []string, prepareProxy string) error {
	_, err := runProcessOutputWithProxy(ctx, directory, argv, prepareProxy)
	return err
}

// runProcessOutput 执行不经过 shell 的 argv，并限制诊断输出以保护结果文件体积。
func runProcessOutput(ctx context.Context, directory string, argv []string) ([]byte, error) {
	return runProcessOutputWithProxy(ctx, directory, argv, "")
}

// runProcessOutputWithProxy 在受限环境中执行预备 argv，并按需传递已校验的本地代理地址。
func runProcessOutputWithProxy(ctx context.Context, directory string, argv []string, prepareProxy string) ([]byte, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("command argv is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = directory
	command.Env = restrictedProcessEnvironment(prepareProxy)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var outputBuffer bytes.Buffer
	command.Stdout = &outputBuffer
	command.Stderr = &outputBuffer
	if err := command.Start(); err != nil {
		return nil, err
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	var err error
	select {
	case err = <-finished:
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		grace := time.NewTimer(2 * time.Second)
		select {
		case <-finished:
			if !grace.Stop() {
				select {
				case <-grace.C:
				default:
				}
			}
		case <-grace.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-finished
		}
		return truncateProcessOutput(outputBuffer.Bytes()), ctx.Err()
	}
	output := truncateProcessOutput(outputBuffer.Bytes())
	if err != nil {
		return output, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// truncateProcessOutput 为失败摘要和私有结果制品保留有限、可诊断的命令输出。
func truncateProcessOutput(output []byte) []byte {
	if len(output) > 64*1024 {
		output = append(append([]byte(nil), output[:64*1024]...), []byte("\n[output truncated]\n")...)
	}
	return output
}

// restrictedProcessEnvironment 为 clone 和受控 Docker CLI 只提供运行工具所需的基础
// 变量，不把模型 API key、代理凭据或用户 HOME 中的私有状态交给目标仓库代码。
func restrictedProcessEnvironment(prepareProxy string) []string {
	environment := []string{"HOME=" + os.TempDir(), "TMPDIR=" + os.TempDir(), "LANG=C", "GIT_TERMINAL_PROMPT=0"}
	if path := os.Getenv("PATH"); path != "" {
		environment = append(environment, "PATH="+path)
	}
	if prepareProxy != "" {
		environment = append(environment, "HTTP_PROXY="+prepareProxy, "HTTPS_PROXY="+prepareProxy, "http_proxy="+prepareProxy, "https_proxy="+prepareProxy)
	}
	return environment
}

// validatePrepareProxy 只允许显式、无凭据的回环 HTTP 代理，防止将公司代理或
// 用户环境中的认证信息交给目标仓库的 clone 进程。
func validatePrepareProxy(value string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("prepare proxy must be a credential-free http loopback URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return errors.New("prepare proxy host must be loopback")
	}
	if parsed.Port() == "" {
		return errors.New("prepare proxy port is required")
	}
	return nil
}

// observationsFromEvents 从持久化事件推导基准特有观测（补丁提议/成功轮次、
// 首次补丁前探索、提出后无成功补丁、验证轮次、重复探索与零 patch 耗尽轮次预算）。
// 轮次与 token 总量属于核心合同，由 contract.Result 统计，此处不重复计数。
// 本函数只做观测，不参与熔断或完成判定。
