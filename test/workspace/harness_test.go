package workspace_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// 对照实验的有界规模与常量：四个分支、单次比较，控制在秒级。
const (
	// experimentBranchCount 是并行分支数量，与 Issue 约定的四分支一致。
	experimentBranchCount = 4
	// dependencyBlobBytes 是模拟依赖体积的文件大小。
	dependencyBlobBytes = 2 << 20
	// verifierTimeout 是单次验证命令的墙钟上限。
	verifierTimeout = 2 * time.Minute
	// mergeTimeout 是单次 Git 合并的墙钟上限。
	mergeTimeout = time.Minute
)

// sharedFilePath 是并行分支各自写入、用于测量分支间可见性的文件前缀。
const sharedFilePath = "branch"

// conflictFilePath 是冲突场景里两个分支同时改写的同一文件。
const conflictFilePath = "shared.txt"

// secretFilePath 是疑似机密场景里写入的文件。
const secretFilePath = "secrets.txt"

// fakeSecret 是用于触发机密规则的假密钥，只在临时仓库内出现。
const fakeSecret = "AKIAIOSFODNN7EXAMPLE"

// branchBackend 是对照实验里两种后端共用的实验面。它在 ports.WorkspaceBackend
// 之外多出写入归因、提交与物理目录三项能力，因为对照实验要模拟真实运行里
// 的补丁归因、版本化交付与文件访问。
type branchBackend interface {
	ports.WorkspaceBackend
	// root 返回版本的物理目录。
	root(version ports.WorkspaceVersion) (string, error)
	// recordWrite 记录某版本写入的工作区相对路径，供共享目录后端归因变更。
	recordWrite(version ports.WorkspaceVersion, paths ...string) error
	// commit 把版本当前的变更固化为可交付单元。
	commit(version ports.WorkspaceVersion) error
	// deliveryUnit 产出该版本的可审阅交付单元，返回单元类型、正文与耗时。
	deliveryUnit(version ports.WorkspaceVersion) (string, []byte, time.Duration, error)
	// physicalRoots 返回承载分支内容的物理目录。共享目录方案只有一个目录，
	// worktree 方案每个版本一个目录；磁盘统计按这些目录去重，避免把共享
	// 内容重复计入。
	physicalRoots() ([]string, error)
}

// branchHandle 是实验里单个分支的句柄：它把后端版本、物理目录与写入归因
// 绑在一起，使两种后端能走同一套实验步骤。
type branchHandle struct {
	backend branchBackend
	version ports.WorkspaceVersion
	root    string
}

// write 写入文件并记录归因路径。
func (handle *branchHandle) write(path string, content string) error {
	full := filepath.Join(handle.root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return handle.backend.recordWrite(handle.version, path)
}

// remove 删除文件并记录归因路径。
func (handle *branchHandle) remove(path string) error {
	full := filepath.Join(handle.root, filepath.FromSlash(path))
	if err := os.Remove(full); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return handle.backend.recordWrite(handle.version, path)
}

// branchHandleOf 为版本建立实验句柄。
func branchHandleOf(backend branchBackend, version ports.WorkspaceVersion) (*branchHandle, error) {
	root, err := backend.root(version)
	if err != nil {
		return nil, err
	}
	return &branchHandle{backend: backend, version: version, root: root}, nil
}

// ownerRequest 构造一次实验用的准备或分叉请求。
func ownerRequest(label string) ports.WorkspaceRequest {
	return ports.WorkspaceRequest{
		Owner:      ports.WorkspaceOwner{RunID: "run-" + label, Role: ports.WorkspaceRoleOwner},
		Permission: ports.WorkspacePermission{Write: true, Execute: true, Publish: true},
		Label:      label,
	}
}

// writeFile 写入文件内容，必要时创建父目录。
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// pseudoRandomBytes 用固定种子的 xorshift 生成确定性字节序列，用于模拟不可
// 压缩的依赖体积；同一实验重复运行得到同样的内容摘要。
func pseudoRandomBytes(size int) []byte {
	buffer := make([]byte, size)
	state := uint64(20261029)
	for index := range buffer {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		buffer[index] = byte(state)
	}
	return buffer
}

// seedRepository 在给定目录建立一个可离线构建的最小 Git 仓库：它包含一个只
// 依赖标准库的 Go 模块和一个模拟依赖体积的大文件，用于磁盘占用对照。
func seedRepository(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "go.mod"), []byte("module example.com/spike\n\ngo 1.21\n"))
	writeFile(t, filepath.Join(root, "main.go"), []byte("package main\n\nimport \"example.com/spike/pkg/core\"\n\nfunc main() {\n\t_ = core.Value()\n}\n"))
	writeFile(t, filepath.Join(root, "pkg", "core", "core.go"), []byte("package core\n\n// Value 返回固定值，供验证命令检查包可编译。\nfunc Value() int { return 1 }\n"))
	writeFile(t, filepath.Join(root, "deps", "blob.bin"), pseudoRandomBytes(dependencyBlobBytes))
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "spike@example.invalid")
	runGit(t, root, "config", "user.name", "Cairn Spike")
	runGit(t, root, "config", "commit.gpgsign", "false")
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", "seed")
}

