package interop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCairnFingerprint 是假 cairn 报告的审批绑定指纹，形状与真实 Cairn
// 的 `sha256:<64 位小写十六进制>` 一致。
const fakeCairnFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// fakeCairnScript 是假 cairn 可执行脚本：第一次调用停在待审批态并以退出码
// 6 结束，第二次调用（resume）放行后完成。它让 Cairn 侧外壳的驱动逻辑
// ——审批往返、指纹提取、事件与用量解析、运行标识提取——可以在不编译
// Go 二进制、不拉起外部 CLI 的前提下被秒级验证。
const fakeCairnScript = `#!/bin/sh
count_file="$CAIRN_FAKE_COUNT"
run_id="run_fake_0001"
count=0
if [ -f "$count_file" ]; then count=$(cat "$count_file"); fi
count=$((count + 1))
echo "$count" > "$count_file"

if [ "$1" = "run" ]; then
  printf '{"type":"event","event":{"type":"run_created","run_id":"%s"}}\n' "$run_id"
  printf '{"type":"event","event":{"type":"model_response_finished","run_id":"%s","payload":{"usage":{"prompt_tokens":1000,"completion_tokens":120,"total_tokens":1120}}}}\n' "$run_id"
  echo "run failed: approval required for op_fake_0001 (` + fakeCairnFingerprint + `)" >&2
  exit 6
fi

printf '{"type":"event","event":{"type":"approval_granted","run_id":"%s"}}\n' "$run_id"
printf '{"type":"event","event":{"type":"run_completed","run_id":"%s"}}\n' "$run_id"
printf '{"type":"result","run_id":"%s","response":"ok"}\n' "$run_id"
exit 0
`

// truncate 截断长文本，供失败信息使用。
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// lastResultLine 返回输出中最后一条 result 事件行，供失败诊断使用。
func lastResultLine(stdout string) string {
	lines := strings.Split(stdout, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if strings.Contains(line, `"type":"result"`) {
			return line
		}
	}
	return "(无 result 行)"
}

// writeFakeCairn 写出假 cairn 可执行文件并返回其路径。
func writeFakeCairn(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "cairn-fake")
	if err := os.WriteFile(path, []byte(fakeCairnScript), 0o755); err != nil {
		t.Fatalf("write fake cairn: %v", err)
	}
	return path
}

// requireCLI 在外壳可执行文件缺失时跳过整段对照实验：外部 CLI 不在机器上
// 时，实验结论不可得，而不是"失败"。
func requireCLI(t *testing.T, name string, command string) {
	t.Helper()
	if _, err := lookPath(command); err != nil {
		t.Skipf("外部 CLI %s 不可用（%v），对照实验不可得", name, err)
	}
}

