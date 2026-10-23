// Package tools 提供声明式提示模板：显式参数、锁定摘要、不执行 shell 插值。
// Author: fxbin
package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/lock"
	"gopkg.in/yaml.v3"
)

// 模板限制约束单文件大小、参数个数与正文长度，避免无界展开。
const (
	TemplateContentVersion      = 1
	TemplateMaxNameCharacters   = 64
	TemplateMaxBodyBytes        = 64 * 1024
	TemplateMaxDescriptionBytes = 512
	TemplateMaxParameters       = 32
	TemplateMaxRenderBytes      = 256 * 1024
)

// templateNamePattern 与技能同源：可移植 ASCII 小写名称。
var templateNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// templatePlaceholder 匹配 {{param}} 形式，不解析任何脚本语法。
var templatePlaceholder = regexp.MustCompile(`\{\{\s*([a-z0-9_]+)\s*\}\}`)

// TemplateParameter 声明一个必填或可选参数。
type TemplateParameter struct {
	Name     string `json:"name" yaml:"name"`
	Required bool   `json:"required" yaml:"required"`
	Hint     string `json:"hint,omitempty" yaml:"hint,omitempty"`
}

// TemplateDocument 是单个提示模板的可展示投影；正文不含宿主机路径。
type TemplateDocument struct {
	Version     int                 `json:"version"`
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Parameters  []TemplateParameter `json:"parameters"`
	Body        string              `json:"body"`
	SHA256      string              `json:"sha256"`
	SourcePath  string              `json:"source_path,omitempty"`
}

// TemplateCatalog 是目录发现结果；诊断不回显原始 YAML 错误正文。
type TemplateCatalog struct {
	Entries     []TemplateDocument   `json:"entries"`
	Diagnostics []TemplateDiagnostic `json:"diagnostics,omitempty"`
}

// TemplateDiagnostic 记录无效模板条目。
type TemplateDiagnostic struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

// templateFrontmatter 解析模板头部，只解释已知字段。
type templateFrontmatter struct {
	Name        string              `yaml:"name"`
	Description string              `yaml:"description"`
	Parameters  []TemplateParameter `yaml:"parameters"`
}

// ValidTemplateName 校验模板名称可移植且无路径字符。
func ValidTemplateName(name string) bool {
	return len(name) <= TemplateMaxNameCharacters && templateNamePattern.MatchString(name)
}

// TemplateDigest 计算正文摘要，供渲染结果绑定来源。
func TemplateDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// parseTemplate 严格解析 YAML 头与正文，拒绝未知占位符集合以外的脚本语法。
func parseTemplate(raw []byte, fallbackName string) (TemplateDocument, error) {
	if len(raw) == 0 || len(raw) > TemplateMaxBodyBytes || !utf8.Valid(raw) {
		return TemplateDocument{}, errors.New("invalid template text or size")
	}
	text := string(raw)
	header, body := splitTemplateDocument(text)
	var front templateFrontmatter
	if header != "" {
		if err := yaml.Unmarshal([]byte(header), &front); err != nil {
			return TemplateDocument{}, errors.New("invalid template frontmatter")
		}
	}
	name := strings.TrimSpace(front.Name)
	if name == "" {
		name = strings.TrimSpace(fallbackName)
	}
	if !ValidTemplateName(name) {
		return TemplateDocument{}, errors.New("invalid template name")
	}
	description := strings.TrimSpace(front.Description)
	if len(description) > TemplateMaxDescriptionBytes {
		return TemplateDocument{}, errors.New("template description too long")
	}
	parameters := front.Parameters
	if len(parameters) > TemplateMaxParameters {
		return TemplateDocument{}, errors.New("too many template parameters")
	}
	for index := range parameters {
		parameters[index].Name = strings.TrimSpace(parameters[index].Name)
		if !validTemplateParameterName(parameters[index].Name) {
			return TemplateDocument{}, errors.New("invalid template parameter name")
		}
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return TemplateDocument{}, errors.New("template body is required")
	}
	declared := make(map[string]struct{}, len(parameters))
	for _, parameter := range parameters {
		if _, exists := declared[parameter.Name]; exists {
			return TemplateDocument{}, errors.New("duplicate template parameter")
		}
		declared[parameter.Name] = struct{}{}
	}
	for _, match := range templatePlaceholder.FindAllStringSubmatch(body, -1) {
		if _, ok := declared[match[1]]; !ok {
			return TemplateDocument{}, fmt.Errorf("template references undeclared parameter %q", match[1])
		}
	}
	return TemplateDocument{
		Version:     TemplateContentVersion,
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Body:        body,
		SHA256:      TemplateDigest(body),
	}, nil
}

