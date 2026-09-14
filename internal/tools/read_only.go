package tools

import (
	"bytes"
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
	defaultListResults = 100
	maximumListResults = 2000
	defaultReadLimit   = 16 * 1024
	maximumReadLines   = 2000
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
	return ports.ToolSpec{Name: "read_file", Description: "Read a UTF-8 text file from the workspace. Prefer start_line (one-based, from search_text.line) with line_count for local code inspection; provide both and do not mix them with offset or limit. Line reads preserve original newlines, are capped by the workspace byte limit without splitting UTF-8 characters, and return actual byte offset and byte limit. For byte mode, offset is zero-based (default 0, use search_text.byte_offset); limit is a byte count (default up to 16384, capped by the workspace read limit), not a line count. The result includes the complete file sha256; copy it exactly to apply_patch.expected_sha256 before updating or deleting this file.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0,"description":"Zero-based byte offset, not a line number; byte mode only."},"limit":{"type":"integer","minimum":1,"maximum":1048576,"description":"Maximum number of bytes to read, not lines; byte mode only."},"start_line":{"type":"integer","minimum":1,"description":"One-based first line, from search_text.line; requires line_count and excludes offset/limit."},"line_count":{"type":"integer","minimum":1,"maximum":2000,"description":"Maximum lines to read; requires start_line and excludes offset/limit."}}}`)}
}

// Spec 返回 search_text 的受限 JSON 参数定义。
func (tool SearchText) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "search_text", Description: "Search UTF-8 text files in the workspace. Each match includes a one-based line number and a zero-based byte_offset for the start of that line; prefer line as read_file.start_line together with line_count to inspect local code. For byte-mode reads, pass byte_offset to read_file.offset.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1},"path":{"type":"string"},"glob":{"type":"string"},"regex":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":2000}}}`)}
}

// listFilesArguments 是 list_files 的内部解码参数。
type listFilesArguments struct {
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	MaxResults int    `json:"max_results"`
}

// readFileArguments 是 read_file 的内部解码参数。
type readFileArguments struct {
	Path      string `json:"path"`
	Offset    *int64 `json:"offset,omitempty"`
	Limit     *int64 `json:"limit,omitempty"`
	StartLine *int64 `json:"start_line,omitempty"`
	LineCount *int64 `json:"line_count,omitempty"`
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
	arguments := readFileArguments{}
	if err := decodeArguments(raw, &arguments); err != nil {
		return Failure("invalid", err.Error())
	}
	offset, limit := int64(0), defaultLimit
	if arguments.Offset != nil {
		offset = *arguments.Offset
	}
	if arguments.Limit != nil && *arguments.Limit != 0 {
		limit = *arguments.Limit
	}
	if arguments.Path == "" || offset < 0 {
		return Failure("invalid", "path is required and offset cannot be negative")
	}
	lineMode := arguments.StartLine != nil || arguments.LineCount != nil
	if lineMode {
		if arguments.StartLine == nil || arguments.LineCount == nil || arguments.Offset != nil || arguments.Limit != nil {
			return Failure("invalid", "start_line and line_count must be paired and cannot be mixed with offset or limit")
		}
		if *arguments.StartLine < 1 || *arguments.LineCount < 1 || *arguments.LineCount > maximumReadLines {
			return Failure("invalid", fmt.Sprintf("start_line must be positive and line_count must be between 1 and %d", maximumReadLines))
		}
		limit = tool.Workspace.MaxReadBytes
	}
	if limit < 1 || limit > tool.Workspace.MaxReadBytes {
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
	if offset > int64(len(content)) {
		return Failure("invalid", "offset exceeds file length")
	}
	end := offset + min(limit, int64(len(content))-offset)
	if lineMode {
		offset, end = readLineRange(content, *arguments.StartLine, *arguments.LineCount)
		if end-offset > limit {
			end = offset + limit
			for end > offset && !utf8.RuneStart(content[end]) {
				end--
			}
			if end == offset {
				return Failure("invalid", "workspace read limit cannot fit the first UTF-8 character")
			}
		}
		limit = end - offset
	}
	selected := content[offset:end]
	return Success(struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		TotalBytes int64  `json:"total_bytes"`
		Offset     int64  `json:"offset"`
		Limit      int64  `json:"limit"`
	}{Path: relative, Content: string(selected), TotalBytes: int64(len(content)), Offset: offset, Limit: limit}, string(selected), int64(len(content)), "sha256:"+hex.EncodeToString(digest[:]), end < int64(len(content)))
}

// readLineRange 将一基行范围转换为原始字节区间，保留换行并将超出末尾的起点收敛到 EOF。
func readLineRange(content []byte, startLine, lineCount int64) (int64, int64) {
	offset := 0
	for line := int64(1); line < startLine && offset < len(content); line++ {
		next := bytes.IndexByte(content[offset:], '\n')
		if next < 0 {
			offset = len(content)
			break
		}
		offset += next + 1
	}
	end := offset
	for line := int64(0); line < lineCount && end < len(content); line++ {
		next := bytes.IndexByte(content[end:], '\n')
		if next < 0 {
			end = len(content)
			break
		}
		end += next + 1
	}
	return int64(offset), int64(end)
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
	// match 同时提供展示行号与可直接用于读取的行首字节偏移。
	type match struct {
		Path       string `json:"path"`
		Line       int    `json:"line"`
		ByteOffset int    `json:"byte_offset"`
		Preview    string `json:"preview"`
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
		byteOffset := 0
		for index, line := range strings.Split(string(content), "\n") {
			lineOffset := byteOffset
			byteOffset += len(line) + len("\n")
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
			matches = append(matches, match{Path: relative, Line: index + 1, ByteOffset: lineOffset, Preview: preview})
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
