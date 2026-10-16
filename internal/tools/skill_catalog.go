// Package tools 为 SKILL.md 技能提供有界目录和不可授予权限的内容快照。
// Author: fxbin
package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/canonicaljson"
	"gopkg.in/yaml.v3"
)

// 技能限制约束单次读取、目录遍历和启动目录的最大资源消耗。
const (
	SkillContentVersion             = 1
	SkillMaxNameCharacters          = 64
	SkillMaxDescriptionCharacters   = 1024
	SkillMaxCompatibilityCharacters = 500
	SkillMaxInstructionsBytes       = 64 * 1024
	SkillMaxResourceBytes           = 1024 * 1024
	SkillMaxPackageBytes            = 8 * 1024 * 1024
	SkillMaxFiles                   = 512
	SkillMaxPackageEntries          = 1024
	SkillMaxCatalogEntries          = 128
	skillDisabledFile               = ".cairn-disabled"
)

// skillNamePattern 与 Agent Skills 的可移植 ASCII 名称规则一致。
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// SkillRoot 声明一个显式装配的目录；顺序靠前的同名技能覆盖后续目录。
type SkillRoot struct {
	Path                string
	Scope               string
	AllowLinkedPackages bool
}

// SkillEntry 是可以展示的技能元数据；物理目录仅供内部受限读取。
type SkillEntry struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Compatibility string `json:"compatibility,omitempty"`
	Scope         string `json:"scope"`
	Disabled      bool   `json:"disabled"`
	ManualOnly    bool   `json:"manual_only"`
	UserInvocable bool   `json:"user_invocable"`
	SHA256        string `json:"sha256"`
	SourceDigest  string `json:"source_digest"`
	directory     string
}

// SkillDiagnostic 记录无效或被覆盖的目录，不复制原始文件或 YAML 错误正文。
type SkillDiagnostic struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Code  string `json:"code"`
}

