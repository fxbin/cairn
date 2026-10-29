package workspace_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// gitVersion 是 Git worktree 后端的一个版本：一个独立检出目录加一条分支。
type gitVersion struct {
	// id 是版本标识。
	id string
	// branch 是该版本在仓库里的分支名。
	branch string
	// path 是该版本的检出目录。
	path string
	// owner 是版本所有者。
	owner ports.WorkspaceOwner
	// permission 是该版本实际获得的权限面。
	permission ports.WorkspacePermission
	// baseDigest 是分叉时基线内容的摘要。
	baseDigest ports.WorkspaceDigest
	// baseHashes 是分叉时基线的路径内容摘要表。
	baseHashes map[string]string
	// writes 记录该版本写入的工作区相对路径。
	writes map[string]struct{}
}

// portable 返回该版本的可移植标识。
func (version *gitVersion) portable() ports.WorkspaceVersion {
	return ports.WorkspaceVersion{Backend: ports.WorkspaceBackendGitWorktree, ID: version.id}
}

// gitWorktreeBackend 是 Git worktree 后端的对照实现：每个版本拥有独立检出
// 目录并共享同一对象库，发布等价于把源分支合并回目标分支。
type gitWorktreeBackend struct {
	// repo 是 Git 仓库路径。
	repo string
	// worktreeRoot 是检出目录的父目录。
	worktreeRoot string
	// mu 保护并发访问。
	mu sync.Mutex
	// sequence 生成版本标识与分支名。
	sequence int
	// versions 保存已知版本。
	versions map[string]*gitVersion
}

// newGitWorktreeBackend 创建 Git worktree 后端；repo 必须是一个已有提交的
// 仓库，worktreeRoot 用于放置检出目录。
func newGitWorktreeBackend(repo string, worktreeRoot string) (*gitWorktreeBackend, error) {
	if _, err := gitOutput(repo, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("locate git repository: %w", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create worktree root: %w", err)
	}
	return &gitWorktreeBackend{
		repo:         repo,
		worktreeRoot: worktreeRoot,
		versions:     make(map[string]*gitVersion),
	}, nil
}

// Prepare 基于当前提交创建一个新的隔离检出目录。
func (backend *gitWorktreeBackend) Prepare(ctx context.Context, request ports.WorkspaceRequest) (ports.WorkspaceState, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	head, err := gitOutput(backend.repo, "rev-parse", "HEAD")
	if err != nil {
		return ports.WorkspaceState{}, fmt.Errorf("resolve head: %w", err)
	}
	return backend.createLocked(strings.TrimSpace(head), request)
}

// Fork 从基线版本的当前提交派生一个新的隔离检出目录。
func (backend *gitWorktreeBackend) Fork(ctx context.Context, base ports.WorkspaceVersion, request ports.WorkspaceRequest) (ports.WorkspaceState, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[base.ID]
	if !exists {
		return ports.WorkspaceState{}, ports.ErrWorkspaceVersionUnknown
	}
	commit, err := gitOutput(stored.path, "rev-parse", "HEAD")
	if err != nil {
		return ports.WorkspaceState{}, fmt.Errorf("resolve base head: %w", err)
	}
	return backend.createLocked(strings.TrimSpace(commit), request)
}

// createLocked 在锁内创建分支、检出目录与版本记录。
func (backend *gitWorktreeBackend) createLocked(commit string, request ports.WorkspaceRequest) (ports.WorkspaceState, error) {
	backend.sequence++
	id := fmt.Sprintf("wt-%d", backend.sequence)
	branch := fmt.Sprintf("cairn/spike/%d", backend.sequence)
	path := filepath.Join(backend.worktreeRoot, id)
	if _, err := gitOutput(backend.repo, "worktree", "add", "-b", branch, path, commit); err != nil {
		return ports.WorkspaceState{}, fmt.Errorf("create worktree: %w", err)
	}
	digest, hashes, _, err := contentDigest(path)
	if err != nil {
		return ports.WorkspaceState{}, fmt.Errorf("observe worktree: %w", err)
	}
	version := &gitVersion{
		id:         id,
		branch:     branch,
		path:       path,
		owner:      request.Owner,
		permission: request.Permission,
		baseDigest: digest,
		baseHashes: hashes,
		writes:     make(map[string]struct{}),
	}
	backend.versions[version.id] = version
	return ports.WorkspaceState{
		Version:    version.portable(),
		Digest:     digest,
		Owner:      request.Owner,
		Permission: request.Permission,
		Isolated:   true,
	}, nil
}

// Observe 返回检出目录当前的内容摘要与该版本的所有者、权限。
func (backend *gitWorktreeBackend) Observe(ctx context.Context, version ports.WorkspaceVersion) (ports.WorkspaceState, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[version.ID]
	if !exists {
		return ports.WorkspaceState{}, ports.ErrWorkspaceVersionUnknown
	}
	digest, _, _, err := contentDigest(stored.path)
	if err != nil {
		return ports.WorkspaceState{}, fmt.Errorf("observe worktree: %w", err)
	}
	return ports.WorkspaceState{
		Version:    stored.portable(),
		Digest:     digest,
		Owner:      stored.owner,
		Permission: stored.permission,
		Isolated:   true,
	}, nil
}

// Diff 比较基线版本与目标版本检出目录的内容差异。
func (backend *gitWorktreeBackend) Diff(ctx context.Context, base ports.WorkspaceVersion, head ports.WorkspaceVersion) (ports.WorkspaceDiff, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	baseVersion, exists := backend.versions[base.ID]
	if !exists {
		return ports.WorkspaceDiff{}, ports.ErrWorkspaceVersionUnknown
	}
	headVersion, exists := backend.versions[head.ID]
	if !exists {
		return ports.WorkspaceDiff{}, ports.ErrWorkspaceVersionUnknown
	}
	digest, current, _, err := contentDigest(headVersion.path)
	if err != nil {
		return ports.WorkspaceDiff{}, fmt.Errorf("observe worktree: %w", err)
	}
	return ports.WorkspaceDiff{
		Base:            baseVersion.portable(),
		Head:            headVersion.portable(),
		BaseDigest:      baseVersion.baseDigest,
		HeadDigest:      digest,
		ChangedPaths:    diffHashes(baseVersion.baseHashes, current),
		ConflictedPaths: nil,
		Comparable:      true,
	}, nil
}

// Discard 丢弃版本：移除检出目录与分支，基线版本不受影响。
func (backend *gitWorktreeBackend) Discard(ctx context.Context, version ports.WorkspaceVersion) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[version.ID]
	if !exists {
		return ports.ErrWorkspaceVersionUnknown
	}
	if _, err := gitOutput(backend.repo, "worktree", "remove", "--force", stored.path); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	if _, err := gitOutput(backend.repo, "branch", "-D", stored.branch); err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}
	if _, err := gitOutput(backend.repo, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune worktrees: %w", err)
	}
	delete(backend.versions, version.ID)
	return nil
}

