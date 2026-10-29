package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fxbin/cairn/internal/ports"
)

// setupSharedScenario 准备共享目录方案的实验环境：临时仓库的主工作区就是共
// 享目录，所有分支版本都指向它。
func setupSharedScenario(t *testing.T) scenarioEnv {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "shared")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("create shared workspace: %v", err)
	}
	seedRepository(t, repo)
	backend, err := newSharedBackend(repo)
	if err != nil {
		t.Fatalf("create shared backend: %v", err)
	}
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	base, err := backend.Prepare(ctx, ownerRequest("shared-base"))
	if err != nil {
		t.Fatalf("prepare shared base version: %v", err)
	}
	return scenarioEnv{
		backend:      backend,
		target:       base.Version,
		isolated:     base.Isolated,
		metadataRoot: filepath.Join(repo, ".git"),
		cleanup:      func() {},
	}
}

// setupWorktreeScenario 准备 Git worktree 方案的实验环境：临时仓库提供共享
// 对象库，每个分支版本在同级目录下拥有独立检出目录。
func setupWorktreeScenario(t *testing.T) scenarioEnv {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	seedRepository(t, repo)
	backend, err := newGitWorktreeBackend(repo, filepath.Join(root, "worktrees"))
	if err != nil {
		t.Fatalf("create worktree backend: %v", err)
	}
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	base, err := backend.Prepare(ctx, ownerRequest("worktree-base"))
	if err != nil {
		t.Fatalf("prepare worktree base version: %v", err)
	}
	return scenarioEnv{
		backend:      backend,
		target:       base.Version,
		isolated:     base.Isolated,
		metadataRoot: filepath.Join(repo, ".git"),
		cleanup:      func() {},
	}
}

// sharedBackendKind 是共享目录方案的种类标识。
const sharedBackendKind = ports.WorkspaceBackendSharedDirectory

// worktreeBackendKind 是 Git worktree 方案的种类标识。
const worktreeBackendKind = ports.WorkspaceBackendGitWorktree