// splitTemplateDocument 分离 YAML 头与正文；无头时整篇作为正文。
func splitTemplateDocument(text string) (header string, body string) {
	if !strings.HasPrefix(text, "---\n") {
		return "", text
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", text
	}
	return rest[:end], strings.TrimPrefix(rest[end+len("\n---"):], "\n")
}

// validTemplateParameterName 仅允许 snake_case 风格标识。
func validTemplateParameterName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// RenderTemplate 按显式参数替换 {{param}}，缺参/多参失败关闭，禁止 shell 与环境展开。
func RenderTemplate(document TemplateDocument, values map[string]string) (string, error) {
	if document.Body == "" {
		return "", errors.New("template body is required")
	}
	if len(values) > TemplateMaxParameters {
		return "", errors.New("too many template parameter values")
	}
	declared := make(map[string]TemplateParameter, len(document.Parameters))
	for _, parameter := range document.Parameters {
		declared[parameter.Name] = parameter
	}
	for key := range values {
		if _, ok := declared[key]; !ok {
			return "", fmt.Errorf("unknown template parameter %q", key)
		}
	}
	for _, parameter := range document.Parameters {
		if parameter.Required {
			if strings.TrimSpace(values[parameter.Name]) == "" {
				return "", fmt.Errorf("missing required template parameter %q", parameter.Name)
			}
		}
	}
	var builder strings.Builder
	last := 0
	for _, match := range templatePlaceholder.FindAllStringSubmatchIndex(document.Body, -1) {
		name := document.Body[match[2]:match[3]]
		literal := document.Body[last:match[0]]
		if len(literal) > TemplateMaxRenderBytes-builder.Len() || len(values[name]) > TemplateMaxRenderBytes-builder.Len()-len(literal) {
			return "", errors.New("rendered template exceeds size limit")
		}
		builder.WriteString(literal)
		builder.WriteString(values[name])
		last = match[1]
	}
	if len(document.Body[last:]) > TemplateMaxRenderBytes-builder.Len() {
		return "", errors.New("rendered template exceeds size limit")
	}
	builder.WriteString(document.Body[last:])
	rendered := builder.String()
	if len(rendered) > TemplateMaxRenderBytes {
		return "", errors.New("rendered template exceeds size limit")
	}
	return rendered, nil
}

// TemplateStore 管理显式本地模板目录；只读列表与原子写入，不执行任何模板脚本。
type TemplateStore struct {
	Root string
}

// NewTemplateStore 打开指定模板根目录；调用方负责确定用户/工作区作用域。
func NewTemplateStore(root string) (TemplateStore, error) {
	if strings.TrimSpace(root) == "" {
		return TemplateStore{}, errors.New("template store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return TemplateStore{}, errors.New("resolve template store failed")
	}
	return TemplateStore{Root: abs}, nil
}

// List 扫描 *.md 模板；无效条目进入诊断，不阻断其余发现。
func (store TemplateStore) List() (TemplateCatalog, error) {
	entries, err := os.ReadDir(store.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TemplateCatalog{Entries: []TemplateDocument{}}, nil
		}
		return TemplateCatalog{}, errors.New("read template store failed")
	}
	catalog := TemplateCatalog{Entries: []TemplateDocument{}}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		raw, readErr := store.read(entry.Name())
		if readErr != nil {
			catalog.Diagnostics = append(catalog.Diagnostics, TemplateDiagnostic{Name: name, Code: "unreadable"})
			continue
		}
		document, parseErr := parseTemplate(raw, name)
		if parseErr != nil {
			catalog.Diagnostics = append(catalog.Diagnostics, TemplateDiagnostic{Name: name, Code: "invalid"})
			continue
		}
		document.SourcePath = filepath.Join(store.Root, entry.Name())
		catalog.Entries = append(catalog.Entries, document)
	}
	sort.Slice(catalog.Entries, func(i, j int) bool {
		return catalog.Entries[i].Name < catalog.Entries[j].Name
	})
	return catalog, nil
}