// runGit 在目录内执行 Git 命令并返回标准输出。
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := gitOutput(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return output
}

// gitOutput 在目录内执行 Git 命令并返回标准输出；失败时返回含标准错误的错误。
func gitOutput(dir string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"HOME="+dir,
		"GIT_AUTHOR_NAME=Cairn Spike",
		"GIT_AUTHOR_EMAIL=spike@example.invalid",
		"GIT_COMMITTER_NAME=Cairn Spike",
		"GIT_COMMITTER_EMAIL=spike@example.invalid",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// contentDigest 计算目录内容摘要、路径内容摘要表与内容字节数。算法对两种
// 后端一致：逐文件记录相对路径、权限位与内容 sha256，按路径排序后整体哈
// 希，并忽略 .git，使两种后端的摘要可直接比较。
func contentDigest(root string) (ports.WorkspaceDigest, map[string]string, int64, error) {
	entries := make([]string, 0)
	hashes := make(map[string]string)
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if relative == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			entries = append(entries, fmt.Sprintf("d\t%04o\t%s", info.Mode().Perm(), relative))
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entries = append(entries, fmt.Sprintf("l\t%04o\t%s\t%s", info.Mode().Perm(), relative, target))
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		hashes[relative] = hex.EncodeToString(digest[:])
		entries = append(entries, fmt.Sprintf("f\t%04o\t%s\t%s", info.Mode().Perm(), relative, hashes[relative]))
		total += int64(len(content))
		return nil
	})
	if err != nil {
		return "", nil, 0, err
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return ports.WorkspaceDigest("sha256:" + hex.EncodeToString(sum[:])), hashes, total, nil
}

