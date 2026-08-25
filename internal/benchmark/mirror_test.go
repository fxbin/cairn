package benchmark

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMirrorCloneIsLocalFirstAfterWarmup 锁定本地优先语义：镜像已含基线时，
// 把镜像目录置为只读（任何 fetch 都会失败）后克隆仍成功——证明不再发起
// 网络请求；工作区为全新目录且检出不可变基线。
func TestMirrorCloneIsLocalFirstAfterWarmup(t *testing.T) {
	source, baseline := localGitRepository(t)
	task := localMirrorTask(source, baseline)
	root := t.TempDir()
	first := filepath.Join(root, "attempt-01")
	if err := mirrorCloneAtBaseline(context.Background(), task, first, "", root); err != nil {
		t.Fatalf("warmup clone: %v", err)
	}
	if head := headRevision(t, first); head != baseline {
		t.Fatalf("warmup HEAD=%s want %s", head, baseline)
	}
	mirror := mirrorDirectory(task, root)
	info, err := os.Stat(mirror)
	if err != nil || !info.IsDir() {
		t.Fatalf("mirror missing: %v", err)
	}
	if err := os.Chmod(mirror, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(mirror, 0o755)
	second := filepath.Join(root, "attempt-02")
	started := time.Now()
	if err := mirrorCloneAtBaseline(context.Background(), task, second, "", root); err != nil {
		t.Fatalf("local-first clone must not fetch: %v", err)
	}
	if head := headRevision(t, second); head != baseline {
		t.Fatalf("second HEAD=%s want %s", head, baseline)
	}
	if time.Since(started) > 30*time.Second {
		t.Fatalf("local clone took %s; it must be near-instant", time.Since(started))
	}
}

// TestMirrorFetchesOnlyWhenBaselineMissing 锁定缺则远程语义：镜像缺新基线时
// 经 origin 增量 fetch 补齐，且只补对象不重建镜像；已含的旧基线仍走本地。
func TestMirrorFetchesOnlyWhenBaselineMissing(t *testing.T) {
	source, baseline := localGitRepository(t)
	task := localMirrorTask(source, baseline)
	root := t.TempDir()
	if err := mirrorCloneAtBaseline(context.Background(), task, filepath.Join(root, "attempt-01"), "", root); err != nil {
		t.Fatal(err)
	}
	// 在来源仓库追加第二个提交，任务基线指向它：镜像缺失 → 走 fetch 补齐。
	if err := os.WriteFile(filepath.Join(source, "note.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "note.txt")
	runGit(t, source, "commit", "-m", "second")
	second := headRevision(t, source)
	advanced := task
	advanced.Repository.BaselineCommit = second
	if err := mirrorCloneAtBaseline(context.Background(), advanced, filepath.Join(root, "attempt-02"), "", root); err != nil {
		t.Fatalf("fetch-on-missing clone: %v", err)
	}
	if head := headRevision(t, filepath.Join(root, "attempt-02")); head != second {
		t.Fatalf("fetched HEAD=%s want %s", head, second)
	}
	if head := headRevision(t, mirrorDirectory(task, root)); head != second {
		t.Fatalf("mirror HEAD=%s want %s after fetch", head, second)
	}
}

// TestMirrorCloneRefusesBaselineMissingFromRemote 锁定失败关闭：镜像与远程都
// 没有任务声明的基线时，准备失败而不是检出错误内容。
func TestMirrorCloneRefusesBaselineMissingFromRemote(t *testing.T) {
	source, baseline := localGitRepository(t)
	task := localMirrorTask(source, baseline)
	task.Repository.BaselineCommit = strings.Repeat("0", 40)
	if err := mirrorCloneAtBaseline(context.Background(), task, filepath.Join(t.TempDir(), "ws"), "", t.TempDir()); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("missing baseline must fail closed: %v", err)
	}
}

// localMirrorTask 直接以本地仓库路径构造任务；不经过 testPack（其隐私
// 改写会把本地路径替换成 example.invalid，不适合镜像行为测试）。
func localMirrorTask(source, baseline string) Task {
	return Task{
		ID: "LOCAL-001", Kind: "natural",
		Repository:   Repository{Name: "local", URL: source, BaselineCommit: baseline},
		Verification: Verification{Command: Command{Argv: []string{"true"}, CWD: "."}},
		Runtime:      Runtime{DockerImage: "alpine:latest"},
	}
}

// headRevision 返回指定工作区/镜像当前 HEAD 的完整提交。
func headRevision(t *testing.T, directory string) string {
	t.Helper()
	output, err := runProcessOutput(context.Background(), directory, []string{"git", "rev-parse", "HEAD"})
	if err != nil {
		t.Fatalf("rev-parse %s: %v: %s", directory, err, output)
	}
	return strings.TrimSpace(string(output))
}