// Load 读取单个模板；名称非法或缺失失败关闭。
func (store TemplateStore) Load(name string) (TemplateDocument, error) {
	if !ValidTemplateName(name) {
		return TemplateDocument{}, errors.New("invalid template name")
	}
	path := filepath.Join(store.Root, name+".md")
	raw, err := store.read(name + ".md")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TemplateDocument{}, errors.New("template not found")
		}
		return TemplateDocument{}, errors.New("read template failed")
	}
	document, err := parseTemplate(raw, name)
	if err != nil {
		return TemplateDocument{}, err
	}
	document.SourcePath = path
	return document, nil
}

// read 复用工作区路径与普通文件容量边界，避免目录外链接和无界读取。
func (store TemplateStore) read(relative string) ([]byte, error) {
	view, err := NewWorkspace(store.Root, nil, TemplateMaxBodyBytes, 0, 0)
	if err != nil {
		return nil, errors.New("open template store failed")
	}
	return skillReadFile(view, relative, TemplateMaxBodyBytes)
}

// Save 原子写入模板；已存在名称失败关闭，不覆盖用户内容。
func (store TemplateStore) Save(document TemplateDocument) error {
	if !ValidTemplateName(document.Name) {
		return errors.New("invalid template name")
	}
	if err := os.MkdirAll(store.Root, 0o700); err != nil {
		return errors.New("create template store failed")
	}
	guard, err := lock.Acquire(filepath.Join(store.Root, ".cairn-templates.lock"))
	if err != nil {
		return errors.New("template store is locked or unavailable")
	}
	defer func() { _ = guard.Release() }()
	target := filepath.Join(store.Root, document.Name+".md")
	if _, err := os.Lstat(target); err == nil {
		return errors.New("template already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect template target failed")
	}
	header, err := yaml.Marshal(templateFrontmatter{
		Name:        document.Name,
		Description: document.Description,
		Parameters:  document.Parameters,
	})
	if err != nil {
		return errors.New("encode template frontmatter failed")
	}
	var builder strings.Builder
	builder.WriteString("---\n")
	builder.Write(header)
	builder.WriteString("---\n\n")
	builder.WriteString(document.Body)
	builder.WriteString("\n")
	raw := []byte(builder.String())
	if _, err := parseTemplate(raw, document.Name); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(store.Root, ".template-*.tmp")
	if err != nil {
		return errors.New("create template temporary failed")
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return errors.New("write template failed")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("sync template failed")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close template failed")
	}
	if err := os.Chmod(temporaryName, 0o600); err != nil {
		return errors.New("set template permissions failed")
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return errors.New("publish template failed")
	}
	return syncDirectory(store.Root)
}

// TemplateSummary 返回单行可读摘要。
func TemplateSummary(document TemplateDocument) string {
	hints := make([]string, 0, len(document.Parameters))
	for _, parameter := range document.Parameters {
		mark := "?"
		if parameter.Required {
			mark = "!"
		}
		hints = append(hints, parameter.Name+mark)
	}
	if len(hints) == 0 {
		return fmt.Sprintf("%s  (no parameters)  %s", document.Name, document.SHA256)
	}
	return fmt.Sprintf("%s  (%s)  %s", document.Name, strings.Join(hints, ","), document.SHA256)
}
