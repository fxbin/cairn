// Package tools 读取用户与工作区的 AGENTS.md，并校验供 Journal 回放的脱敏指令快照。
// Author: fxbin
package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/artifact"
)

// 项目指令在新运行中完整冻结；容量不足时拒绝启动，禁止截断规范正文。
const (
	ProjectInstructionsVersion          = 2
	projectInstructionsLegacyVersion    = 1
	ProjectInstructionsMaxBytes         = 32 * 1024
	ProjectInstructionsMaxDocuments     = 128
	ProjectInstructionsMaxEntries       = 100_000
	ProjectInstructionsMaxSnapshotBytes = 1024 * 1024
	projectInstructionsFilename         = "AGENTS.md"
)

// ProjectInstructionSource 区分用户全局来源与工作区来源；空值兼容旧版项目快照。
type ProjectInstructionSource string

// ProjectInstructionUser 标记从 Cairn 用户配置目录读取、优先级低于项目规则的来源。
const ProjectInstructionUser ProjectInstructionSource = "user"

// ProjectInstruction 保存逻辑来源与目录作用域；SHA256 绑定脱敏正文，SourceSHA256 记录读取时的原文摘要。
type ProjectInstruction struct {
	Source       ProjectInstructionSource `json:"source,omitempty"`
	Path         string                   `json:"path"`
	Scope        string                   `json:"scope"`
	SHA256       string                   `json:"sha256"`
	SourceSHA256 string                   `json:"source_sha256"`
	Content      string                   `json:"content"`
}

// ProjectInstructions 是一次运行固定使用的规范，用户全局来源在先，项目来源按目录深度和路径排序。
type ProjectInstructions struct {
	Version   int                  `json:"version"`
	Documents []ProjectInstruction `json:"documents"`
}

// LoadProjectInstructions 只发现显式工作区内可见的 AGENTS.md；不遍历目录链接或运行态目录，也不解释正文命令。
func LoadProjectInstructions(workspace Workspace) (snapshot ProjectInstructions, err error) {
	return LoadProjectInstructionsWithUser(workspace, "")
}

// LoadProjectInstructionsWithUser 仅读取指定用户配置目录中的单个 AGENTS.md，再有界发现工作区来源；空目录参数禁用用户来源。
func LoadProjectInstructionsWithUser(workspace Workspace, userDirectory string) (snapshot ProjectInstructions, err error) {
	snapshot.Version = ProjectInstructionsVersion
	sourceBytes := 0
	if userDirectory != "" {
		document, readBytes, loadErr := readUserProjectInstruction(userDirectory)
		if loadErr != nil {
			return snapshot, fmt.Errorf("load user AGENTS.md instructions: %w", loadErr)
		}
		sourceBytes = readBytes
		if document.Content != "" {
			snapshot.Documents = append(snapshot.Documents, document)
		}
	}
	root, err := os.OpenRoot(workspace.Root)
	if err != nil {
		return snapshot, errors.New("open project instructions workspace failed")
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	entries := 0
	var visit func(string) error
	visit = func(relative string) error {
		_, resolved, resolveErr := workspace.Resolve(relative)
		if resolveErr != nil || projectInstructionsIgnored(workspace, resolved) {
			return errors.New("project instructions directory is unavailable")
		}
		directory, openErr := root.Open(resolved)
		if openErr != nil {
			return errors.New("open project instructions directory failed")
		}
		children, readErr := directory.ReadDir(ProjectInstructionsMaxEntries - entries + 1)
		closeErr := directory.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
			return errors.New("read project instructions directory failed")
		}
		entries += len(children)
		if entries > ProjectInstructionsMaxEntries {
			return fmt.Errorf("project instructions discovery exceeds %d entries", ProjectInstructionsMaxEntries)
		}
		for _, child := range children {
			candidate := path.Join(relative, child.Name())
			if projectInstructionsIgnored(workspace, candidate) {
				continue
			}
			if child.Name() == projectInstructionsFilename {
				document, readBytes, loadErr := readProjectInstruction(root, workspace, candidate, ProjectInstructionsMaxBytes-sourceBytes)
				if loadErr != nil {
					return fmt.Errorf("load project instructions %q: %w", string(artifact.Redact([]byte(candidate))), loadErr)
				}
				sourceBytes += readBytes
				if document.Content == "" {
					continue
				}
				snapshot.Documents = append(snapshot.Documents, document)
				if len(snapshot.Documents) > ProjectInstructionsMaxDocuments {
					return fmt.Errorf("project instructions exceed %d documents", ProjectInstructionsMaxDocuments)
				}
			} else if child.IsDir() {
				if visitErr := visit(candidate); visitErr != nil {
					return visitErr
				}
			}
		}
		return nil
	}
	if err := visit("."); err != nil {
		return ProjectInstructions{}, err
	}
	sort.Slice(snapshot.Documents, func(left, right int) bool {
		return projectInstructionDocumentLess(snapshot.Documents[left], snapshot.Documents[right])
	})
	return snapshot, snapshot.Validate()
}

