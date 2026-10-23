package desktop

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/tools"
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

// resolveReadableRelative 统一预览/Diff/外部打开的路径边界：
// 使用 tools.Workspace.Resolve 约束符号链接逃逸，并对别名与解析目标
// 同时执行敏感/忽略拒绝，禁止只校验用户给定别名。
func resolveReadableRelative(workspace, relative string) (root string, resolved string, canonical string, err error) {
	root, err = absWorkspace(workspace)
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(relative) == "" {
		return "", "", "", errInvalid("relative path is required")
	}
	if filepath.IsAbs(relative) {
		return "", "", "", errInvalid("absolute paths are not allowed")
	}
	ignore := workspaceIgnore(root)
	ws, err := tools.NewWorkspace(root, ignore, 0, 0, 0)
	if err != nil {
		return "", "", "", err
	}
	resolvedPath, resolvedRel, err := ws.Resolve(relative)
	if err != nil {
		return "", "", "", errInvalid(err.Error())
	}
	aliasRel := filepath.ToSlash(filepath.Clean(relative))
	// 敏感策略同时覆盖别名与解析目标，防止符号链接别名绕过。
	if ws.Ignore(aliasRel) || ws.Ignore(resolvedRel) ||
		isSensitiveRelative(aliasRel) || isSensitiveRelative(resolvedRel) {
		return "", "", "", errInvalid("path is sensitive or ignored")
	}
	return root, resolvedPath, resolvedRel, nil
}

// workspaceIgnore 返回当前工作区用于工具视图的忽略列表；缺少配置时用安全缺省。
func workspaceIgnore(root string) []string {
	configuration, err := loadConfiguration(root, "")
	if err != nil {
		return []string{".git", ".agent", ".cairn", "node_modules", "vendor"}
	}
	if len(configuration.Workspace.Ignore) == 0 {
		return []string{".git", ".agent", ".cairn", "node_modules", "vendor"}
	}
	return append([]string(nil), configuration.Workspace.Ignore...)
}

