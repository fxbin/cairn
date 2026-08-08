package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/ports"
)

const (
	defaultListResults = 200
	maximumListResults = 2000
	defaultReadLimit   = 64 * 1024
)

// ListFiles 是列出工作区文件的只读工具。
type ListFiles struct{ Workspace Workspace }

// ReadFile 是读取 UTF-8 工作区文件的只读工具。
type ReadFile struct{ Workspace Workspace }

// SearchText 是在 UTF-8 工作区文件内检索文本的只读工具。
type SearchText struct{ Workspace Workspace }

// SideEffect 声明 ListFiles 不产生副作用。
func (ListFiles) SideEffect() SideEffectClass { return SideEffectNone }

// SideEffect 声明 ReadFile 不产生副作用。
func (ReadFile) SideEffect() SideEffectClass { return SideEffectNone }

// SideEffect 声明 SearchText 不产生副作用。
func (SearchText) SideEffect() SideEffectClass { return SideEffectNone }

// Spec 返回 list_files 的受限 JSON 参数定义。
func (tool ListFiles) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "list_files", Description: "List files below a workspace-relative path.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"},"glob":{"type":"string"},"max_results":{"type":"integer","minimum":1,"maximum":2000}}}`)}
}

// Spec 返回 read_file 的受限 JSON 参数定义。
func (tool ReadFile) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "read_file", Description: "Read a UTF-8 text file from the workspace.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":1048576}}}`)}
}

// Spec 返回 search_text 的受限 JSON 参数定义。
func (tool SearchText) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "search_text", Description: "Search UTF-8 text files in the workspace.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1},"path":{"type":"string"},"glob":{"type":"string"},"regex":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":2000}}}`)}
}

// listFilesArguments 是 list_files 的内部解码参数。
type listFilesArguments struct {
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	MaxResults int    `json:"max_results"`
}

// readFileArguments 是 read_file 的内部解码参数。
type readFileArguments struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Limit  int64  `json:"limit"`
}

// searchTextArguments 是 search_text 的内部解码参数。
type searchTextArguments struct {
	Query      string `json:"query"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	Regex      bool   `json:"regex"`
	MaxResults int    `json:"max_results"`
}

// Execute 枚举受工作区边界、glob 和结果上限约束的文件路径。
func (tool ListFiles) Execute(ctx context.Context, raw json.RawMessage) Result {
	arguments := listFilesArguments{Path: ".", MaxResults: defaultListResults}
	if err := decodeArguments(raw, &arguments); err != nil {
		return Failure("invalid", err.Error())
	}
	if arguments.MaxResults == 0 {
		arguments.MaxResults = defaultListResults
	}
	if arguments.MaxResults < 1 || arguments.MaxResults > maximumListResults {
		return Failure("invalid", "max_results must be between 1 and 2000")
	}
	if err := validateGlob(arguments.Glob); err != nil {
		return Failure("invalid", err.Error())
	}
	files := make([]string, 0, arguments.MaxResults)
	truncated := false
	err := tool.Workspace.walk(arguments.Path, func(_ string, entry os.DirEntry, relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !matchesGlob(relative, arguments.Glob) {
			return nil
		}
		if len(files) >= arguments.MaxResults {
			truncated = true
			return io.EOF
		}
		files = append(files, relative)
		return nil
	})
	if err != nil && err != io.EOF {
		return Failure("io", err.Error())
	}
	return Success(struct {
		Files []string `json:"files"`
	}{Files: files}, fmt.Sprintf("%d file(s)", len(files)), int64(len(files)), "", truncated)
}

// Execute 在读取大小和 UTF-8 限制内返回文件片段及其完整内容摘要。
func (tool ReadFile) Execute(ctx context.Context, raw json.RawMessage) Result {
	defaultLimit := int64(defaultReadLimit)
	if tool.Workspace.MaxReadBytes < defaultLimit {
		defaultLimit = tool.Workspace.MaxReadBytes
	}
	arguments := readFileArguments{Limit: defaultLimit}
	if err := decodeArguments(raw, &arguments); err != nil {
		return Failure("invalid", err.Error())
	}
	if arguments.Path == "" || arguments.Offset < 0 {
		return Failure("invalid", "path is required and offset cannot be negative")
	}
	if arguments.Limit == 0 {
		arguments.Limit = defaultLimit
	}
	if arguments.Limit < 1 || arguments.Limit > tool.Workspace.MaxReadBytes {
		return Failure("invalid", fmt.Sprintf("limit must be between 1 and %d", tool.Workspace.MaxReadBytes))
	}
	filePath, relative, err := tool.Workspace.Resolve(arguments.Path)
	if err != nil {
		return Failure("invalid_path", err.Error())
	}
	if tool.Workspace.Ignore(relative) {
		return Failure("ignored_path", "path is ignored")
	}
	if err := ctx.Err(); err != nil {
		return Failure("cancelled", err.Error())
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		return Failure("io", err.Error())
	}
	if !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return Failure("unsupported_binary", "read_file only supports UTF-8 text")
	}
	digest := sha256.Sum256(content)
	if arguments.Offset > int64(len(content)) {
		return Failure("invalid", "offset exceeds file length")
	}
	end := arguments.Offset + arguments.Limit
	if end > int64(len(content)) {
		end = int64(len(content))
	}
	selected := content[arguments.Offset:end]
	return Success(struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		TotalBytes int64  `json:"total_bytes"`
		Offset     int64  `json:"offset"`
		Limit      int64  `json:"limit"`
	}{Path: relative, Content: string(selected), TotalBytes: int64(len(content)), Offset: arguments.Offset, Limit: arguments.Limit}, string(selected), int64(len(content)), "sha256:"+hex.EncodeToString(digest[:]), end < int64(len(content)))
}

