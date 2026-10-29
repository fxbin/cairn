package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// sharedVersion 是共享目录后端的一个版本视图。
type sharedVersion struct {
	// id 是版本标识。
	id string
	// owner 是版本所有者。
	owner ports.WorkspaceOwner
	// permission 是该版本实际获得的权限面。
	permission ports.WorkspacePermission
	// baseDigest 是分叉时基线内容的摘要。
	baseDigest ports.WorkspaceDigest
	// writes 记录该版本写入的工作区相对路径。共享目录本身无法区分写入
	// 来源，这份归因对应真实运行里事件记录的补丁事实。
	writes map[string]struct{}
}

// portable 返回该版本的可移植标识。
func (version *sharedVersion) portable() ports.WorkspaceVersion {
	return ports.WorkspaceVersion{Backend: ports.WorkspaceBackendSharedDirectory, ID: version.id}
}

// sharedBackend 是共享目录后端的对照实现：所有版本指向同一物理目录，分叉
// 只保留基线快照，写入互相可见。它复现现行单写入者行为，用来对照隔离后端
// 在并行分支、冲突检测与失败恢复上的差异。
type sharedBackend struct {
	// directory 是共享的物理工作区目录。
	directory string
	// mu 保护并发访问，实验里的并行分支会同时调用后端。
	mu sync.Mutex
	// sequence 生成版本标识。
	sequence int
	// versions 保存已知版本。
	versions map[string]*sharedVersion
	// snapshots 按内容摘要缓存基线正文，供差异比较与失败恢复复用。
	snapshots map[string]map[string][]byte
}

// newSharedBackend 创建共享目录后端，root 必须已存在且可读写。
func newSharedBackend(root string) (*sharedBackend, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat shared workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("shared workspace is not a directory")
	}
	return &sharedBackend{
		directory: root,
		versions:  make(map[string]*sharedVersion),
		snapshots: make(map[string]map[string][]byte),
	}, nil
}

// Prepare 在共享目录上准备一个新版本视图；所有版本共享同一物理目录。
func (backend *sharedBackend) Prepare(ctx context.Context, request ports.WorkspaceRequest) (ports.WorkspaceState, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	digest, _, _, err := contentDigest(backend.directory)
	if err != nil {
		return ports.WorkspaceState{}, fmt.Errorf("observe shared workspace: %w", err)
	}
	if _, err := backend.storeSnapshot(digest); err != nil {
		return ports.WorkspaceState{}, err
	}
	backend.sequence++
	version := &sharedVersion{
		id:         fmt.Sprintf("shared-%d", backend.sequence),
		owner:      request.Owner,
		permission: request.Permission,
		baseDigest: digest,
		writes:     make(map[string]struct{}),
	}
	backend.versions[version.id] = version
	return ports.WorkspaceState{
		Version:    version.portable(),
		Digest:     digest,
		Owner:      request.Owner,
		Permission: request.Permission,
		Isolated:   false,
	}, nil
}

// Fork 从共享目录派生一个新版本：物理目录不变，只记录分叉时刻的基线快照。
func (backend *sharedBackend) Fork(ctx context.Context, base ports.WorkspaceVersion, request ports.WorkspaceRequest) (ports.WorkspaceState, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if _, exists := backend.versions[base.ID]; !exists {
		return ports.WorkspaceState{}, ports.ErrWorkspaceVersionUnknown
	}
	digest, _, _, err := contentDigest(backend.directory)
	if err != nil {
		return ports.WorkspaceState{}, fmt.Errorf("observe shared workspace: %w", err)
	}
	if _, err := backend.storeSnapshot(digest); err != nil {
		return ports.WorkspaceState{}, err
	}
	backend.sequence++
	version := &sharedVersion{
		id:         fmt.Sprintf("shared-%d", backend.sequence),
		owner:      request.Owner,
		permission: request.Permission,
		baseDigest: digest,
		writes:     make(map[string]struct{}),
	}
	backend.versions[version.id] = version
	return ports.WorkspaceState{
		Version:    version.portable(),
		Digest:     digest,
		Owner:      request.Owner,
		Permission: request.Permission,
		Isolated:   false,
	}, nil
}

// Observe 返回共享目录当前的内容摘要与该视图的所有者、权限。
func (backend *sharedBackend) Observe(ctx context.Context, version ports.WorkspaceVersion) (ports.WorkspaceState, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[version.ID]
	if !exists {
		return ports.WorkspaceState{}, ports.ErrWorkspaceVersionUnknown
	}
	digest, _, _, err := contentDigest(backend.directory)
	if err != nil {
		return ports.WorkspaceState{}, fmt.Errorf("observe shared workspace: %w", err)
	}
	return ports.WorkspaceState{
		Version:    stored.portable(),
		Digest:     digest,
		Owner:      stored.owner,
		Permission: stored.permission,
		Isolated:   false,
	}, nil
}

