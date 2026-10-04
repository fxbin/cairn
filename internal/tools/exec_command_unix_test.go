//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestExecCommandUsesArgvAndEnforcesEnvironmentAllowlist(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, EnvAllow: map[string]struct{}{"CAIRN_TEST": {}}}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","hello"],"cwd":".","env":{"CAIRN_TEST":"1"}}`))
	if !result.OK || result.Code != "ok" {
		t.Fatalf("result = %#v", result)
	}
	blocked := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","hello"],"env":{"UNSAFE":"1"}}`))
	if blocked.OK || blocked.Code != "invalid" {
		t.Fatalf("blocked = %#v", blocked)
	}
}

func TestExecCommandAcceptsWorkspaceAlias(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","hello"],"cwd":"/workspace"}`))
	if !result.OK || !strings.Contains(result.Preview, `"cwd":"."`) {
		t.Fatalf("result = %#v", result)
	}
	arguments, err := ParseExecArguments(json.RawMessage(`{"argv":["/usr/bin/true"],"cwd":"/workspace/subdir"}`))
	if err != nil || arguments.CWD != "subdir" {
		t.Fatalf("arguments = %#v, %v", arguments, err)
	}
}

func TestExecCommandDoesNotInheritUnallowlistedEnvironment(t *testing.T) {
	t.Setenv("CAIRN_SHOULD_NOT_LEAK", "private-value")
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, EnvAllow: map[string]struct{}{}}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/usr/bin/env"]}`))
	if !result.OK || strings.Contains(result.Preview, "CAIRN_SHOULD_NOT_LEAK") || strings.Contains(result.Preview, "private-value") {
		t.Fatalf("environment isolation failed: %#v", result)
	}
}

func TestExecCommandDockerBuildsRestrictedInvocationWithoutHostEnvironment(t *testing.T) {
	t.Setenv("AGENT_API_KEY", "must-not-reach-docker")
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, Backend: ExecBackendDocker, DockerImage: "alpine:3.21", DockerMaxPIDs: 37}
	arguments, err := ParseExecArguments(json.RawMessage(`{"argv":["/bin/echo","hello"],"cwd":"/workspace/subdir","env":{"LANG":"C"}}`))
	if err != nil {
		t.Fatal(err)
	}
	command := tool.dockerCommand("/fake/docker", arguments, "subdir", "cairn-test")
	joined := strings.Join(command.Args, "\n")
	for _, required := range []string{
		"--pull\nnever", "--network\nnone", "--read-only", "--cap-drop\nALL", "--security-opt\nno-new-privileges",
		"--pids-limit\n37", "--mount\ntype=bind,src=" + workspace.Root + ",dst=/workspace", "--workdir\n/workspace/subdir",
		"--env\nLANG=C", "alpine:3.21", "/bin/echo\nhello",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("docker args missing %q: %q", required, command.Args)
		}
	}
	if strings.Count(joined, "--mount") != 1 || strings.Contains(joined, "must-not-reach-docker") || strings.Contains(joined, "AGENT_API_KEY") {
		t.Fatalf("docker bind/environment boundary = %q", command.Args)
	}
	if command.Dir != workspace.Root || command.Env == nil || len(command.Env) != 0 {
		t.Fatalf("docker client inherited host state: dir=%q env=%#v", command.Dir, command.Env)
	}
}

func TestExecCommandDockerFailsClosedWhenDockerIsUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace.Root, "host-command-ran")
	tool := ExecCommand{Workspace: workspace, Backend: ExecBackendDocker, DockerImage: "alpine:3.21", DockerMaxPIDs: 64}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/usr/bin/touch","host-command-ran"]}`))
	if result.OK || result.Code != "sandbox_unavailable" {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("docker fallback executed host argv: %v", err)
	}
}

func TestExecCommandRejectsExplicitSecretLikeEnvironmentBeforeDockerLaunch(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{
		Workspace:     workspace,
		EnvAllow:      map[string]struct{}{"AGENT_API_KEY": {}},
		Backend:       ExecBackendDocker,
		DockerImage:   "alpine:3.21",
		DockerMaxPIDs: 64,
	}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","never-runs"],"env":{"AGENT_API_KEY":"secret"}}`))
	if result.OK || result.Code != "invalid" || !strings.Contains(result.Preview, "secret-like") {
		t.Fatalf("result = %#v", result)
	}
}

