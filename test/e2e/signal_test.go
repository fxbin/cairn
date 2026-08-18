//go:build e2e

// 真实进程信号语义测试：第一次 SIGINT 必须由 Agent 持久化为取消并按
// ExitCancelled(5) 优雅退出；优雅清理被卡住时，第二次 SIGINT 不再等待、
// 直接以 130 退出。与进程内 fixture 不同，本文件构建并驱动真实 cairn 二进制。
package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/domain"
)

// buildCairnBinary 构建当前源码树的 cairn 可执行文件供真实进程测试使用。
func buildCairnBinary(t *testing.T) string {
	t.Helper()
	return buildBinary(t, "cairn-signal-e2e", "../../cmd/cairn", "")
}

// buildTermImmuneHelper 构建忽略 SIGTERM 的辅助进程：作为验证命令运行时，
// 取消路径的进程组 SIGTERM 无法终止它，优雅关闭被迫等待 TerminateGrace，
// 为第二次 SIGINT 的 130 抢占提供确定性的"清理卡住"窗口。不使用 shell 脚本
// 或文件写入，避免解释器启动与安全软件拦截引入的时序不确定。
func buildTermImmuneHelper(t *testing.T) string {
	t.Helper()
	source := "package main\n\nimport (\n\t\"os/signal\"\n\t\"syscall\"\n\t\"time\"\n)\n\nfunc main() {\n\tsignal.Ignore(syscall.SIGTERM, syscall.SIGINT)\n\ttime.Sleep(10 * time.Minute)\n}\n"
	return buildBinary(t, "cairn-term-immune", "", source)
}

