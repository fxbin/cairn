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

type Workspace struct {
	Root           string
	ignored        map[string]struct{}
	MaxReadBytes   int64
	MaxSearchFiles int
	MaxSearchBytes int64
}

func NewWorkspace(root string, ignored []string, maxReadBytes int64, maxSearchFiles int, maxSearchBytes int64) (Workspace, error) {
	return newWorkspace(root, ignored, maxReadBytes, maxSearchFiles, maxSearchBytes, true)
}

// NewObservationWorkspace creates the whole-workspace view used by the
// verifier. Unlike model tools, this view must not inherit workspace.ignore:
// a tool-only convenience such as node_modules cannot hide verifier-induced
// writes. Only Cairn's own state, Git metadata, sensitive paths, and the
// explicitly configured volatile paths remain excluded.
func NewObservationWorkspace(root string, volatile []string) (Workspace, error) {
	return newWorkspace(root, volatile, 1, 1, 1, false)
}

// ObservationView derives a verifier view from an already-canonical tool
// workspace. It cannot fail, which keeps dependency assembly fail-closed
// without reopening or recanonicalizing a path between policy setup and use.
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
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		resolvedRelative, relErr := filepath.Rel(workspace.Root, resolved)
		if relErr != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
			return "", "", errors.New("symlink escapes workspace")
		}
		return resolved, filepath.ToSlash(resolvedRelative), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("resolve path: %w", err)
	}
	return candidate, filepath.ToSlash(relativePath), nil
}

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

func sensitivePath(relative string) bool {
	base := strings.ToLower(filepath.Base(filepath.Clean(relative)))
	return base == ".env" || strings.HasPrefix(base, ".env.") || strings.Contains(base, "credential")
}

// Observation returns a stable digest for this workspace view. Callers choose
// the view deliberately: model approvals use NewWorkspace, while verifier
// integrity uses NewObservationWorkspace so tool-only ignores cannot mask a
// test command changing the repository.
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