// Publish 把源分支合并回目标分支。合并前依次校验授权、验证证据、目标摘要
// 与疑似机密；合并冲突会中止合并并显式失败，绝不留下覆盖结果。
func (backend *gitWorktreeBackend) Publish(ctx context.Context, request ports.WorkspacePublishRequest) (ports.WorkspacePublishResult, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	source, exists := backend.versions[request.Source.ID]
	if !exists {
		return ports.WorkspacePublishResult{}, ports.ErrWorkspaceVersionUnknown
	}
	target, exists := backend.versions[request.Target.ID]
	if !exists {
		return ports.WorkspacePublishResult{}, ports.ErrWorkspaceVersionUnknown
	}
	if !request.Authorized {
		return ports.WorkspacePublishResult{}, ports.ErrWorkspaceNotAuthorized
	}
	if !request.VerifierPassed {
		return ports.WorkspacePublishResult{}, ports.ErrWorkspaceVerificationMissing
	}
	digest, _, _, err := contentDigest(target.path)
	if err != nil {
		return ports.WorkspacePublishResult{}, fmt.Errorf("observe target worktree: %w", err)
	}
	if request.ExpectedTargetDigest.IsZero() || request.ExpectedTargetDigest != digest {
		return ports.WorkspacePublishResult{}, &ports.WorkspaceDriftError{Expected: request.ExpectedTargetDigest, Actual: digest}
	}
	_, sourceCurrent, _, err := contentDigest(source.path)
	if err != nil {
		return ports.WorkspacePublishResult{}, fmt.Errorf("observe source worktree: %w", err)
	}
	changed := diffHashes(source.baseHashes, sourceCurrent)
	leaked, err := scanSecrets(source.path, changed)
	if err != nil {
		return ports.WorkspacePublishResult{}, err
	}
	if len(leaked) > 0 {
		return ports.WorkspacePublishResult{}, &ports.WorkspaceSecretLeakError{Paths: leaked}
	}
	status, err := gitOutput(source.path, "status", "--porcelain")
	if err != nil {
		return ports.WorkspacePublishResult{}, fmt.Errorf("inspect source worktree: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return ports.WorkspacePublishResult{}, fmt.Errorf("%w: source branch has uncommitted changes", ports.ErrWorkspaceDirty)
	}
	if _, err := gitOutput(target.path, "merge", "--no-ff", "--no-edit", source.branch); err != nil {
		conflicts, conflictErr := gitConflictPaths(target.path)
		if conflictErr == nil && len(conflicts) > 0 {
			if _, abortErr := gitOutput(target.path, "merge", "--abort"); abortErr != nil {
				return ports.WorkspacePublishResult{}, fmt.Errorf("abort conflicted merge: %w", abortErr)
			}
			return ports.WorkspacePublishResult{}, &ports.WorkspaceConflictError{Paths: conflicts}
		}
		return ports.WorkspacePublishResult{}, fmt.Errorf("merge source branch: %w", err)
	}
	merged, _, _, err := contentDigest(target.path)
	if err != nil {
		return ports.WorkspacePublishResult{}, fmt.Errorf("observe merged worktree: %w", err)
	}
	target.baseDigest = merged
	target.baseHashes = hashesFromSnapshot(mustSnapshot(target.path))
	return ports.WorkspacePublishResult{Published: target.portable(), Digest: merged, ChangedPaths: changed}, nil
}

// root 返回版本的检出目录。
func (backend *gitWorktreeBackend) root(version ports.WorkspaceVersion) (string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[version.ID]
	if !exists {
		return "", ports.ErrWorkspaceVersionUnknown
	}
	return stored.path, nil
}

// recordWrite 记录版本写入的路径。
func (backend *gitWorktreeBackend) recordWrite(version ports.WorkspaceVersion, paths ...string) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[version.ID]
	if !exists {
		return ports.ErrWorkspaceVersionUnknown
	}
	for _, path := range paths {
		stored.writes[filepath.ToSlash(path)] = struct{}{}
	}
	return nil
}