func TestExecCommandDockerTimeoutAttemptsContainerCleanup(t *testing.T) {
	directory := t.TempDir()
	cleanupMarker := filepath.Join(directory, "cleanup")
	dockerPath := filepath.Join(directory, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"run\" ]; then\n" +
		"  trap 'exit 0' TERM\n" +
		"  while true; do /bin/sleep 1; done\n" +
		"fi\n" +
		"if [ \"$1\" = \"rm\" ]; then\n" +
		"  printf '%s' \"$*\" > \"" + cleanupMarker + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, Backend: ExecBackendDocker, DockerImage: "alpine:3.21", DockerMaxPIDs: 64, Grace: 100 * time.Millisecond}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","never-runs"],"timeout_ms":25}`))
	if result.OK || result.Code != "cancelled" {
		t.Fatalf("result = %#v", result)
	}
	content, err := os.ReadFile(cleanupMarker)
	if err != nil || !strings.HasPrefix(string(content), "rm --force cairn-") {
		t.Fatalf("cleanup = %q, %v", content, err)
	}
}

// TestExecCommandDockerCancellationRequiresContainerTerminationConfirmation
// 锁定 Docker 客户端已退出后仍须确认容器清理，daemon 错误不能表示容器不存在。
func TestExecCommandDockerCancellationRequiresContainerTerminationConfirmation(t *testing.T) {
	cases := []struct {
		name        string
		removeCode  int
		queryCode   int
		queryOutput string
		wantCode    string
		wantQuery   bool
	}{
		{name: "remove fails and container remains", removeCode: 1, queryOutput: "container-id\n", wantCode: CodeTerminationUnconfirmed, wantQuery: true},
		{name: "daemon cannot be queried", removeCode: 1, queryCode: 1, wantCode: CodeTerminationUnconfirmed, wantQuery: true},
		{name: "successful query confirms absence", removeCode: 1, wantCode: "cancelled", wantQuery: true},
		{name: "successful remove confirms termination", wantCode: "cancelled"},
		{name: "truncated empty looking query cannot confirm absence", removeCode: 1, queryOutput: strings.Repeat(" ", dockerCleanupQueryBytes+1), wantCode: CodeTerminationUnconfirmed, wantQuery: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			callsPath := filepath.Join(directory, "calls")
			quotedCalls := "'" + strings.ReplaceAll(callsPath, "'", "'\"'\"'") + "'"
			quotedOutput := "'" + strings.ReplaceAll(test.queryOutput, "'", "'\"'\"'") + "'"
			script := fmt.Sprintf("#!/bin/sh\n"+
				"if [ -n \"${DOCKER_HOST-}\" ]; then exit 98; fi\n"+
				"case \"$1\" in\n"+
				"run)\n"+
				"  trap 'exit 0' TERM\n"+
				"  while true; do /bin/sleep 1; done\n"+
				"  ;;\n"+
				"rm)\n"+
				"  printf '%%s\\n' \"$*\" >> %s\n"+
				"  exit %d\n"+
				"  ;;\n"+
				"container)\n"+
				"  printf '%%s\\n' \"$*\" >> %s\n"+
				"  printf '%%s' %s\n"+
				"  exit %d\n"+
				"  ;;\n"+
				"*) exit 99 ;;\n"+
				"esac\n", quotedCalls, test.removeCode, quotedCalls, quotedOutput, test.queryCode)
			if err := os.WriteFile(filepath.Join(directory, "docker"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", directory)
			t.Setenv("DOCKER_HOST", "tcp://must-not-reach-cleanup.invalid:2375")
			workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
			if err != nil {
				t.Fatal(err)
			}
			tool := ExecCommand{Workspace: workspace, Backend: ExecBackendDocker, DockerImage: "alpine:3.21", DockerMaxPIDs: 64, Grace: 100 * time.Millisecond}
			result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","never-runs"],"timeout_ms":50}`))
			if result.OK || result.Code != test.wantCode {
				t.Fatalf("result = %#v, want %s", result, test.wantCode)
			}
			if result.Code == CodeTerminationUnconfirmed && (len(result.Data) != 0 || result.Bytes != 0) {
				t.Error("终止未确认时不得发布缓冲区内容")
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			wantRemovals := dockerCleanupAttempts
			if test.removeCode == 0 {
				wantRemovals = 1
			}
			if got := strings.Count(string(calls), "rm --force cairn-"); got != wantRemovals {
				t.Fatalf("remove calls = %d, want %d: %q", got, wantRemovals, calls)
			}
			queryRan := strings.Contains(string(calls), "container ls --all --quiet --no-trunc --filter name=cairn-")
			if queryRan != test.wantQuery {
				t.Fatalf("query ran = %t, want %t: %q", queryRan, test.wantQuery, calls)
			}
		})
	}
}

func TestExecCommandDockerRuntimeWithExplicitLocalImage(t *testing.T) {
	image := os.Getenv("CAIRN_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set CAIRN_DOCKER_TEST_IMAGE to an already-local image to exercise Docker runtime")
	}
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, Backend: ExecBackendDocker, DockerImage: image, DockerMaxPIDs: 64}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/sh","-c","printf sandbox-ok"],"timeout_ms":5000}`))
	if !result.OK || !strings.Contains(result.Preview, "sandbox-ok") {
		t.Fatalf("runtime result = %#v", result)
	}
}