// Diff 比较共享目录相对基线快照的变化路径；共享目录没有可合并的分支基线，
// 因此冲突路径恒为空。
func (backend *sharedBackend) Diff(ctx context.Context, base ports.WorkspaceVersion, head ports.WorkspaceVersion) (ports.WorkspaceDiff, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	baseVersion, exists := backend.versions[base.ID]
	if !exists {
		return ports.WorkspaceDiff{}, ports.ErrWorkspaceVersionUnknown
	}
	if _, exists := backend.versions[head.ID]; !exists {
		return ports.WorkspaceDiff{}, ports.ErrWorkspaceVersionUnknown
	}
	baseSnapshot, err := backend.snapshot(baseVersion.baseDigest)
	if err != nil {
		return ports.WorkspaceDiff{}, err
	}
	_, current, _, err := contentDigest(backend.directory)
	if err != nil {
		return ports.WorkspaceDiff{}, fmt.Errorf("observe shared workspace: %w", err)
	}
	digest, _, _, err := contentDigest(backend.directory)
	if err != nil {
		return ports.WorkspaceDiff{}, fmt.Errorf("observe shared workspace: %w", err)
	}
	return ports.WorkspaceDiff{
		Base:            baseVersion.portable(),
		Head:            backend.versions[head.ID].portable(),
		BaseDigest:      baseVersion.baseDigest,
		HeadDigest:      digest,
		ChangedPaths:    diffHashes(hashesFromSnapshot(baseSnapshot), current),
		ConflictedPaths: nil,
		Comparable:      true,
	}, nil
}

// Discard 丢弃版本并把共享目录恢复到分叉时的基线内容；其他分支的并发改动
// 会被一并覆盖，这正是共享目录方案无法并行写下的直接后果。
func (backend *sharedBackend) Discard(ctx context.Context, version ports.WorkspaceVersion) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	stored, exists := backend.versions[version.ID]
	if !exists {
		return ports.ErrWorkspaceVersionUnknown
	}
	baseline, err := backend.snapshot(stored.baseDigest)
	if err != nil {
		return err
	}
	current, err := snapshotFiles(backend.directory)
	if err != nil {
		return fmt.Errorf("snapshot shared workspace: %w", err)
	}
	for path := range current {
		if _, exists := baseline[path]; exists {
			continue
		}
		full := filepath.Join(backend.directory, filepath.FromSlash(path))
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	for path, content := range baseline {
		existing, exists := current[path]
		if exists && string(existing) == string(content) {
			continue
		}
		full := filepath.Join(backend.directory, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create directory for %s: %w", path, err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return fmt.Errorf("restore %s: %w", path, err)
		}
	}
	delete(backend.versions, version.ID)
	return nil
}

// Publish 把源版本的变更并入目标版本。共享目录的源与目标是同一物理目录，
// 内容早已就位，因此发布只做三件事：校验授权与验证证据、校验目标摘要、
// 用写入归因确认没有别的分支在同一路径上写过内容。
func (backend *sharedBackend) Publish(ctx context.Context, request ports.WorkspacePublishRequest) (ports.WorkspacePublishResult, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	source, exists := backend.versions[request.Source.ID]
	if !exists {
		return ports.WorkspacePublishResult{}, ports.ErrWorkspaceVersionUnknown
	}
	if _, exists := backend.versions[request.Target.ID]; !exists {
		return ports.WorkspacePublishResult{}, ports.ErrWorkspaceVersionUnknown
	}
	if !request.Authorized {
		return ports.WorkspacePublishResult{}, ports.ErrWorkspaceNotAuthorized
	}
	if !request.VerifierPassed {
		return ports.WorkspacePublishResult{}, ports.ErrWorkspaceVerificationMissing
	}
	digest, current, _, err := contentDigest(backend.directory)
	if err != nil {
		return ports.WorkspacePublishResult{}, fmt.Errorf("observe shared workspace: %w", err)
	}
	if request.ExpectedTargetDigest.IsZero() || request.ExpectedTargetDigest != digest {
		return ports.WorkspacePublishResult{}, &ports.WorkspaceDriftError{Expected: request.ExpectedTargetDigest, Actual: digest}
	}
	baseSnapshot, err := backend.snapshot(source.baseDigest)
	if err != nil {
		return ports.WorkspacePublishResult{}, err
	}
	changed := diffHashes(hashesFromSnapshot(baseSnapshot), current)
	own := make([]string, 0)
	conflicts := make([]string, 0)
	unattributed := make([]string, 0)
	for _, path := range changed {
		writtenBySource := containsPath(source.writes, path)
		writtenByOther := false
		for _, other := range backend.versions {
			if other.id == source.id {
				continue
			}
			if containsPath(other.writes, path) {
				writtenByOther = true
				break
			}
		}
		switch {
		case writtenBySource && writtenByOther:
			conflicts = append(conflicts, path)
		case writtenBySource:
			own = append(own, path)
		case writtenByOther:
			// 其他分支的变更不属于本次发布，但也不是漂移。
		default:
			unattributed = append(unattributed, path)
		}
	}
	if len(unattributed) > 0 {
		return ports.WorkspacePublishResult{}, &ports.WorkspaceDriftError{Expected: request.ExpectedTargetDigest, Actual: digest}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return ports.WorkspacePublishResult{}, &ports.WorkspaceConflictError{Paths: conflicts}
	}
	leaked, err := scanSecrets(backend.directory, own)
	if err != nil {
		return ports.WorkspacePublishResult{}, err
	}
	if len(leaked) > 0 {
		return ports.WorkspacePublishResult{}, &ports.WorkspaceSecretLeakError{Paths: leaked}
	}
	return ports.WorkspacePublishResult{Published: request.Target, Digest: digest, ChangedPaths: own}, nil
}

