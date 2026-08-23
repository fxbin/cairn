//go:build e2e

// 真实进程信号语义测试：第一次 SIGINT 或 SIGTERM 必须由 Agent 持久化为取消并按
// ExitCancelled(5) 优雅退出；优雅清理被卡住时，第二次 SIGINT 不再等待、直接以
// 130 退出。与进程内 fixture 不同，本文件构建并驱动真实 cairn 二进制。
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
	"sync"
	"syscall"
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

// buildTermImmuneHelper 构建拦截 SIGTERM 的辅助进程：它先在测试专用临时目录
// 写入就绪标记，收到 SIGTERM 后写入取消传播标记但继续运行。两个标记分别证明
// 验证子进程已启动和第一次取消已传到子进程，从而为第二次 SIGINT 的 130 抢占提供
// 确定性窗口。父 cairn 进程退出后它会自行退出，避免强制退出测试遗留孤儿进程；
// 不写入 Agent 工作区，也不使用 shell 脚本。
func buildTermImmuneHelper(t *testing.T) string {
	t.Helper()
	source := "package main\n\nimport (\n\t\"os\"\n\t\"os/signal\"\n\t\"syscall\"\n\t\"time\"\n)\n\nfunc main() {\n\tif len(os.Args) != 3 {\n\t\tpanic(\"ready and cancellation paths are required\")\n\t}\n\tparentPID := os.Getppid()\n\tterminated := make(chan os.Signal, 1)\n\tsignal.Notify(terminated, syscall.SIGTERM)\n\tsignal.Ignore(syscall.SIGINT)\n\tdefer signal.Stop(terminated)\n\tgo func() {\n\t\tticker := time.NewTicker(25 * time.Millisecond)\n\t\tdefer ticker.Stop()\n\t\tfor range ticker.C {\n\t\t\tif os.Getppid() != parentPID {\n\t\t\t\tos.Exit(0)\n\t\t\t}\n\t\t}\n\t}()\n\tif err := os.WriteFile(os.Args[1], []byte(\"started\"), 0o600); err != nil {\n\t\tpanic(err)\n\t}\n\tselect {\n\tcase <-terminated:\n\t\tif err := os.WriteFile(os.Args[2], []byte(\"cancelled\"), 0o600); err != nil {\n\t\t\tpanic(err)\n\t\t}\n\t\ttime.Sleep(10 * time.Minute)\n\tcase <-time.After(10 * time.Minute):\n\t}\n}\n"
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
	return "version: 1\nprovider:\n  base_url: " + baseURL + "\n  model: signal-model\n  api_key_env: CAIRN_E2E_SIGNAL_KEY\n  capability:\n    tool_calls: true\n    streaming_usage: true\n  request_timeout: 120s\n"
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

// waitForFile 等待验证子进程写入测试专用就绪标记，避免把 verification_started
// 事件（它发生在命令真正启动前）误认为子进程已运行。
func waitForFile(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("check %s: %v", what, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
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

// TestFirstInterruptPersistsCancellationAndExitsGracefully 锁定单次 SIGINT 与
// SIGTERM 语义：模型流中收到第一次信号后，取消被持久化进事件日志（状态
// cancelled、哈希链完整），进程按 ExitCancelled(5) 优雅退出。
func TestFirstInterruptPersistsCancellationAndExitsGracefully(t *testing.T) {
	binary := buildCairnBinary(t)
	for _, signalCase := range []struct {
		name   string
		signal os.Signal
	}{
		{name: "sigint", signal: os.Interrupt},
		{name: "sigterm", signal: syscall.SIGTERM},
	} {
		t.Run(signalCase.name, func(t *testing.T) {
			baseURL, arrived := startSlowProvider(t)
			workspace, stateDir := t.TempDir(), t.TempDir()
			command, stderr := launchCairnRun(t, binary, workspace, stateDir, baseSignalConfig(baseURL), "summarize the repository", &bytes.Buffer{})
			waitFor(t, arrived, "first model request")
			if err := command.Process.Signal(signalCase.signal); err != nil {
				t.Fatal(err)
			}
			if code := exitCodeOf(t, command); code != 5 {
				t.Fatalf("first %s exit = %d, want 5 (cancelled); stderr: %s", signalCase.name, code, stderr.String())
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
		})
	}
}

// TestSecondInterruptPreemptsCancellation 锁定双次
// SIGINT 语义：验证命令是忽略 SIGTERM 的辅助进程，优雅取消被迫陷入
// TerminateGrace 等待窗口；第二次信号必须立即以 130 抢占、不再等待清理。
// 第二次信号与第一次间隔 300ms，远大于标准信号内核合并窗口，保证两次独立投递。
func TestSecondInterruptPreemptsCancellation(t *testing.T) {
	binary := buildCairnBinary(t)
	helper := buildTermImmuneHelper(t)
	baseURL := startFinalProvider(t)
	workspace, stateDir := t.TempDir(), t.TempDir()
	readyPath := filepath.Join(t.TempDir(), "verification-child-ready")
	cancelledPath := filepath.Join(t.TempDir(), "verification-child-cancelled")
	configYAML := baseSignalConfig(baseURL) + "policy:\n  write: allow\n  exec: allow\nverification:\n  commands:\n    - argv: [" + strconv.Quote(helper) + ", " + strconv.Quote(readyPath) + ", " + strconv.Quote(cancelledPath) + "]\n"
	stdout := &bytes.Buffer{}
	command, stderr := launchCairnRun(t, binary, workspace, stateDir, configYAML, "finish quickly", stdout)
	waitForFile(t, readyPath, "verification child process start")
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, cancelledPath, "first cancellation reaching verification child")
	time.Sleep(300 * time.Millisecond)
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	code := exitCodeOf(t, command)
	if code != 130 {
		t.Fatalf("second interrupt exit = %d, want 130 (preempted); stderr: %s; stdout: %s", code, stderr.String(), stdout.String())
	}
	// replayOnlyRun 持续校验哈希链与回放；强制退出可以发生在取消终态持久化前，
	// 终态取消持久化由上面的首次 SIGINT/SIGTERM 场景单独且严格地锁定。
	events, _ := replayOnlyRun(t, workspace, stateDir)
	operationStarted := false
	for _, event := range events {
		operationStarted = operationStarted || event.Type == domain.EventOperationStarted
	}
	if !operationStarted {
		t.Fatalf("verification child started without a persisted operation_started event: %#v", events)
	}
}