// TestStringResultTruncatesAtRuneBoundary 验证超限预览在 UTF-8 边界截断并带
// 确定性标记：既不以损坏的多字节字符结尾，也不静默丢失截断事实。
func TestStringResultTruncatesAtRuneBoundary(t *testing.T) {
	// 每 3 字节一个多字节字符，使 1024 边界必然落在字符中间。
	payload := strings.Repeat("世", 600)
	preview := stringResult(map[string]string{"text": payload})
	if !strings.HasSuffix(preview, "...[preview-truncated]") {
		t.Fatalf("preview missing truncation marker: %q", preview[len(preview)-40:])
	}
	if !utf8.ValidString(preview) {
		t.Fatalf("preview ends with a corrupted rune: %q", preview[len(preview)-40:])
	}
	short := stringResult(map[string]string{"text": "ok"})
	if short != `{"text":"ok"}` {
		t.Fatalf("short preview was altered: %q", short)
	}
}

// TestExecCommandHostBackendKillsProcessGroupOnTimeout 锁定 host 后端的进程组
// 取消语义：超时后后台孙进程也必须被收割，不允许孤儿进程继续产生副作用。
func TestExecCommandHostBackendKillsProcessGroupOnTimeout(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace.Root, "orphan-marker")
	tool := ExecCommand{Workspace: workspace, EnvAllow: map[string]struct{}{}}
	started := time.Now()
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/sh","-c","sleep 2; touch orphan-marker"],"timeout_ms":150}`))
	if result.Code != "cancelled" {
		t.Fatalf("result = %#v", result)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("cancellation waited %v; process group was not reaped promptly", elapsed)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("orphan descendant survived process-group cancellation: %v", statErr)
	}
}

func TestWaitForExitAfterKillDistinguishesConfirmedAndUnconfirmed(t *testing.T) {
	finished := make(chan error, 1)
	finished <- errors.New("killed")
	if err, confirmed := waitForExitAfterKill(finished, time.Second); !confirmed || err == nil {
		t.Fatalf("confirmed wait = %v, %t", err, confirmed)
	}
	blocked := make(chan error)
	if err, confirmed := waitForExitAfterKill(blocked, time.Millisecond); confirmed || err == nil {
		t.Fatalf("unconfirmed wait = %v, %t", err, confirmed)
	}
}