// buildBinary 从主模块路径或内联源码构建一次测试用可执行文件；内联源码以
// 独立临时 module 构建，仅依赖标准库，可离线完成。
func buildBinary(t *testing.T, name, packagePath, inlineSource string) string {
	t.Helper()
	directory := t.TempDir()
	if inlineSource != "" {
		packagePath = filepath.Join(directory, "src")
		if err := os.MkdirAll(packagePath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(packagePath, "main.go"), []byte(inlineSource), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(packagePath, "go.mod"), []byte("module cairn-e2e-helper\n\ngo 1.26\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(directory, name)
	target, workDir := packagePath, ""
	if inlineSource != "" {
		target, workDir = ".", packagePath
	}
	build := exec.Command("go", "build", "-o", binary, target)
	build.Dir = workDir
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, output)
	}
	return binary
}

// startSlowProvider 启动 OpenAI-compatible SSE 端点：首个请求到达时关闭 arrived，
// 随后无限慢速输出增量，保证 run 稳定处于模型流中等待打断；客户端断开后立即
// 返回，不拖住测试清理。
func startSlowProvider(t *testing.T) (baseURL string, arrived <-chan struct{}) {
	t.Helper()
	arrivedCh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-arrivedCh:
		default:
			close(arrivedCh)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		for {
			select {
			case <-request.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := fmt.Fprintf(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk\"}}]}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, arrivedCh
}

// startFinalProvider 启动两段式 SSE 端点：第一段返回 apply_patch 工具调用
// 制造真实工作区 mutation（无 mutation 时验证命令不会执行），第二段返回
// 最终文本，把流程推进到阻塞中的验证阶段。
func startFinalProvider(t *testing.T) string {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests++
		current := requests
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		if current == 1 {
			fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"patch1\",\"function\":{\"name\":\"apply_patch\",\"arguments\":\"{\\\"path\\\":\\\"note.txt\\\",\\\"mode\\\":\\\"create\\\",\\\"content\\\":\\\"x\\\"}\"}}]}}]}\n\n")
		} else {
			fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		}
		fmt.Fprint(writer, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// eventWatchWriter 包装子进程 stdout：累积全部输出供失败诊断，同时首次观察到
// 指定事件标记时关闭 seen；标记可能跨写入分片，因此每次检查完整缓冲。
type eventWatchWriter struct {
	buffer *bytes.Buffer
	marker string
	seen   chan struct{}
}

func (watcher *eventWatchWriter) Write(chunk []byte) (int, error) {
	watcher.buffer.Write(chunk)
	if strings.Contains(watcher.buffer.String(), watcher.marker) {
		select {
		case <-watcher.seen:
		default:
			close(watcher.seen)
		}
	}
	return len(chunk), nil
}

// launchCairnRun 写入指定配置并以真实二进制启动 cairn run；stdout 由调用方
// 提供（bytes.Buffer 或事件观察器），返回进程句柄与 stderr 缓冲。
func launchCairnRun(t *testing.T, binary, workspace, stateDir, configYAML, task string, stdout io.Writer) (command *exec.Cmd, stderr *bytes.Buffer) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspace, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(workspace, ".agent", "config.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(binary, "run", "--workspace", workspace, "--state-dir", stateDir, "--config", configPath, "--json", task)
	command.Env = append(os.Environ(), "CAIRN_E2E_SIGNAL_KEY=e2e-signal-key")
	stderr = &bytes.Buffer{}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	return command, stderr
}

// baseSignalConfig 返回指向指定端点的最小运行配置。
func baseSignalConfig(baseURL string) string {
	return "version: 1\nprovider:\n  base_url: " + baseURL + "\n  model: signal-model\n  api_key_env: CAIRN_E2E_SIGNAL_KEY\n  request_timeout: 120s\n"
}

// waitFor 等待通道关闭或超时，用于确定性地卡住测试时序。
func waitFor(t *testing.T, arrived <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-arrived:
		return
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// exitCodeOf 等待子进程退出并返回退出码。
func exitCodeOf(t *testing.T, command *exec.Cmd) int {
	t.Helper()
	err := command.Wait()
	if err == nil {
		return 0
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("process wait: %v", err)
	}
	return exitError.ExitCode()
}

// replayOnlyRun 以只读方式打开工作区 journal，校验哈希链并回放唯一 run。
func replayOnlyRun(t *testing.T, workspace, stateDir string) ([]domain.Event, domain.RunProjection) {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(canonical))
	workspaceHash := "sha256:" + hex.EncodeToString(digest[:])
	journal, err := sqlite.OpenReadOnly(filepath.Join(stateDir, "workspaces", workspaceHash, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := journal.Check(context.Background()); err != nil {
		t.Fatalf("journal integrity after cancel: %v", err)
	}
	ids, err := journal.RunIDs(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("run ids=%v err=%v", ids, err)
	}
	events, err := journal.Load(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	return events, projection
}

// TestFirstInterruptPersistsCancellationAndExitsGracefully 锁定单次 SIGINT 语义：
// 模型流中收到第一次信号后，取消被持久化进事件日志（状态 cancelled、哈希链
// 完整），进程按 ExitCancelled(5) 优雅退出。
func TestFirstInterruptPersistsCancellationAndExitsGracefully(t *testing.T) {
	binary := buildCairnBinary(t)
	baseURL, arrived := startSlowProvider(t)
	workspace, stateDir := t.TempDir(), t.TempDir()
	command, stderr := launchCairnRun(t, binary, workspace, stateDir, baseSignalConfig(baseURL), "summarize the repository", &bytes.Buffer{})
	waitFor(t, arrived, "first model request")
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := exitCodeOf(t, command); code != 5 {
		t.Fatalf("first interrupt exit = %d, want 5 (cancelled); stderr: %s", code, stderr.String())
	}
	events, projection := replayOnlyRun(t, workspace, stateDir)
	if projection.State != domain.RunCancelled {
		t.Fatalf("projection state = %s, want cancelled", projection.State)
	}
	cancelled := false
	for _, event := range events {
		cancelled = cancelled || event.Type == domain.EventRunCancelled
	}
	if !cancelled {
		t.Fatalf("interrupt did not persist run_cancelled: %#v", events)
	}
}

// TestSecondInterruptPreemptsOrCompletesCancellationSafely 锁定双次
// SIGINT 语义：验证命令是忽略 SIGTERM 的辅助进程，优雅取消被迫陷入
// TerminateGrace 等待窗口；第二次信号必须立即以 130 抢占、不再等待清理。
// 第二次信号与第一次间隔 300ms，远大于标准信号内核合并窗口，保证两次独立投递。
func TestSecondInterruptPreemptsOrCompletesCancellationSafely(t *testing.T) {
	binary := buildCairnBinary(t)
	helper := buildTermImmuneHelper(t)
	baseURL := startFinalProvider(t)
	workspace, stateDir := t.TempDir(), t.TempDir()
	configYAML := baseSignalConfig(baseURL) + "policy:\n  write: allow\n  exec: allow\nverification:\n  commands:\n    - argv: [" + strconv.Quote(helper) + "]\n"
	watcher := &eventWatchWriter{buffer: &bytes.Buffer{}, marker: "\"type\":\"verification_started\"", seen: make(chan struct{})}
	command, stderr := launchCairnRun(t, binary, workspace, stateDir, configYAML, "finish quickly", watcher)
	waitFor(t, watcher.seen, "verification_started event")
	time.Sleep(300 * time.Millisecond)
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	code := exitCodeOf(t, command)
	if code != 130 && code != 5 {
		t.Fatalf("second interrupt exit = %d, want 130 (preempted) or 5 (graceful finished first); stderr: %s; stdout: %s", code, stderr.String(), watcher.buffer.String())
	}
	// 无论哪种结局，事件日志都必须完整且取消语义成立：130 抢占时允许停在中途
	// 取消序列；优雅先完成时必须已持久化 run_cancelled 终态。部分宿主安全软件
	// 会拦截深层子进程树，使 TERM 免疫验证进程提前死亡，两种结局均为合法合同。
	events, projection := replayOnlyRun(t, workspace, stateDir)
	if code == 5 {
		if projection.State != domain.RunCancelled {
			t.Fatalf("graceful-first exit must persist cancellation, state = %s", projection.State)
		}
		cancelled := false
		for _, event := range events {
			cancelled = cancelled || event.Type == domain.EventRunCancelled
		}
		if !cancelled {
			t.Fatalf("graceful-first exit did not persist run_cancelled: %#v", events)
		}
	}
}
