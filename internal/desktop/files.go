package desktop

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 本文件提供有界文件树与安全预览：禁止符号链接逃逸工作区，拒绝敏感与超大/
// 二进制文件，外部编辑器入口不授予任意 shell。

const (
	// maxPreviewBytes 限制预览读取的字节数，避免超大文件拖垮 UI。
	maxPreviewBytes = 256 * 1024
	// maxTreeEntries 限制文件树节点数，防止无界枚举。
	maxTreeEntries = 2000
	// maxTreeDepth 限制目录展开深度。
	maxTreeDepth = 8
)

// FileNode 是有界文件树节点。
type FileNode struct {
	Name     string     `json:"name"`
	Relative string     `json:"relative"`
	Path     string     `json:"path"`
	Kind     string     `json:"kind"` // file | dir | symlink | blocked
	Size     int64      `json:"size,omitempty"`
	Children []FileNode `json:"children,omitempty"`
	Reason   string     `json:"reason,omitempty"`
}

// FilePreview 是源码预览正文与元数据。
type FilePreview struct {
	Relative  string `json:"relative"`
	Path      string `json:"path"`
	Language  string `json:"language,omitempty"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	Text      string `json:"text,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ChangeEntry 是工作区改动摘要；归属需证据，不能猜测。
type ChangeEntry struct {
	Relative string `json:"relative"`
	Status   string `json:"status"` // untracked | modified | deleted | staged | other
	// AgentOwned 仅在有明确证据时为真；未知保持 false 并标注 Evidence。
	AgentOwned bool   `json:"agent_owned"`
	Evidence   string `json:"evidence,omitempty"`
}

// ChangeSet 是改动列表与统计。
type ChangeSet struct {
	Root      string        `json:"root"`
	Branch    string        `json:"branch,omitempty"`
	Entries   []ChangeEntry `json:"entries"`
	Added     int           `json:"added"`
	Deleted   int           `json:"deleted"`
	Modified  int           `json:"modified"`
	Untracked int           `json:"untracked"`
}

// ListFileTree 生成工作区有界文件树。
func ListFileTree(workspace string) (FileNode, error) {
	root, err := absWorkspace(workspace)
	if err != nil {
		return FileNode{}, err
	}
	if !dirExists(root) {
		return FileNode{}, os.ErrNotExist
	}
	// 先解析 root 本身（macOS /var 等为符号链接），避免相对比较误判逃逸。
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = root
	}
	node, count, err := buildTree(resolvedRoot, resolvedRoot, 0, 0)
	if err != nil {
		return node, err
	}
	_ = count
	node.Path = root
	node.Relative = "."
	return node, nil
}

func buildTree(root, current string, depth, count int) (FileNode, int, error) {
	node := FileNode{Name: filepath.Base(current), Relative: relOf(root, current), Path: current, Kind: "dir"}
	if depth >= maxTreeDepth {
		node.Reason = "max_depth"
		return node, count, nil
	}
	entries, err := os.ReadDir(current)
	if err != nil {
		node.Kind = "blocked"
		node.Reason = "unreadable"
		return node, count, nil
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if count >= maxTreeEntries {
			node.Reason = "truncated"
			return node, count, nil
		}
		name := entry.Name()
		if name == ".git" || name == "node_modules" || name == ".vidt" {
			continue
		}
		full := filepath.Join(current, name)
		// 禁止符号链接逃逸：解析后必须仍在 root 内。
		resolved, resolveErr := filepath.EvalSymlinks(full)
		if resolveErr != nil {
			node.Children = append(node.Children, FileNode{Name: name, Relative: relOf(root, full), Path: full, Kind: "blocked", Reason: "symlink_unresolved"})
			count++
			continue
		}
		relResolved, relErr := filepath.Rel(root, resolved)
		if relErr != nil || strings.HasPrefix(relResolved, "..") {
			node.Children = append(node.Children, FileNode{Name: name, Relative: relOf(root, full), Path: full, Kind: "blocked", Reason: "symlink_escape"})
			count++
			continue
		}
		if entry.IsDir() {
			child, nextCount, _ := buildTree(root, full, depth+1, count+1)
			node.Children = append(node.Children, child)
			count = nextCount
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			node.Children = append(node.Children, FileNode{Name: name, Relative: relOf(root, full), Path: full, Kind: "symlink", Reason: "symlink"})
			count++
			continue
		}
		if isSensitiveRelative(relOf(root, full)) {
			node.Children = append(node.Children, FileNode{Name: name, Relative: relOf(root, full), Path: full, Kind: "blocked", Reason: "sensitive"})
			count++
			continue
		}
		info, infoErr := entry.Info()
		size := int64(0)
		if infoErr == nil {
			size = info.Size()
		}
		node.Children = append(node.Children, FileNode{Name: name, Relative: relOf(root, full), Path: full, Kind: "file", Size: size})
		count++
	}
	return node, count, nil
}