// root 返回共享目录的物理路径；共享目录下所有版本的根相同。
func (backend *sharedBackend) root(version ports.WorkspaceVersion) (string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if _, exists := backend.versions[version.ID]; !exists {
		return "", ports.ErrWorkspaceVersionUnknown
	}
	return backend.directory, nil
}

// recordWrite 记录版本写入的路径，供发布时的变更归因使用。
func (backend *sharedBackend) recordWrite(version ports.WorkspaceVersion, paths ...string) error {
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

// commit 返回不支持：共享目录没有分支级版本单元，无法把某个分支的改动固化
// 成独立可审阅的提交。
func (backend *sharedBackend) commit(version ports.WorkspaceVersion) error {
	return fmt.Errorf("%w: shared directory has no per-branch commit unit", ports.ErrWorkspaceUnsupported)
}

// deliveryUnit 产出共享目录方案的唯一可用交付单元：整棵目录的归档。它没有
// 版本号、没有差异、没有冲突检测，只能整体覆盖。
func (backend *sharedBackend) deliveryUnit(version ports.WorkspaceVersion) (string, []byte, time.Duration, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if _, exists := backend.versions[version.ID]; !exists {
		return "", nil, 0, ports.ErrWorkspaceVersionUnknown
	}
	snapshot, err := snapshotFiles(backend.directory)
	if err != nil {
		return "", nil, 0, fmt.Errorf("snapshot shared workspace: %w", err)
	}
	paths := make([]string, 0, len(snapshot))
	for path := range snapshot {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	started := time.Now()
	content, err := tarSnapshot(backend.directory, paths)
	if err != nil {
		return "", nil, 0, err
	}
	return "tar_snapshot", content, time.Since(started), nil
}

// physicalRoots 返回共享目录方案唯一的物理目录。
func (backend *sharedBackend) physicalRoots() ([]string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return []string{backend.directory}, nil
}

// storeSnapshot 缓存当前目录内容快照，并返回对应摘要。
func (backend *sharedBackend) storeSnapshot(digest ports.WorkspaceDigest) (map[string][]byte, error) {
	if snapshot, exists := backend.snapshots[string(digest)]; exists {
		return snapshot, nil
	}
	snapshot, err := snapshotFiles(backend.directory)
	if err != nil {
		return nil, fmt.Errorf("snapshot shared workspace: %w", err)
	}
	backend.snapshots[string(digest)] = snapshot
	return snapshot, nil
}

// snapshot 返回指定摘要对应的基线快照。
func (backend *sharedBackend) snapshot(digest ports.WorkspaceDigest) (map[string][]byte, error) {
	snapshot, exists := backend.snapshots[string(digest)]
	if !exists {
		return nil, fmt.Errorf("%w: baseline snapshot %s is missing", ports.ErrWorkspaceVersionUnknown, digest)
	}
	return snapshot, nil
}

// containsPath 判断路径集合是否包含给定路径。
func containsPath(paths map[string]struct{}, path string) bool {
	_, exists := paths[filepath.ToSlash(path)]
	return exists
}