// snapshotFiles 读取目录内的文件正文快照，供差异比较与失败恢复使用。
func snapshotFiles(root string) (map[string][]byte, error) {
	snapshot := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if relative == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[filepath.ToSlash(relative)] = content
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

// hashesFromSnapshot 由正文快照计算路径内容摘要表。
func hashesFromSnapshot(snapshot map[string][]byte) map[string]string {
	hashes := make(map[string]string, len(snapshot))
	for path, content := range snapshot {
		digest := sha256.Sum256(content)
		hashes[path] = hex.EncodeToString(digest[:])
	}
	return hashes
}

// diffHashes 返回相对基线发生变化的工作区相对路径：新增、删除与内容变化都
// 计入，路径按字典序排列。
func diffHashes(base map[string]string, current map[string]string) []string {
	changed := make([]string, 0)
	for path, digest := range current {
		if previous, exists := base[path]; !exists || previous != digest {
			changed = append(changed, path)
		}
	}
	for path := range base {
		if _, exists := current[path]; !exists {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

// secretRule 是疑似机密规则；命中即视为发布被拦截。
type secretRule struct {
	// name 是规则名，只进入报告，不含正文。
	name string
	// pattern 是命中规则的正则。
	pattern *regexp.Regexp
}

// secretRules 覆盖私钥块、云厂商访问密钥与通用令牌赋值三类形态。
var secretRules = []secretRule{
	{name: "private_key", pattern: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{name: "cloud_access_key", pattern: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{name: "assigned_token", pattern: regexp.MustCompile(`(?i)(api[_-]?key|secret[_-]?token|password)\s*[:=]\s*["']?[A-Za-z0-9/+_-]{16,}`)},
}

// scanSecrets 返回命中疑似机密规则的工作区相对路径，按字典序排列。
func scanSecrets(root string, paths []string) ([]string, error) {
	hit := make([]string, 0)
	for _, path := range paths {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		for _, rule := range secretRules {
			if rule.pattern.Match(content) {
				hit = append(hit, path)
				break
			}
		}
	}
	sort.Strings(hit)
	return hit, nil
}

// runVerifier 在目录内执行固定验证命令 go build ./...，返回是否通过与耗时。
// 验证命令只依赖标准库，可在离线环境复跑。
func runVerifier(root string) (bool, time.Duration, error) {
	command := exec.Command("go", "build", "./...")
	command.Dir = root
	command.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off", "GOFLAGS=")
	started := time.Now()
	output, err := command.CombinedOutput()
	elapsed := time.Since(started)
	if err != nil {
		return false, elapsed, fmt.Errorf("verify: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return true, elapsed, nil
}

// environmentInfo 记录实验环境，便于复跑时对齐条件。
type environmentInfo struct {
	// Platform 是宿主平台。
	Platform string `json:"platform"`
	// GoVersion 是实验使用的 Go 工具链版本。
	GoVersion string `json:"go_version"`
	// GitVersion 是实验使用的 Git 版本。
	GitVersion string `json:"git_version"`
	// Branches 是并行分支数量。
	Branches int `json:"branches"`
}

// gitVersionDescription 读取本机 Git 版本描述。
func gitVersionDescription(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("git", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// scenarioReport 汇总单个场景下四个分支的实测结果。
type scenarioReport struct {
	// Branches 是参与的分支数。
	Branches int `json:"branches"`
	// ForkMillis 是各分支分叉耗时，单位毫秒。
	ForkMillis []float64 `json:"fork_millis"`
	// PublishSucceeded 是发布成功的分支数。
	PublishSucceeded int `json:"publish_succeeded"`
	// PublishFailed 是发布显式失败的分支数。
	PublishFailed int `json:"publish_failed"`
	// PublishFailureKinds 是失败原因的分类计数。
	PublishFailureKinds map[string]int `json:"publish_failure_kinds"`
	// VerifyPassed 是验证通过的分支数。
	VerifyPassed int `json:"verify_passed"`
	// VerifyFailed 是验证失败的分支数。
	VerifyFailed int `json:"verify_failed"`
	// CrossTalkFiles 是单个分支能看到的其他分支文件数。
	CrossTalkFiles int `json:"cross_talk_files"`
	// ContaminatedBranches 是被其他分支内容破坏的分支数。
	ContaminatedBranches int `json:"contaminated_branches"`
}

// failureKind 把错误归入稳定的分类名，便于跨后端比较失败语义。
func failureKind(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ports.ErrWorkspaceNotAuthorized):
		return "not_authorized"
	case errors.Is(err, ports.ErrWorkspaceVerificationMissing):
		return "verification_missing"
	case errors.Is(err, ports.ErrWorkspaceDrift):
		return "drift"
	case errors.Is(err, ports.ErrWorkspaceConflict):
		return "conflict"
	case errors.Is(err, ports.ErrWorkspaceSecretLeak):
		return "secret_leak"
	case errors.Is(err, ports.ErrWorkspaceDirty):
		return "dirty"
	case errors.Is(err, ports.ErrWorkspaceUnsupported):
		return "unsupported"
	case errors.Is(err, ports.ErrWorkspaceVersionUnknown):
		return "version_unknown"
	default:
		return "other"
	}
}

// countFailure 在分类计数表里累加一次失败。
func countFailure(kinds map[string]int, err error) {
	if kinds == nil {
		return
	}
	kinds[failureKind(err)]++
}

// conflictReport 汇总同文件冲突场景的实测结果。
type conflictReport struct {
	// FirstPublishSucceeded 是第一个分支发布是否成功。
	FirstPublishSucceeded bool `json:"first_publish_succeeded"`
	// SecondPublishFailed 是第二个分支发布是否显式失败。
	SecondPublishFailed bool `json:"second_publish_failed"`
	// FailureKind 是第二个分支失败的分类名。
	FailureKind string `json:"failure_kind"`
	// ConflictPaths 是错误里给出的冲突路径数。
	ConflictPaths int `json:"conflict_paths"`
	// TargetOverwritten 是第二个分支的内容是否覆盖了第一个分支的内容。
	TargetOverwritten bool `json:"target_overwritten"`
}

// driftReport 汇总目标漂移场景的实测结果。
type driftReport struct {
	// DriftDetected 是否显式拒绝了过期摘要的发布。
	DriftDetected bool `json:"drift_detected"`
	// FailureKind 是失败的分类名。
	FailureKind string `json:"failure_kind"`
}

// secretReport 汇总疑似机密场景的实测结果。
type secretReport struct {
	// LeakDetected 是否显式拒绝了含机密的发布。
	LeakDetected bool `json:"leak_detected"`
	// FailureKind 是失败的分类名。
	FailureKind string `json:"failure_kind"`
	// ReportedPaths 是错误里给出的命中路径数。
	ReportedPaths int `json:"reported_paths"`
}

// recoveryReport 汇总失败回滚场景的实测结果。
type recoveryReport struct {
	// BaseRestored 是丢弃失败分支后基线内容是否恢复。
	BaseRestored bool `json:"base_restored"`
	// CollateralDamage 是丢弃失败分支时是否损坏了其他分支的内容。
	CollateralDamage bool `json:"collateral_damage"`
	// RestoredDigest 是回滚后基线的内容摘要前缀，便于人工核对。
	RestoredDigest string `json:"restored_digest_prefix"`
}

// deliveryReport 汇总交付单元场景的实测结果。
type deliveryReport struct {
	// Unit 是交付单元类型：git_patch 或 tar_snapshot。
	Unit string `json:"unit"`
	// Available 是否能产出可审阅的交付单元。
	Available bool `json:"available"`
	// Millis 是产出交付单元的耗时。
	Millis float64 `json:"millis"`
	// Bytes 是交付单元大小。
	Bytes int64 `json:"bytes"`
}

// diskReport 汇总四个分支占用的磁盘。
type diskReport struct {
	// Roots 是承载分支内容的物理目录数：共享目录方案为一个，worktree 方案
	// 为交付目标加四个分支。
	Roots int `json:"roots"`
	// ContentBytes 是工作区内容字节数，不含 Git 元数据。
	ContentBytes int64 `json:"content_bytes"`
	// MetadataBytes 是 Git 元数据字节数；worktree 方案只保留一份对象库。
	MetadataBytes int64 `json:"metadata_bytes"`
	// DependencyCopies 是依赖文件在磁盘上的副本数。
	DependencyCopies int `json:"dependency_copies"`
	// DependencyBytes 是依赖文件单份大小。
	DependencyBytes int64 `json:"dependency_bytes"`
}

// backendReport 汇总单个后端在全部场景下的实测结果。
type backendReport struct {
	// Backend 是后端种类。
	Backend ports.WorkspaceBackendKind `json:"backend"`
	// Isolated 表示该后端是否提供分支级文件隔离。
	Isolated bool `json:"isolated"`
	// Serial 是四分支串行场景结果。
	Serial scenarioReport `json:"serial"`
	// Parallel 是四分支并行场景结果。
	Parallel scenarioReport `json:"parallel"`
	// Conflict 是同文件冲突场景结果。
	Conflict conflictReport `json:"conflict"`
	// Drift 是目标漂移场景结果。
	Drift driftReport `json:"drift"`
	// Secret 是疑似机密场景结果。
	Secret secretReport `json:"secret"`
	// Recovery 是失败回滚场景结果。
	Recovery recoveryReport `json:"recovery"`
	// Delivery 是交付单元场景结果。
	Delivery deliveryReport `json:"delivery"`
	// Disk 是磁盘占用结果。
	Disk diskReport `json:"disk"`
}

// comparisonReport 是对照实验的完整实测结果。
type comparisonReport struct {
	// Environment 是实验环境。
	Environment environmentInfo `json:"environment"`
	// Backends 是逐后端的实测结果。
	Backends []backendReport `json:"backends"`
	// Notes 是复跑方式与数据口径说明。
	Notes []string `json:"notes"`
}

// tarSnapshot 把给定相对路径打包成单个 gzip 归档，作为共享目录方案的交付
// 单元：它没有版本号、没有差异、没有冲突检测，只能整体覆盖。
func tarSnapshot(root string, paths []string) ([]byte, error) {
	buffer := &bytes.Buffer{}
	writer := gzip.NewWriter(buffer)
	archive := tar.NewWriter(writer)
	for _, path := range paths {
		full := filepath.Join(root, filepath.FromSlash(path))
		content, err := os.ReadFile(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		header := &tar.Header{Name: path, Mode: 0o644, Size: int64(len(content))}
		if err := archive.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write tar header %s: %w", path, err)
		}
		if _, err := archive.Write(content); err != nil {
			return nil, fmt.Errorf("write tar body %s: %w", path, err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return buffer.Bytes(), nil
}

// contextWithTimeout 返回带墙钟上限的上下文，避免实验步骤无限等待。
func contextWithTimeout(t *testing.T, timeout time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), timeout)
}

// runtimeInfo 汇总宿主平台信息。
func runtimeInfo() environmentInfo {
	return environmentInfo{
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		GoVersion: runtime.Version(),
		Branches:  experimentBranchCount,
	}
}