// PreviewFile 返回有界文本预览；二进制/敏感/超大文件失败关闭。
func PreviewFile(workspace, relative string) (FilePreview, error) {
	root, err := absWorkspace(workspace)
	if err != nil {
		return FilePreview{}, err
	}
	if strings.TrimSpace(relative) == "" {
		return FilePreview{}, errInvalid("relative path is required")
	}
	if filepath.IsAbs(relative) {
		return FilePreview{}, errInvalid("absolute paths are not allowed")
	}
	clean := filepath.Clean(relative)
	if strings.HasPrefix(clean, "..") {
		return FilePreview{}, errInvalid("path escapes workspace")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = root
	}
	full := filepath.Join(resolvedRoot, clean)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return FilePreview{}, err
	}
	relResolved, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || strings.HasPrefix(relResolved, "..") {
		return FilePreview{}, errInvalid("symlink escapes workspace")
	}
	if isSensitiveRelative(clean) {
		return FilePreview{Relative: clean, Path: full, Reason: "sensitive"}, errInvalid("sensitive file is not previewable")
	}
	info, err := os.Stat(full)
	if err != nil {
		return FilePreview{}, err
	}
	if info.IsDir() {
		return FilePreview{}, errInvalid("path is a directory")
	}
	preview := FilePreview{Relative: clean, Path: full, Size: info.Size(), Language: languageOf(clean)}
	file, err := os.Open(full)
	if err != nil {
		return preview, err
	}
	defer file.Close()
	// 一次性有界读取，避免预读导致正文丢失。
	limited := io.LimitReader(file, maxPreviewBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return preview, err
	}
	if len(buf) > maxPreviewBytes {
		buf = buf[:maxPreviewBytes]
		preview.Truncated = true
	}
	if isBinary(buf) {
		preview.Binary = true
		preview.Reason = "binary"
		return preview, nil
	}
	preview.Text = string(buf)
	return preview, nil
}

// ListChanges 读取 git 状态；非 git 或命令失败时返回空集合，不猜测归属。
func ListChanges(workspace string) (ChangeSet, error) {
	root, err := absWorkspace(workspace)
	if err != nil {
		return ChangeSet{}, err
	}
	gitRoot, branch, _ := gitStatus(root)
	set := ChangeSet{Root: root, Branch: branch, Entries: []ChangeEntry{}}
	if gitRoot == "" {
		return set, nil
	}
	out, err := runGit(root, "status", "--porcelain")
	if err != nil {
		return set, nil
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 {
			continue
		}
		statusCode := strings.TrimSpace(line[:2])
		relative := strings.TrimSpace(line[3:])
		entry := ChangeEntry{Relative: relative, Status: classifyPorcelain(statusCode), AgentOwned: false, Evidence: "git_status_only"}
		switch entry.Status {
		case "added":
			// porcelain XY 中 staged 新增计入 modified 统计更合适；此处用 untracked/modified。
		case "deleted":
			set.Deleted++
		case "untracked":
			set.Untracked++
		default:
			set.Modified++
		}
		set.Entries = append(set.Entries, entry)
	}
	return set, nil
}

// DiffFile 返回单文件统一 diff（有界）。
func DiffFile(workspace, relative string) (string, error) {
	root, err := absWorkspace(workspace)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(relative) == "" {
		return "", errInvalid("relative path is required")
	}
	out, err := runGit(root, "diff", "--", relative)
	if err != nil {
		return "", errors.New("diff unavailable")
	}
	const maxDiff = 512 * 1024
	if len(out) > maxDiff {
		return string(out[:maxDiff]) + "\n… truncated", nil
	}
	return string(out), nil
}

func classifyPorcelain(code string) string {
	switch {
	case strings.Contains(code, "?"):
		return "untracked"
	case strings.Contains(code, "D"):
		return "deleted"
	case strings.Contains(code, "A"):
		return "staged"
	case strings.Contains(code, "M"), strings.Contains(code, "R"), strings.Contains(code, "C"):
		return "modified"
	default:
		return "other"
	}
}

func isSensitiveRelative(relative string) bool {
	base := strings.ToLower(filepath.Base(relative))
	if strings.HasSuffix(base, ".env") || base == "keys.env" || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
		return true
	}
	lower := strings.ToLower(relative)
	for _, marker := range []string{"id_rsa", "id_ed25519", "credentials", "secrets.", ".netrc", "npmrc"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isBinary(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

func languageOf(relative string) string {
	switch strings.ToLower(filepath.Ext(relative)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".json":
		return "json"
	case ".md":
		return "markdown"
	case ".yaml", ".yml":
		return "yaml"
	case ".css":
		return "css"
	case ".html":
		return "html"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	default:
		return "text"
	}
}

func relOf(root, full string) string {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return full
	}
	return rel
}