// Execute 在扫描文件数、扫描字节数和匹配数上限内搜索文本或正则表达式。
func (tool SearchText) Execute(ctx context.Context, raw json.RawMessage) Result {
	arguments := searchTextArguments{Path: ".", MaxResults: 100}
	if err := decodeArguments(raw, &arguments); err != nil {
		return Failure("invalid", err.Error())
	}
	if arguments.Query == "" {
		return Failure("invalid", "query is required")
	}
	if arguments.MaxResults == 0 {
		arguments.MaxResults = 100
	}
	if arguments.MaxResults < 1 || arguments.MaxResults > maximumListResults {
		return Failure("invalid", "max_results must be between 1 and 2000")
	}
	if err := validateGlob(arguments.Glob); err != nil {
		return Failure("invalid", err.Error())
	}
	var expression *regexp.Regexp
	if arguments.Regex {
		compiled, err := regexp.Compile(arguments.Query)
		if err != nil {
			return Failure("invalid", "invalid regex: "+err.Error())
		}
		expression = compiled
	}
	type match struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Preview string `json:"preview"`
	}
	matches := make([]match, 0, arguments.MaxResults)
	var scannedBytes int64
	var scannedFiles int
	truncated := false
	err := tool.Workspace.walk(arguments.Path, func(filePath string, entry os.DirEntry, relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !matchesGlob(relative, arguments.Glob) {
			return nil
		}
		if scannedFiles >= tool.Workspace.MaxSearchFiles || scannedBytes >= tool.Workspace.MaxSearchBytes || len(matches) >= arguments.MaxResults {
			truncated = true
			return io.EOF
		}
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil
		}
		scannedFiles++
		scannedBytes += int64(len(content))
		if strings.IndexByte(string(content), 0) >= 0 || !utf8.Valid(content) {
			return nil
		}
		for index, line := range strings.Split(string(content), "\n") {
			found := strings.Contains(line, arguments.Query)
			if expression != nil {
				found = expression.MatchString(line)
			}
			if !found {
				continue
			}
			if len(matches) >= arguments.MaxResults {
				truncated = true
				return io.EOF
			}
			preview := line
			if len(preview) > 400 {
				preview = preview[:400]
			}
			matches = append(matches, match{Path: relative, Line: index + 1, Preview: preview})
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return Failure("io", err.Error())
	}
	return Success(struct {
		Matches      []match `json:"matches"`
		ScannedFiles int     `json:"scanned_files"`
		ScannedBytes int64   `json:"scanned_bytes"`
	}{Matches: matches, ScannedFiles: scannedFiles, ScannedBytes: scannedBytes}, fmt.Sprintf("%d match(es)", len(matches)), scannedBytes, "", truncated)
}

// decodeArguments 严格解码单个 JSON 参数对象并拒绝未知字段。
func decodeArguments(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if decoder.More() {
		return fmt.Errorf("invalid arguments: multiple JSON values")
	}
	return nil
}

// validateGlob 验证可选 glob 符合 path.Match 语法。
func validateGlob(glob string) error {
	if glob == "" {
		return nil
	}
	_, err := path.Match(glob, "")
	if err != nil {
		return fmt.Errorf("invalid glob: %w", err)
	}
	return nil
}

// matchesGlob 判断相对路径是否匹配 glob，并兼容以 **/ 开头的常用模式。
func matchesGlob(relative, glob string) bool {
	if glob == "" {
		return true
	}
	matched, err := path.Match(glob, relative)
	if err == nil && matched {
		return true
	}
	if strings.HasPrefix(glob, "**/") {
		matched, _ = path.Match(strings.TrimPrefix(glob, "**/"), path.Base(relative))
		return matched
	}
	return false
}