// SkillResource 绑定一个普通资源文件的相对位置、长度与原始摘要。
type SkillResource struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// SkillContent 是成功加载后可写入 Journal 的版本化内容，不包含宿主机目录。
type SkillContent struct {
	Version       int             `json:"version"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Compatibility string          `json:"compatibility,omitempty"`
	Scope         string          `json:"scope"`
	SourceDigest  string          `json:"source_digest"`
	SHA256        string          `json:"sha256"`
	Instructions  string          `json:"instructions"`
	Resources     []SkillResource `json:"resources"`
}

// SkillCatalog 保存启动时发现的元数据；扫描不执行任何技能代码。
type SkillCatalog struct {
	entries     []SkillEntry
	Diagnostics []SkillDiagnostic
}

// skillFrontmatter 只解释内容格式与调用可见性，allowed-tools 和其他客户端扩展不授予能力。
type skillFrontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	Metadata      map[string]string `yaml:"metadata"`
	ManualOnly    bool              `yaml:"disable-model-invocation"`
	UserInvocable *bool             `yaml:"user-invocable"`
}

// ValidSkillName 校验可移植技能名称，禁止路径字符及连续连字符。
func ValidSkillName(name string) bool {
	return len(name) <= SkillMaxNameCharacters && skillNamePattern.MatchString(name)
}

// parseSkill 严格校验必需字段，未知扩展仅作为文件内容保留且不执行。
func parseSkill(raw []byte, directoryName string) (skillFrontmatter, string, error) {
	var metadata skillFrontmatter
	if len(raw) > SkillMaxInstructionsBytes || !utf8.Valid(raw) || strings.IndexFunc(string(raw), func(value rune) bool {
		return unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t'
	}) >= 0 {
		return metadata, "", errors.New("invalid skill text or size")
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return metadata, "", errors.New("skill requires YAML frontmatter")
	}
	end := 1
	for end < len(lines) && lines[end] != "---" {
		end++
	}
	if end == len(lines) {
		return metadata, "", errors.New("skill frontmatter is not closed")
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return metadata, "", errors.New("invalid skill frontmatter")
	}
	fields := document.Content[0].Content
	for index := 0; index+1 < len(fields); index += 2 {
		key, value := fields[index].Value, fields[index+1]
		if (key == "name" || key == "description" || key == "compatibility" || key == "license") && (value.Kind != yaml.ScalarNode || value.Tag != "!!str") {
			return metadata, "", errors.New("skill metadata text fields must be strings")
		}
	}
	if err := document.Decode(&metadata); err != nil {
		return metadata, "", errors.New("invalid skill frontmatter")
	}
	if !ValidSkillName(metadata.Name) || metadata.Name != directoryName {
		return metadata, "", errors.New("skill name must match its directory")
	}
	if string(artifact.Redact([]byte(metadata.Name))) != metadata.Name {
		return metadata, "", errors.New("skill name contains a credential-like value")
	}
	if strings.TrimSpace(metadata.Description) == "" || utf8.RuneCountInString(metadata.Description) > SkillMaxDescriptionCharacters {
		return metadata, "", errors.New("invalid skill description")
	}
	if utf8.RuneCountInString(metadata.Compatibility) > SkillMaxCompatibilityCharacters {
		return metadata, "", errors.New("invalid skill compatibility")
	}
	metadata.Description = strings.Join(strings.Fields(metadata.Description), " ")
	metadata.Compatibility = strings.Join(strings.Fields(metadata.Compatibility), " ")
	body := strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	if body == "" {
		return metadata, "", errors.New("skill instructions are empty")
	}
	return metadata, body, nil
}

// DiscoverSkills 按显式优先级扫描一级技能目录，拒绝越界链接并报告冲突。
func DiscoverSkills(roots []SkillRoot) (*SkillCatalog, error) {
	catalog := &SkillCatalog{}
	seen := make(map[string]bool)
	for _, root := range roots {
		if _, err := os.Lstat(root.Path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, errors.New("inspect skill root failed")
		}
		workspace, err := NewWorkspace(root.Path, nil, SkillMaxResourceBytes, SkillMaxFiles, SkillMaxPackageBytes)
		if err != nil {
			return nil, errors.New("open skill root failed")
		}
		entries, err := skillDirectoryEntries(workspace.Root)
		if err != nil {
			return nil, errors.New("read skill root failed")
		}
		for _, candidate := range entries {
			if strings.HasPrefix(candidate.Name(), ".") {
				continue
			}
			if len(catalog.entries)+len(catalog.Diagnostics) >= SkillMaxCatalogEntries {
				return nil, errors.New("skill catalog exceeds entry limit")
			}
			directory, err := resolveSkillDirectory(workspace, candidate, root.AllowLinkedPackages)
			if err != nil {
				catalog.Diagnostics = append(catalog.Diagnostics, skillDiagnostic(candidate.Name(), root.Scope, "invalid_directory"))
				continue
			}
			view, err := NewWorkspace(directory, nil, SkillMaxResourceBytes, SkillMaxFiles, SkillMaxPackageBytes)
			if err != nil {
				return nil, errors.New("open skill directory failed")
			}
			raw, err := skillReadFile(view, "SKILL.md", SkillMaxInstructionsBytes)
			if err != nil {
				catalog.Diagnostics = append(catalog.Diagnostics, skillDiagnostic(candidate.Name(), root.Scope, "invalid_skill_file"))
				continue
			}
			metadata, _, err := parseSkill(raw, candidate.Name())
			if err != nil {
				catalog.Diagnostics = append(catalog.Diagnostics, skillDiagnostic(candidate.Name(), root.Scope, "invalid_frontmatter"))
				continue
			}
			if seen[metadata.Name] {
				catalog.Diagnostics = append(catalog.Diagnostics, skillDiagnostic(metadata.Name, root.Scope, "shadowed"))
				continue
			}
			seen[metadata.Name] = true
			_, disabledErr := os.Lstat(filepath.Join(directory, skillDisabledFile))
			if disabledErr != nil && !errors.Is(disabledErr, os.ErrNotExist) {
				return nil, errors.New("read skill enabled state failed")
			}
			invocable := metadata.UserInvocable == nil || *metadata.UserInvocable
			metadata.Description = string(artifact.Redact([]byte(metadata.Description)))
			metadata.Compatibility = string(artifact.Redact([]byte(metadata.Compatibility)))
			catalog.entries = append(catalog.entries, SkillEntry{Name: metadata.Name, Description: metadata.Description, Compatibility: metadata.Compatibility, Scope: root.Scope, Disabled: disabledErr == nil, ManualOnly: metadata.ManualOnly, UserInvocable: invocable, SHA256: skillDigest(raw), SourceDigest: skillDigest([]byte(directory)), directory: directory})
		}
	}
	sort.Slice(catalog.entries, func(i, j int) bool { return catalog.entries[i].Name < catalog.entries[j].Name })
	return catalog, nil
}

// skillDiagnostic 在格式化诊断之前脱敏外部目录名，避免无效技能把凭据带进错误输出。
func skillDiagnostic(name, scope, code string) SkillDiagnostic {
	return SkillDiagnostic{Name: string(artifact.Redact([]byte(name))), Scope: scope, Code: code}
}

// resolveSkillDirectory 仅对显式授权的用户共享根接纳包级链接，包内读取仍限制在规范根中。
func resolveSkillDirectory(view Workspace, candidate os.DirEntry, allowLinked bool) (string, error) {
	if allowLinked && candidate.Type()&os.ModeSymlink != 0 {
		directory, err := filepath.EvalSymlinks(filepath.Join(view.Root, candidate.Name()))
		if err != nil {
			return "", errors.New("linked skill is unavailable")
		}
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			return "", errors.New("linked skill is not a directory")
		}
		return directory, nil
	}
	if !candidate.IsDir() {
		return "", errors.New("skill is not a directory")
	}
	directory, _, err := view.Resolve(candidate.Name())
	return directory, err
}

// Entries 返回元数据副本，避免调用者修改目录绑定。
func (catalog *SkillCatalog) Entries() []SkillEntry {
	return append([]SkillEntry(nil), catalog.entries...)
}

// Inspect 为用户显式查看已停用的技能，保持目录本身的启停状态不变。
func (catalog *SkillCatalog) Inspect(name string) (SkillContent, error) {
	copy := &SkillCatalog{entries: catalog.Entries()}
	for index := range copy.entries {
		if copy.entries[index].Name == name {
			copy.entries[index].Disabled = false
		}
	}
	return copy.Load(name)
}

// find 按名称返回确定优先级选中的目录。
func (catalog *SkillCatalog) find(name string) (SkillEntry, bool) {
	for _, entry := range catalog.entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return SkillEntry{}, false
}

// Load 校验发现时的正文摘要并冻结普通文件清单，不隐式加载或运行资源。
func (catalog *SkillCatalog) Load(name string) (SkillContent, error) {
	entry, found := catalog.find(name)
	if !found || entry.Disabled {
		return SkillContent{}, errors.New("skill is unavailable")
	}
	view, err := NewWorkspace(entry.directory, nil, SkillMaxResourceBytes, SkillMaxFiles, SkillMaxPackageBytes)
	if err != nil || skillDigest([]byte(view.Root)) != entry.SourceDigest {
		return SkillContent{}, errors.New("skill source changed")
	}
	raw, err := skillReadFile(view, "SKILL.md", SkillMaxInstructionsBytes)
	if err != nil || skillDigest(raw) != entry.SHA256 {
		return SkillContent{}, errors.New("skill instructions changed; restart with the reviewed version")
	}
	_, body, err := parseSkill(raw, name)
	if err != nil {
		return SkillContent{}, err
	}
	resources, err := skillResources(view)
	if err != nil {
		return SkillContent{}, err
	}
	return SkillContent{Version: SkillContentVersion, Name: name, Description: entry.Description, Compatibility: entry.Compatibility, Scope: entry.Scope, SourceDigest: entry.SourceDigest, SHA256: entry.SHA256, Instructions: body, Resources: resources}, nil
}

// ReadResource 只读取成功激活快照列出的文件，并验证来源与文件摘要仍匹配。
func (catalog *SkillCatalog) ReadResource(content SkillContent, relative string) ([]byte, error) {
	entry, found := catalog.find(content.Name)
	if !found || entry.Disabled || entry.SourceDigest != content.SourceDigest || entry.SHA256 != content.SHA256 {
		return nil, errors.New("skill source changed or is unavailable")
	}
	var resource *SkillResource
	for _, candidate := range content.Resources {
		if candidate.Path == relative {
			value := candidate
			resource = &value
			break
		}
	}
	if resource == nil {
		return nil, errors.New("resource is not in the activated skill snapshot")
	}
	view, err := NewWorkspace(entry.directory, nil, SkillMaxResourceBytes, SkillMaxFiles, SkillMaxPackageBytes)
	if err != nil || skillDigest([]byte(view.Root)) != content.SourceDigest {
		return nil, errors.New("skill source changed")
	}
	main, err := skillReadFile(view, "SKILL.md", SkillMaxInstructionsBytes)
	if err != nil || skillDigest(main) != content.SHA256 {
		return nil, errors.New("skill instructions changed")
	}
	raw, err := skillReadFile(view, relative, SkillMaxResourceBytes)
	if err != nil || int64(len(raw)) != resource.Bytes || skillDigest(raw) != resource.SHA256 {
		return nil, errors.New("skill resource changed or is unavailable")
	}
	return raw, nil
}

// skillReadFile 先经过 Workspace.Resolve，再用根目录句柄约束实际打开，拒绝特殊文件和无限读取。
func skillReadFile(view Workspace, relative string, maximum int64) ([]byte, error) {
	root, err := os.OpenRoot(view.Root)
	if err != nil {
		return nil, errors.New("open skill root failed")
	}
	defer root.Close()
	_, resolved, err := view.Resolve(relative)
	if err != nil || view.Ignore(resolved) {
		return nil, errors.New("skill path is not allowed")
	}
	info, err := root.Lstat(filepath.FromSlash(resolved))
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("skill file is not a bounded regular file")
	}
	file, err := root.Open(filepath.FromSlash(resolved))
	if err != nil {
		return nil, errors.New("open skill file failed")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid skill file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(raw)) > maximum {
		return nil, errors.New("skill file exceeds limit")
	}
	return raw, nil
}

// skillResources 对包内每个普通文件计算摘要，禁止链接、敏感文件和超限包。
func skillResources(view Workspace) ([]SkillResource, error) {
	root, err := os.OpenRoot(view.Root)
	if err != nil {
		return nil, errors.New("open skill package failed")
	}
	defer root.Close()
	resources := []SkillResource{}
	var total int64
	err = walkSkillPackage(root.FS(), func(path string, entry fs.DirEntry) error {
		if path == skillDisabledFile {
			return nil
		}
		if view.Ignore(path) {
			return errors.New("skill package contains an ignored or sensitive path")
		}
		if entry.IsDir() {
			return nil
		}
		if len(resources) >= SkillMaxFiles {
			return errors.New("skill package has too many files")
		}
		raw, err := skillReadFile(view, path, SkillMaxResourceBytes)
		if err != nil {
			return err
		}
		total += int64(len(raw))
		if total > SkillMaxPackageBytes {
			return errors.New("skill package exceeds size limit")
		}
		resources = append(resources, SkillResource{Path: path, Bytes: int64(len(raw)), SHA256: skillDigest(raw)})
		return nil
	})
	return resources, err
}

// walkSkillPackage 在读取目录项时就限制剩余额度，拒绝链接和特殊文件；调用者必须传入受限根的 FS。
func walkSkillPackage(filesystem fs.FS, visit func(string, fs.DirEntry) error) error {
	visited := 0
	var walk func(string) error
	walk = func(directory string) error {
		file, err := filesystem.Open(directory)
		if err != nil {
			return errors.New("open skill package directory failed")
		}
		reader, ok := file.(fs.ReadDirFile)
		if !ok {
			_ = file.Close()
			return errors.New("skill package entry is not a directory")
		}
		remaining := SkillMaxPackageEntries - visited
		entries, readErr := reader.ReadDir(remaining + 1)
		closeErr := file.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
			return errors.New("read skill package directory failed")
		}
		if len(entries) > remaining {
			return errors.New("skill package has too many entries")
		}
		visited += len(entries)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
				return errors.New("skill package links and special files are not supported")
			}
			path := filepath.ToSlash(filepath.Join(directory, entry.Name()))
			if err := visit(path, entry); err != nil {
				return err
			}
			if entry.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(".")
}

// skillDirectoryEntries 有界读取一级目录并排序，避免无效条目耗尽内存或改变冲突优先级。
func skillDirectoryEntries(path string) ([]os.DirEntry, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errors.New("open skill directory failed")
	}
	defer root.Close()
	file, err := root.Open(".")
	if err != nil {
		return nil, errors.New("open skill directory failed")
	}
	defer file.Close()
	entries, err := file.ReadDir(SkillMaxCatalogEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New("read skill directory failed")
	}
	if len(entries) > SkillMaxCatalogEntries {
		return nil, errors.New("skill directory exceeds entry limit")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// skillDigest 为正文、来源和资源生成同一格式的不可逆摘要。
func skillDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// skillContentIdentity 计算回执载荷身份，供重复加载和冷恢复验证使用。
func skillContentIdentity(content SkillContent) (string, error) {
	return canonicaljson.Fingerprint("cairn-skill-content-v1", content)
}

// SkillSummary 生成安全展示的名称、来源与状态，不把描述中的标记解释为控制结构。
func SkillSummary(entry SkillEntry) string {
	return fmt.Sprintf("%s\t%s\tdisabled=%t manual=%t\t%s", entry.Name, entry.Scope, entry.Disabled, entry.ManualOnly, entry.Description)
}