// TestDockerRunArgumentsMapHostUser 锁定容器以宿主用户身份运行：可写 bind
// mount 的产出文件不会成为 root 属主。
func TestDockerRunArgumentsMapHostUser(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, Backend: "docker", DockerImage: "alpine:3.21", DockerMaxPIDs: 16}
	arguments := ExecArguments{Argv: []string{"/bin/true"}, CWD: ".", TimeoutMS: 1000, MaxOutputBytes: 4096}
	want := "--user\n" + fmt.Sprint(os.Getuid()) + ":" + fmt.Sprint(os.Getgid())
	joined := strings.Join(tool.dockerRunArguments(arguments, ".", "cairn-test"), "\n")
	if !strings.Contains(joined, want) {
		t.Fatalf("docker args missing host user mapping %q: %q", want, joined)
	}
}

// TestExecCommandFailedOutputPreservesDataAndDiagnosticTails 覆盖已结束失败命令的完整输出与压缩可见尾部。
func TestExecCommandFailedOutputPreservesDataAndDiagnosticTails(t *testing.T) {
	for _, mode := range []string{"stdout", "stderr", "both"} {
		t.Run(mode, func(t *testing.T) {
			result := runExecFeedbackHelper(t, mode, 8192, 5000)
			if result.OK || result.Code != "nonzero_exit" {
				t.Fatalf("命令结果状态: ok=%t code=%s", result.OK, result.Code)
			}
			var data struct {
				Argv         []string `json:"argv"`
				Backend      string   `json:"backend"`
				CWD          string   `json:"cwd"`
				ExitCode     int      `json:"exit_code"`
				Stdout       string   `json:"stdout"`
				Stderr       string   `json:"stderr"`
				OutputCapped bool     `json:"output_capped"`
			}
			if err := json.Unmarshal(result.Data, &data); err != nil {
				t.Errorf("已结束的失败命令没有有效结构化 Data: %v", err)
			}
			if data.ExitCode != 7 || data.Backend != "host" || data.CWD != "." || len(data.Argv) == 0 || data.OutputCapped || result.Truncated {
				t.Errorf("失败结果元数据不完整: exit=%d backend=%s capped=%t", data.ExitCode, data.Backend, data.OutputCapped)
			}
			for _, stream := range []struct {
				name   string
				text   string
				marker string
			}{
				{name: "stdout", text: data.Stdout, marker: "STDOUT_DIAGNOSTIC_TAIL"},
				{name: "stderr", text: data.Stderr, marker: "STDERR_DIAGNOSTIC_TAIL"},
			} {
				if mode != stream.name && mode != "both" {
					continue
				}
				if len(stream.text) < 1024 || !strings.Contains(stream.text, stream.marker) {
					t.Errorf("%s 完整捕获输出丢失", stream.name)
				}
				if !strings.Contains(result.Preview, stream.marker) {
					t.Errorf("%s 关键尾部不在预览中", stream.name)
				}
			}
			if len(result.Preview) > 480 || !utf8.ValidString(result.Preview) || !json.Valid([]byte(result.Preview)) {
				t.Errorf("失败预览不满足压缩可见的有界 UTF-8 JSON: bytes=%d", len(result.Preview))
			}
		})
	}
}

// TestExecCommandOutputCapPreservesUTF8 验证成功与失败输出跨多字节边界时，Data 和预览仍有界有效。
func TestExecCommandOutputCapPreservesUTF8(t *testing.T) {
	const maximum = 1025
	for _, mode := range []string{"cap", "cap_success"} {
		t.Run(mode, func(t *testing.T) {
			result := runExecFeedbackHelper(t, mode, maximum, 5000)
			wantCode := "nonzero_exit"
			if mode == "cap_success" {
				wantCode = "ok"
			}
			if result.Code != wantCode || result.OK != (mode == "cap_success") || !result.Truncated || result.Bytes != 2*maximum {
				t.Fatalf("有界输出状态错误: code=%s bytes=%d truncated=%t", result.Code, result.Bytes, result.Truncated)
			}
			var data struct {
				Stdout       string `json:"stdout"`
				Stderr       string `json:"stderr"`
				OutputCapped bool   `json:"output_capped"`
			}
			if err := json.Unmarshal(result.Data, &data); err != nil {
				t.Fatal(err)
			}
			if !data.OutputCapped || len(data.Stdout) > maximum || len(data.Stderr) > maximum || !utf8.ValidString(data.Stdout) || !utf8.ValidString(data.Stderr) {
				t.Fatalf("结构化输出越界或损坏: stdout=%d stderr=%d capped=%t", len(data.Stdout), len(data.Stderr), data.OutputCapped)
			}
			if !utf8.ValidString(result.Preview) || (mode == "cap" && len(result.Preview) > 480) {
				t.Fatalf("有界预览损坏: bytes=%d", len(result.Preview))
			}
		})
	}
}

