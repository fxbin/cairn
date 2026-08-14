package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Workspace 是经过规范化和边界检查的工具可见文件系统视图。
type Workspace struct {
	Root           string
	ignored        map[string]struct{}
	MaxReadBytes   int64
	MaxSearchFiles int
	MaxSearchBytes int64
}

// NewWorkspace 创建供模型工具使用的工作区视图，并应用工具级忽略项。
func NewWorkspace(root string, ignored []string, maxReadBytes int64, maxSearchFiles int, maxSearchBytes int64) (Workspace, error) {
	return newWorkspace(root, ignored, maxReadBytes, maxSearchFiles, maxSearchBytes, true)
}

// NewObservationWorkspace 创建 verifier 使用的全工作区视图。它不继承
// workspace.ignore，避免 node_modules 等工具便利项掩盖验证命令的写入；
// 仅排除 Cairn 状态、Git 元数据、敏感路径和显式 volatile 路径。
func NewObservationWorkspace(root string, volatile []string) (Workspace, error) {
	return newWorkspace(root, volatile, 1, 1, 1, false)
}

// ObservationView 从已规范化的工具视图派生 verifier 视图；它不会失败，
// 从而避免策略配置与使用之间重新打开或重新规范化路径。
func (workspace Workspace) ObservationView(volatile []string) Workspace {
	ignored := map[string]struct{}{".git": {}, ".agent": {}, ".cairn": {}}
	for _, entry := range volatile {
		entry = filepath.Clean(entry)
		if entry != "." && !filepath.IsAbs(entry) && !strings.HasPrefix(entry, ".."+string(filepath.Separator)) && entry != ".." {
			ignored[entry] = struct{}{}
		}
	}
	return Workspace{Root: workspace.Root, ignored: ignored, MaxReadBytes: workspace.MaxReadBytes, MaxSearchFiles: workspace.MaxSearchFiles, MaxSearchBytes: workspace.MaxSearchBytes}
}

// newWorkspace 执行工作区路径规范化、目录确认、容量默认值和忽略集构建。
func newWorkspace(root string, ignored []string, maxReadBytes int64, maxSearchFiles int, maxSearchBytes int64, includeToolDefaults bool) (Workspace, error) {
	if root == "" {
		return Workspace{}, errors.New("workspace path is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve workspace path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("canonicalize workspace path: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return Workspace{}, fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return Workspace{}, errors.New("workspace path is not a directory")
	}
	if maxReadBytes <= 0 {
		maxReadBytes = 1024 * 1024
	}
	if maxSearchFiles <= 0 {
		maxSearchFiles = 10_000
	}
	if maxSearchBytes <= 0 {
		maxSearchBytes = 64 * 1024 * 1024
	}
	ignoreSet := map[string]struct{}{".git": {}, ".agent": {}, ".cairn": {}}
	if includeToolDefaults {
		ignoreSet["node_modules"] = struct{}{}
		ignoreSet["vendor"] = struct{}{}
	}
	for _, entry := range ignored {
		entry = filepath.Clean(entry)
		if entry != "." && !filepath.IsAbs(entry) && !strings.HasPrefix(entry, ".."+string(filepath.Separator)) && entry != ".." {
			ignoreSet[entry] = struct{}{}
		}
	}
	return Workspace{Root: canonical, ignored: ignoreSet, MaxReadBytes: maxReadBytes, MaxSearchFiles: maxSearchFiles, MaxSearchBytes: maxSearchBytes}, nil
}

// Resolve 将工作区相对路径解析为物理路径和规范相对路径，并拒绝越界及逃逸
// 符号链接；对尾部组件尚不存在的路径（如 create 目标），同样先解析最深已
// 存在祖先中的符号链接再做逃逸检查，防止"叶子不存在"掩盖祖先链接逃逸。
func (workspace Workspace) Resolve(relative string) (string, string, error) {
	if relative == "" {
		relative = "."
	}
	if filepath.IsAbs(relative) {
		return "", "", errors.New("workspace paths must be relative")
	}
	candidate := filepath.Clean(filepath.Join(workspace.Root, relative))
	relativePath, err := filepath.Rel(workspace.Root, candidate)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", "", errors.New("path escapes workspace")
	}
	resolved, err := resolveWithMissingTail(candidate)
	if err != nil {
		return "", "", err
	}
	resolvedRelative, relErr := filepath.Rel(workspace.Root, resolved)
	if relErr != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("symlink escapes workspace")
	}
	return resolved, filepath.ToSlash(resolvedRelative), nil
}

// resolveWithMissingTail 返回 candidate 的规范物理路径：整条路径可解析时直
// 接解析符号链接；当尾部组件（典型为 create 目标）尚不存在时，定位最深已
// 存在祖先并解析其符号链接后拼回缺失后缀，使祖先目录符号链接（如 link 指
// 向工作区外目录）无法借 ENOENT 分支绕过逃逸检查。
func resolveWithMissingTail(candidate string) (string, error) {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	probe := filepath.Dir(candidate)
	missing := []string{filepath.Base(candidate)}
	for {
		if _, statErr := os.Lstat(probe); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("resolve path: %w", statErr)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", errors.New("resolve path: no existing ancestor")
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
	resolved, err = filepath.EvalSymlinks(probe)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	for index := len(missing) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, missing[index])
	}
	return resolved, nil
}

// Ignore 判断路径是否因敏感性或工作区忽略规则而不可被工具访问。
func (workspace Workspace) Ignore(relative string) bool {
	if sensitivePath(relative) {
		return true
	}
	for current := filepath.Clean(relative); current != "." && current != string(filepath.Separator); current = filepath.Dir(current) {
		if _, ignored := workspace.ignored[current]; ignored {
			return true
		}
	}
	return false
}

// sensitivePath 判断路径名是否可能包含环境变量或凭据。
func sensitivePath(relative string) bool {
	base := strings.ToLower(filepath.Base(filepath.Clean(relative)))
	return base == ".env" || strings.HasPrefix(base, ".env.") || strings.Contains(base, "credential")
}

// Observation 计算当前视图的稳定摘要。模型审批使用 NewWorkspace，而
// verifier 使用 NewObservationWorkspace，确保工具级忽略不能掩盖测试写入。
func (workspace Workspace) Observation() (string, error) {
	entries := make([]string, 0)
	err := filepath.WalkDir(workspace.Root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(workspace.Root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if workspace.Ignore(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			entries = append(entries, fmt.Sprintf("d\t%04o\t%s", mode, relative))
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entries = append(entries, fmt.Sprintf("l\t%04o\t%s\t%s", mode, relative, target))
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		entries = append(entries, fmt.Sprintf("f\t%04o\t%s\t%s", mode, relative, hex.EncodeToString(digest[:])))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("observe workspace: %w", err)
	}
	sort.Strings(entries)
	digest := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// walk 在工作区边界和忽略规则内遍历 start，并跳过符号链接实体。
func (workspace Workspace) walk(start string, visit func(path string, entry os.DirEntry, relative string) error) error {
	root, rootRelative, err := workspace.Resolve(start)
	if err != nil {
		return err
	}
	if workspace.Ignore(rootRelative) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(workspace.Root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if workspace.Ignore(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return visit(path, entry, filepath.ToSlash(relative))
	})
}