// commit 把检出目录的变更固化为分支提交，形成可审阅的交付单元。
func (backend *gitWorktreeBackend) commit(version ports.WorkspaceVersion) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[version.ID]
	if !exists {
		return ports.ErrWorkspaceVersionUnknown
	}
	status, err := gitOutput(stored.path, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("inspect worktree: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return nil
	}
	if _, err := gitOutput(stored.path, "add", "-A"); err != nil {
		return fmt.Errorf("stage changes: %w", err)
	}
	if _, err := gitOutput(stored.path, "commit", "-m", "spike branch"); err != nil {
		return fmt.Errorf("commit changes: %w", err)
	}
	return nil
}

// physicalRoots 返回各版本独立检出目录，去重后用于磁盘统计。
func (backend *gitWorktreeBackend) physicalRoots() ([]string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	roots := make([]string, 0, len(backend.versions))
	seen := make(map[string]struct{}, len(backend.versions))
	for _, version := range backend.versions {
		if _, exists := seen[version.path]; exists {
			continue
		}
		seen[version.path] = struct{}{}
		roots = append(roots, version.path)
	}
	sort.Strings(roots)
	return roots, nil
}

// deliveryUnit 产出 worktree 方案的交付单元：分支相对基线的补丁文件，可直接
// 进入审阅与提交流程。
func (backend *gitWorktreeBackend) deliveryUnit(version ports.WorkspaceVersion) (string, []byte, time.Duration, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[version.ID]
	if !exists {
		return "", nil, 0, ports.ErrWorkspaceVersionUnknown
	}
	started := time.Now()
	output, err := gitOutput(backend.repo, "format-patch", "-1", "--stdout", stored.branch)
	if err != nil {
		return "", nil, 0, fmt.Errorf("format patch: %w", err)
	}
	return "git_patch", []byte(output), time.Since(started), nil
}

// gitConflictPaths 返回未合并的冲突路径。
func gitConflictPaths(worktree string) ([]string, error) {
	output, err := gitOutput(worktree, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("list conflicted paths: %w", err)
	}
	paths := make([]string, 0)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, filepath.ToSlash(line))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// mustSnapshot 读取目录快照；失败时返回空快照，仅用于刷新基线摘要表。
func mustSnapshot(root string) map[string][]byte {
	snapshot, err := snapshotFiles(root)
	if err != nil {
		return map[string][]byte{}
	}
	return snapshot
}