// lookPath 在 PATH 中查找可执行文件。
func lookPath(name string) (string, error) {
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

// TestProcessRunnerCapturesExitAndStreams 锁定受控进程执行器的基本契约：
// 退出码、stdout、stderr 都被如实捕获。
func TestProcessRunnerCapturesExitAndStreams(t *testing.T) {
	outcome, err := runProcess(context.Background(), HarnessRun{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo out; echo err >&2; exit 7"},
		Dir:     t.TempDir(),
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("run process: %v", err)
	}
	if outcome.ExitCode != 7 {
		t.Fatalf("退出码 = %d，期望 7", outcome.ExitCode)
	}
	if !strings.Contains(outcome.Stdout, "out") {
		t.Fatalf("stdout 未捕获: %q", outcome.Stdout)
	}
	if !strings.Contains(outcome.Stderr, "err") {
		t.Fatalf("stderr 未捕获: %q", outcome.Stderr)
	}
	if outcome.Interrupted {
		t.Fatal("未中断的运行不应标记为已中断")
	}
}

// TestProcessRunnerTimeoutKills 锁定超时语义：超时后进程被回收，运行被
// 标记为已中断。
func TestProcessRunnerTimeoutKills(t *testing.T) {
	outcome, err := runProcess(context.Background(), HarnessRun{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
		Dir:     t.TempDir(),
		Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("run process: %v", err)
	}
	if !outcome.Interrupted {
		t.Fatal("超时运行应被标记为已中断")
	}
	if outcome.Duration > 10*time.Second {
		t.Fatalf("超时回收耗时过长: %s", outcome.Duration)
	}
}

// TestWorkspaceSnapshotAndChangedFiles 锁定工作区快照与差异计算：新增、
// 修改、删除都要被识别，隐藏目录在快照根下被排除。
func TestWorkspaceSnapshotAndChangedFiles(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	before, err := SnapshotWorkspace(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("two"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Remove(filepath.Join(workspace, "a.txt")); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	after, err := SnapshotWorkspace(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	changed := ChangedFiles(before, after)
	if !containsString(changed, "ws/a.txt") || !containsString(changed, "ws/b.txt") {
		t.Fatalf("改动文件识别不完整: %v", changed)
	}
}

// TestCairnHarnessApprovalRoundTrip 锁定 Cairn 侧外壳的审批往返驱动：运行
// 停在待审批态后，外壳必须从错误面取回绑定指纹，发起 resume 放行，并把
// 往返次数记进补充观测。
func TestCairnHarnessApprovalRoundTrip(t *testing.T) {
	root := t.TempDir()
	harness := NewCairnHarness(writeFakeCairn(t, root), filepath.Join(root, "cairn"), "http://127.0.0.1:1")
	countFile := filepath.Join(root, "count")
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	result, err := harness.Run(context.Background(), HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task:    "任务",
		Env:     []string{"CAIRN_FAKE_COUNT=" + countFile},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("run cairn harness: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("退出码 = %d，期望 0；stderr=%s", result.ExitCode, result.Stderr)
	}
	if result.Extra["approval_round_trips"] != "1" {
		t.Fatalf("审批往返次数 = %q，期望 1", result.Extra["approval_round_trips"])
	}
	if result.SessionHandle != "run_fake_0001" {
		t.Fatalf("运行标识 = %q，期望 run_fake_0001", result.SessionHandle)
	}
	if !containsString(result.Events, "approval_granted") {
		t.Fatalf("事件序列缺少审批放行: %v", result.Events)
	}
	if !result.Usage.Reported || result.Usage.InputTokens != 1000 || result.Usage.OutputTokens != 120 {
		t.Fatalf("用量解析不符: %+v", result.Usage)
	}
	if result.Extra["approval_parse"] == "failed" {
		t.Fatalf("审批指纹提取失败: %s", result.Stderr)
	}
}

// TestCairnHarnessParsesApprovalFingerprint 锁定审批指纹的提取规则：必须
// 取回带 `sha256:` 前缀的完整指纹，取掉前缀会导致放行被拒。
func TestCairnHarnessParsesApprovalFingerprint(t *testing.T) {
	stderr := "run failed: approval required for op_123_abc (sha256:" + strings.Repeat("a", 64) + ")"
	fingerprint, runID, ok := parseApprovalRequest(stderr, "")
	if !ok {
		t.Fatal("应当从错误面取回指纹")
	}
	if fingerprint != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("指纹 = %q，期望保留 sha256: 前缀", fingerprint)
	}
	if runID != "" {
		t.Fatalf("没有事件流时运行标识应为空，实际 %q", runID)
	}
	if _, _, ok := parseApprovalRequest("与审批无关的错误", ""); ok {
		t.Fatal("无关错误不应被解析为审批请求")
	}
}

// TestCairnHarnessParsesEventStream 锁定 Cairn NDJSON 事件流的解析：事件
// 类型序列、用量累计与运行标识都来自同一条流。
func TestCairnHarnessParsesEventStream(t *testing.T) {
	stdout := `{"type":"event","event":{"type":"run_created","run_id":"run_1"}}
{"type":"event","event":{"type":"model_response_finished","run_id":"run_1","payload":{"usage":{"prompt_tokens":1000,"completion_tokens":120,"total_tokens":1120}}}}
{"type":"event","event":{"type":"model_response_finished","run_id":"run_1","payload":{"usage":{"prompt_tokens":1100,"completion_tokens":130,"total_tokens":1230}}}}
{"type":"result","run_id":"run_1","response":"ok"}
`
	events := parseCairnEvents(stdout)
	if len(events) != 4 {
		t.Fatalf("事件序列 = %v，期望 4 条", events)
	}
	if !containsString(events, "run_created") || !containsString(events, "result") {
		t.Fatalf("事件序列内容不符: %v", events)
	}
	usage := parseCairnUsage(stdout)
	if !usage.Reported || usage.InputTokens != 2100 || usage.OutputTokens != 250 || usage.TotalTokens != 2350 {
		t.Fatalf("用量解析不符: %+v", usage)
	}
	if parseCairnRunID(stdout) != "run_1" {
		t.Fatalf("运行标识 = %q", parseCairnRunID(stdout))
	}
}