// readUserProjectInstruction 缺省文件不产生规约；存在的文件复用受限根读取，禁止链接逃出用户配置目录或读取凭据。
func readUserProjectInstruction(directory string) (document ProjectInstruction, readBytes int, err error) {
	if _, statErr := os.Lstat(filepath.Join(directory, projectInstructionsFilename)); errors.Is(statErr, os.ErrNotExist) {
		return document, 0, nil
	} else if statErr != nil {
		return document, 0, errors.New("inspect user instructions source failed")
	}
	workspace, err := NewWorkspace(directory, []string{"keys.env"}, 0, 0, 0)
	if err != nil {
		return document, 0, errors.New("open user instructions directory failed")
	}
	root, err := os.OpenRoot(workspace.Root)
	if err != nil {
		return document, 0, errors.New("open user instructions root failed")
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	document, readBytes, err = readProjectInstruction(root, workspace, projectInstructionsFilename, ProjectInstructionsMaxBytes)
	if err == nil && document.Content != "" {
		document.Source = ProjectInstructionUser
	}
	return document, readBytes, err
}

// projectInstructionsIgnored 复用工具可见性，并排除仅供 Cairn 协作留证的运行态目录。
func projectInstructionsIgnored(workspace Workspace, relative string) bool {
	if workspace.Ignore(relative) {
		return true
	}
	for _, segment := range strings.Split(filepath.ToSlash(relative), "/") {
		switch segment {
		case ".git", ".agent", ".cairn", ".vidt", "node_modules", "vendor":
			return true
		}
	}
	return false
}

// readProjectInstruction 在工作区解析和根句柄双重约束内有界读取普通文件，返回实际原文字节数；链接目标同样受忽略规则约束。
func readProjectInstruction(root *os.Root, workspace Workspace, relative string, remaining int) (document ProjectInstruction, readBytes int, err error) {
	if !validProjectInstructionPath(relative) {
		return document, 0, errors.New("invalid project instructions source path")
	}
	_, resolved, err := workspace.Resolve(relative)
	if err != nil || projectInstructionsIgnored(workspace, resolved) {
		return document, 0, errors.New("project instructions source escapes visible workspace")
	}
	info, err := root.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return document, 0, errors.New("project instructions source must be a readable regular file")
	}
	file, err := root.Open(resolved)
	if err != nil {
		return document, 0, errors.New("open project instructions source failed")
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return document, 0, errors.New("project instructions source is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(remaining)+1))
	if err != nil {
		return document, 0, errors.New("read project instructions source failed")
	}
	if len(raw) > remaining {
		return document, 0, fmt.Errorf("project instructions exceed combined limit of %d bytes", ProjectInstructionsMaxBytes)
	}
	if !validProjectInstructionText(string(raw)) {
		return document, 0, errors.New("project instructions must contain UTF-8 Markdown without control characters")
	}
	if strings.TrimSpace(string(raw)) == "" {
		return document, len(raw), nil
	}
	content := string(artifact.Redact(raw))
	return ProjectInstruction{Path: relative, Scope: path.Dir(relative), SHA256: projectInstructionDigest([]byte(content)), SourceSHA256: projectInstructionDigest(raw), Content: content}, len(raw), nil
}

// Validate 校验历史快照的版本、来源作用域、完整正文、容量和确定性顺序，不访问当前工作区。
func (snapshot ProjectInstructions) Validate() error {
	if snapshot.Version != ProjectInstructionsVersion && snapshot.Version != projectInstructionsLegacyVersion || len(snapshot.Documents) > ProjectInstructionsMaxDocuments {
		return errors.New("invalid project instructions snapshot version or document count")
	}
	contentBytes := 0
	for index, document := range snapshot.Documents {
		if document.Source != "" && (snapshot.Version != ProjectInstructionsVersion || document.Source != ProjectInstructionUser || document.Path != projectInstructionsFilename) {
			return errors.New("invalid project instructions snapshot source")
		}
		if !validProjectInstructionPath(document.Path) || document.Scope != path.Dir(document.Path) {
			return errors.New("invalid project instructions snapshot scope")
		}
		if index > 0 && !projectInstructionDocumentLess(snapshot.Documents[index-1], document) {
			return errors.New("project instructions snapshot sources are duplicated or out of order")
		}
		if !validProjectInstructionText(document.Content) || strings.TrimSpace(document.Content) == "" || !bytes.Equal(artifact.Redact([]byte(document.Content)), []byte(document.Content)) {
			return errors.New("invalid or unredacted project instructions snapshot content")
		}
		if document.SHA256 != projectInstructionDigest([]byte(document.Content)) || !validProjectInstructionDigest(document.SourceSHA256) {
			return errors.New("project instructions snapshot digest mismatch")
		}
		contentBytes += len(document.Content)
		if contentBytes > ProjectInstructionsMaxBytes {
			return errors.New("project instructions snapshot exceeds combined byte limit")
		}
	}
	return nil
}

// projectInstructionDocumentLess 将用户全局规则放在项目规则之前，同类来源沿用祖先顺序。
func projectInstructionDocumentLess(left, right ProjectInstruction) bool {
	if left.Source != right.Source {
		return left.Source == ProjectInstructionUser
	}
	return projectInstructionLess(left.Path, right.Path)
}

// ReadProjectInstructions 只接受 Journal 中完整的内容寻址引用；缺失、损坏或未知结构均失败关闭。
func ReadProjectInstructions(store *artifact.Store, rawReference json.RawMessage) (ProjectInstructions, error) {
	var reference artifact.Ref
	if json.Unmarshal(rawReference, &reference) != nil || !validProjectInstructionDigest(reference.SHA256) || reference.Bytes < 1 || reference.Bytes > ProjectInstructionsMaxSnapshotBytes || reference.MediaType != "application/json" || store == nil {
		return ProjectInstructions{}, errors.New("invalid project instructions artifact reference")
	}
	raw, err := store.OpenBounded(reference, ProjectInstructionsMaxSnapshotBytes)
	if err != nil {
		return ProjectInstructions{}, errors.New("project instructions artifact is missing or corrupt")
	}
	if int64(len(raw)) != reference.Bytes || len(raw) > ProjectInstructionsMaxSnapshotBytes {
		return ProjectInstructions{}, errors.New("project instructions artifact size mismatch")
	}
	var snapshot ProjectInstructions
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return ProjectInstructions{}, errors.New("decode project instructions snapshot failed")
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return ProjectInstructions{}, errors.New("project instructions snapshot contains trailing data")
	}
	if err := snapshot.Validate(); err != nil {
		return ProjectInstructions{}, err
	}
	return snapshot, nil
}

// validProjectInstructionPath 保持跨平台的工作区相对来源，拒绝可泄露凭据或伪造提示边界的路径文本。
func validProjectInstructionPath(relative string) bool {
	return relative != "" && utf8.ValidString(relative) && validProjectInstructionText(relative) && !strings.ContainsAny(relative, "\\\r\n\t") && !path.IsAbs(relative) && path.Clean(relative) == relative && relative != ".." && !strings.HasPrefix(relative, "../") && path.Base(relative) == projectInstructionsFilename && bytes.Equal(artifact.Redact([]byte(relative)), []byte(relative))
}

// validProjectInstructionText 允许 Markdown 常用换行与缩进，拒绝二进制内容和终端控制字符。
func validProjectInstructionText(content string) bool {
	return utf8.ValidString(content) && strings.IndexFunc(content, func(value rune) bool {
		return unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t'
	}) < 0
}

// projectInstructionLess 将祖先规则排在子目录规则之前，同深度来源按路径排序。
func projectInstructionLess(left, right string) bool {
	leftDepth, rightDepth := strings.Count(left, "/"), strings.Count(right, "/")
	if leftDepth != rightDepth {
		return leftDepth < rightDepth
	}
	return left < right
}

// projectInstructionDigest 对实际字节计算与制品引用一致格式的 SHA-256。
func projectInstructionDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// validProjectInstructionDigest 只接受完整的小写十六进制摘要，避免不可信引用被解释为文件路径。
func validProjectInstructionDigest(digest string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(digest, prefix))
	return err == nil && strings.ToLower(digest) == digest
}