// TestExecCommandConfirmedCancellationKeepsOutput 验证已确认终止的取消命令保留可诊断输出。
func TestExecCommandConfirmedCancellationKeepsOutput(t *testing.T) {
	result := runExecFeedbackHelper(t, "cancel", 8192, 500)
	if result.OK || result.Code != "cancelled" {
		t.Fatalf("取消状态错误: ok=%t code=%s", result.OK, result.Code)
	}
	var data struct {
		ExitCode int    `json:"exit_code"`
		Signal   string `json:"signal"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.ExitCode != -1 || data.Signal == "" || !strings.Contains(data.Stdout, "STDOUT_DIAGNOSTIC_TAIL") || !strings.Contains(data.Stderr, "STDERR_DIAGNOSTIC_TAIL") {
		t.Fatalf("确认取消缺少退出或输出证据: exit=%d signal=%s", data.ExitCode, data.Signal)
	}
	if !strings.Contains(result.Preview, "STDOUT_DIAGNOSTIC_TAIL") || !strings.Contains(result.Preview, "STDERR_DIAGNOSTIC_TAIL") {
		t.Error("确认取消的错误尾部没有进入预览")
	}
}

// TestExecCommandFailurePreviewRedactsBeforeTailClipping 验证截尾前脱敏，防止丢掉凭据键后暴露值片段。
func TestExecCommandFailurePreviewRedactsBeforeTailClipping(t *testing.T) {
	result := runExecFeedbackHelper(t, "sensitive", 8192, 5000)
	if result.OK || result.Code != "nonzero_exit" {
		t.Fatalf("失败状态错误: code=%s", result.Code)
	}
	if strings.Contains(result.Preview, "fixture-private-value-") || !strings.Contains(result.Preview, "[REDACTED]") || !strings.Contains(result.Preview, "STDERR_DIAGNOSTIC_TAIL") {
		t.Error("失败预览未先脱敏再保留诊断尾部")
	}
}

// runExecFeedbackHelper 以真实 argv 启动本测试进程的输出 fixture，无 shell 或全局测试钩子。
func runExecFeedbackHelper(t *testing.T, mode string, maximum, timeout int) Result {
	t.Helper()
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(ExecArguments{
		Argv: []string{executable, "-test.run=^TestExecCommandFeedbackProcess$", "--", mode},
		CWD:  ".", TimeoutMS: timeout, MaxOutputBytes: maximum,
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, Grace: 100 * time.Millisecond}
	return tool.Execute(context.Background(), arguments)
}

// TestExecCommandFeedbackProcess 仅在子进程参数指定时生成输出，普通测试调用直接返回。
func TestExecCommandFeedbackProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode == "stdout" || mode == "both" || mode == "cancel" {
		fmt.Fprint(os.Stdout, strings.Repeat("stdout-padding\n", 120)+"STDOUT_DIAGNOSTIC_TAIL\n")
	}
	if mode == "stderr" || mode == "both" || mode == "cancel" {
		fmt.Fprint(os.Stderr, strings.Repeat("stderr-padding\n", 120)+"STDERR_DIAGNOSTIC_TAIL\n")
	}
	if mode == "cap" || mode == "cap_success" {
		fmt.Fprint(os.Stdout, strings.Repeat("界", 500))
		fmt.Fprint(os.Stderr, strings.Repeat("文", 500))
		if mode == "cap_success" {
			return
		}
	}
	if mode == "sensitive" {
		fmt.Fprint(os.Stderr, "API_KEY=\""+strings.Repeat("fixture-private-value-", 40)+"\"\nSTDERR_DIAGNOSTIC_TAIL\n")
	}
	if mode == "cancel" {
		time.Sleep(30 * time.Second)
	}
	os.Exit(7)
}