// PreviewFile 返回有界文本预览；二进制/敏感/超大文件失败关闭。
func PreviewFile(workspace, relative string) (FilePreview, error) {
	_, full, canonical, err := resolveReadableRelative(workspace, relative)
	if err != nil {
		if strings.Contains(err.Error(), "sensitive") {
			return FilePreview{Relative: filepath.Clean(relative), Reason: "sensitive"}, err
		}
		return FilePreview{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return FilePreview{}, err
	}
	if info.IsDir() {
		return FilePreview{}, errInvalid("path is a directory")
	}
	preview := FilePreview{Relative: canonical, Path: full, Size: info.Size(), Language: languageOf(canonical)}
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
// 比较范围区分 untracked / staged / unstaged / mixed / deleted / renamed。
func ListChanges(workspace string) (ChangeSet, error) {
	root, err := absWorkspace(workspace)
	if err != nil {
		return ChangeSet{}, err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ChangeSet{}, err
	}
	gitRoot, branch, _ := gitStatus(canonicalRoot)
	set := ChangeSet{Root: root, Branch: branch, Entries: []ChangeEntry{}}
	if gitRoot == "" {
		return set, nil
	}
	// -z 保证含空格/中文/重命名的路径不被截断。
	out, err := runGit(root, "status", "--porcelain", "-z", "--", ".")
	if err != nil {
		return set, nil
	}
	records := splitPorcelainZ(out)
	for _, record := range records {
		if len(record) < 4 {
			continue
		}
		statusCode := record[:2]
		relative := record[3:]
		if statusCode[0] == 'R' || statusCode[0] == 'C' {
			// rename/copy: 后续 NUL 为旧路径，此处取新路径。
			parts := strings.SplitN(record[3:], "\x00", 2)
			relative = parts[0]
		}
		// porcelain 路径始终相对仓库根；转换回显式工作区边界。
		relative, err = filepath.Rel(canonicalRoot, filepath.Join(gitRoot, relative))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		status := classifyPorcelain(statusCode)
		entry := ChangeEntry{Relative: relative, Status: status, AgentOwned: false, Evidence: "git_status_porcelain_z"}
		switch status {
		case "deleted":
			set.Deleted++
		case "untracked":
			set.Untracked++
		case "renamed":
			set.Modified++
		default:
			set.Modified++
		}
		set.Entries = append(set.Entries, entry)
	}
	return set, nil
}

// splitPorcelainZ 拆分 git status -z 输出；路径可含空格与非 ASCII。
func splitPorcelainZ(out []byte) []string {
	var records []string
	fields := strings.Split(string(out), "\x00")
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if len(field) < 4 {
			continue
		}
		records = append(records, field)
		// 重命名或复制的下一个 NUL 字段是源路径，不是另一个状态记录。
		if strings.ContainsAny(field[:2], "RC") {
			index++
		}
	}
	return records
}

// DiffScope 标识 Diff 比较范围。
type DiffScope string

const (
	// DiffScopeUnstaged 工作区相对 index 的改动。
	DiffScopeUnstaged DiffScope = "unstaged"
	// DiffScopeStaged index 相对 HEAD 的改动。
	DiffScopeStaged DiffScope = "staged"
	// DiffScopeUntracked 未跟踪文件的完整内容视图。
	DiffScopeUntracked DiffScope = "untracked"
	// DiffScopeAll 合并 unstaged+staged 的默认展示。
	DiffScopeAll DiffScope = "all"
)

// DiffFile 返回单文件统一 diff（有界）。
// 路径与敏感策略和预览一致；命令失败不得伪装成无改动。
// scope 缺省为 all：同时覆盖仅暂存与仅未暂存。
func DiffFile(workspace, relative string, scope DiffScope) (string, error) {
	root, _, canonical, err := resolveReadableRelative(workspace, relative)
	if err != nil {
		return "", err
	}
	if scope == "" {
		scope = DiffScopeAll
	}
	if scope != DiffScopeAll && scope != DiffScopeUnstaged && scope != DiffScopeStaged && scope != DiffScopeUntracked {
		return "", errInvalid("unsupported diff scope")
	}
	var sections []string
	appendBounded := func(raw []byte) {
		if len(raw) == 0 {
			return
		}
		const maxDiff = 512 * 1024
		if len(raw) > maxDiff {
			raw = append(append([]byte(nil), raw[:maxDiff]...), []byte("\n… truncated")...)
		}
		sections = append(sections, string(raw))
	}
	if scope == DiffScopeUnstaged || scope == DiffScopeAll {
		out, err := runGit(root, "diff", "--no-ext-diff", "--no-textconv", "--", canonical)
		if err != nil {
			return "", errors.New("diff unavailable: unstaged")
		}
		appendBounded(out)
	}
	if scope == DiffScopeStaged || scope == DiffScopeAll {
		out, err := runGit(root, "diff", "--no-ext-diff", "--no-textconv", "--cached", "--", canonical)
		if err != nil {
			return "", errors.New("diff unavailable: staged")
		}
		appendBounded(out)
	}
	if scope == DiffScopeUntracked || scope == DiffScopeAll && len(sections) == 0 {
		// 只有确认为未跟踪的文件才展示完整新增内容；干净的 tracked 文件保持无差异。
		untracked, err := runGit(root, "ls-files", "--others", "--exclude-standard", "-z", "--", canonical)
		if err != nil {
			return "", errors.New("diff unavailable: untracked status")
		}
		if len(untracked) > 0 {
			preview, err := PreviewFile(root, canonical)
			if err != nil || preview.Binary {
				return "", errors.New("diff unavailable: untracked content")
			}
			out, diffErr := runGit(root, "diff", "--no-ext-diff", "--no-textconv", "--no-index", "--", os.DevNull, canonical)
			var exitErr *exec.ExitError
			if diffErr != nil && (!errors.As(diffErr, &exitErr) || exitErr.ExitCode() != 1) {
				return "", errors.New("diff unavailable: untracked")
			}
			appendBounded(out)
		}
	}
	return strings.Join(sections, "\n"), nil
}

// classifyPorcelain 将 Git 的 index/工作区双列状态映射为展示分类。
func classifyPorcelain(code string) string {
	if strings.Contains(code, "?") {
		return "untracked"
	}
	x, y := byte(' '), byte(' ')
	if len(code) > 0 {
		x = code[0]
	}
	if len(code) > 1 {
		y = code[1]
	}
	staged := x != ' ' && x != '?'
	unstaged := y != ' ' && y != '?'
	switch {
	case x == 'R' || x == 'C':
		return "renamed"
	case x == 'D' && y == ' ':
		return "deleted"
	case x == ' ' && y == 'D':
		return "deleted"
	case staged && unstaged:
		return "mixed"
	case staged:
		return "staged"
	case unstaged:
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

// WriteProjectInstructions 在工作区内写入 AGENTS.md；路径固定，原子写，不解释正文命令。
func WriteProjectInstructions(workspace, content string) error {
	if !utf8.ValidString(content) || len(content) > tools.ProjectInstructionsMaxBytes {
		return errInvalid("project instructions must be bounded UTF-8 text")
	}
	root, err := absWorkspace(workspace)
	if err != nil {
		return err
	}
	ws, err := tools.NewWorkspace(root, nil, 0, 0, 0)
	if err != nil {
		return err
	}
	if _, _, err := ws.Resolve("AGENTS.md"); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Lstat(filepath.Join(ws.Root, "AGENTS.md")); err == nil {
		if !info.Mode().IsRegular() {
			return errInvalid("project instructions must be a regular file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(ws.Root, ".agents-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return err
	}
	if err := os.Chmod(temporary.Name(), mode); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), filepath.Join(ws.Root, "AGENTS.md")); err != nil {
		return err
	}
	directory, err := os.Open(ws.Root)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
