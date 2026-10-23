// git_read.go 提供 git_status 与 git_diff 两个只读工具：只调用 git 的只读子命令，
// 不执行写操作，不经 shell，输出截断后写入 Result。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/ports"
)

// gitReadTimeout 限制单次 git 只读命令的墙钟时间，避免索引异常时拖死循环。
const gitReadTimeout = 10 * time.Second

// gitReadMaxOutput 限制 git 输出写入 Result 的字节上限，防止大仓库 diff 撑爆日志。
const gitReadMaxOutput = 64 * 1024

// GitStatus 读取工作区 git 状态（porcelain + 分支头），不改变仓库。
type GitStatus struct{ Workspace Workspace }

// GitDiff 读取工作区 git 差异（可选 staged 与路径过滤），不改变仓库。
type GitDiff struct{ Workspace Workspace }

// SideEffect 声明 GitStatus 无副作用。
func (GitStatus) SideEffect() SideEffectClass { return SideEffectNone }

// SideEffect 声明 GitDiff 无副作用。
func (GitDiff) SideEffect() SideEffectClass { return SideEffectNone }

// Spec 返回 git_status 的受限 JSON 参数定义。
func (tool GitStatus) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "git_status",
		Description: "Read git status for the workspace without changing the repository. Returns branch header and porcelain status lines (untracked, modified, staged).",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
	}
}

// Spec 返回 git_diff 的受限 JSON 参数定义。
func (tool GitDiff) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "git_diff",
		Description: "Read a unified git diff without changing the repository. Use staged=true for the index, optional paths to limit files. Output is truncated at the workspace tool limit.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"staged":{"type":"boolean"},"paths":{"type":"array","maxItems":32,"items":{"type":"string","minLength":1}}}}`),
	}
}

// gitStatusResult 是 git_status 的稳定数据形状。
type gitStatusResult struct {
	Branch     string   `json:"branch,omitempty"`
	Upstream   string   `json:"upstream,omitempty"`
	Ahead      int      `json:"ahead,omitempty"`
	Behind     int      `json:"behind,omitempty"`
	Clean      bool     `json:"clean"`
	Entries    []string `json:"entries"`
	Truncated  bool     `json:"truncated"`
	GitVersion string   `json:"git_version,omitempty"`
}

// gitDiffResult 是 git_diff 的稳定数据形状。
type gitDiffResult struct {
	Staged    bool     `json:"staged"`
	Paths     []string `json:"paths,omitempty"`
	Diff      string   `json:"diff"`
	Truncated bool     `json:"truncated"`
	Bytes     int      `json:"bytes"`
}

// Execute 运行只读 git status 并返回结构化结果。
func (tool GitStatus) Execute(ctx context.Context, raw json.RawMessage) Result {
	if len(raw) > 0 && string(raw) != "null" {
		var args struct{}
		if err := json.Unmarshal(raw, &args); err != nil {
			return Failure("invalid_arguments", "git_status does not accept arguments")
		}
	}
	version, err := gitVersion(ctx, tool.Workspace.Root)
	if err != nil {
		return Failure("git_unavailable", err.Error())
	}
	output, err := runGitRead(ctx, tool.Workspace.Root, gitReadMaxOutput, "status", "--porcelain=v1", "--branch", "--untracked-files=normal")
	if err != nil {
		return Failure("git_status_failed", err.Error())
	}
	result := gitStatusResult{Entries: []string{}, GitVersion: version, Truncated: output.truncated}
	for _, line := range strings.Split(output.text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			parseGitBranchHeader(line[3:], &result)
			continue
		}
		if len(result.Entries) < 2000 {
			result.Entries = append(result.Entries, line)
		} else {
			result.Truncated = true
		}
	}
	result.Clean = len(result.Entries) == 0
	preview := "clean"
	if !result.Clean {
		preview = fmt.Sprintf("%d change entr%s", len(result.Entries), pluralY(len(result.Entries)))
	}
	return Success(result, preview, int64(output.bytes), "", output.truncated)
}

// gitDiffArguments 是 git_diff 的内部解码参数。
type gitDiffArguments struct {
	Staged bool     `json:"staged"`
	Paths  []string `json:"paths"`
}

// Execute 运行只读 git diff 并返回截断后的统一差异文本。
func (tool GitDiff) Execute(ctx context.Context, raw json.RawMessage) Result {
	var args gitDiffArguments
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return Failure("invalid_arguments", "git_diff arguments must be an object with optional staged and paths")
		}
	}
	if len(args.Paths) > 32 {
		return Failure("invalid_arguments", "git_diff paths accepts at most 32 entries")
	}
	argv := []string{"diff", "--no-color", "--no-ext-diff"}
	if args.Staged {
		argv = append(argv, "--cached")
	}
	if len(args.Paths) > 0 {
		argv = append(argv, "--")
		for _, path := range args.Paths {
			if strings.TrimSpace(path) == "" || strings.Contains(path, "..") {
				return Failure("invalid_arguments", "git_diff paths must be workspace-relative without ..")
			}
			argv = append(argv, path)
		}
	}
	output, err := runGitRead(ctx, tool.Workspace.Root, gitReadMaxOutput, argv...)
	if err != nil {
		return Failure("git_diff_failed", err.Error())
	}
	data := gitDiffResult{Staged: args.Staged, Paths: args.Paths, Diff: output.text, Truncated: output.truncated, Bytes: output.bytes}
	preview := "no diff"
	if strings.TrimSpace(output.text) != "" {
		preview = fmt.Sprintf("diff %d bytes", output.bytes)
		if output.truncated {
			preview += " (truncated)"
		}
	}
	return Success(data, preview, int64(output.bytes), "", output.truncated)
}

// gitCommandOutput 保存一次只读 git 命令的文本输出与截断标记。
type gitCommandOutput struct {
	text      string
	bytes     int
	truncated bool
}

// gitVersion 在工作区解析 git 版本；失败时返回可读错误供工具失败关闭。
func gitVersion(ctx context.Context, dir string) (string, error) {
	versionCtx, cancel := context.WithTimeout(ctx, gitReadTimeout)
	defer cancel()
	command := exec.CommandContext(versionCtx, "git", "version")
	command.Dir = dir
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", errors.New("git is required for git_status/git_diff: " + detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runGitRead 以 argv 方式运行 git 只读子命令，按字节上限截断输出。
func runGitRead(ctx context.Context, dir string, limit int, args ...string) (gitCommandOutput, error) {
	if len(args) == 0 {
		return gitCommandOutput{}, errors.New("git arguments are required")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return gitCommandOutput{}, errors.New("git argument contains NUL")
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, gitReadTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, "git", args...)
	command.Dir = dir
	var stdout bytes.Buffer
	var stderr limitedBuffer
	stderr.limit = 4096
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return gitCommandOutput{}, fmt.Errorf("git %s timed out: %s", args[0], detail)
		}
		return gitCommandOutput{}, fmt.Errorf("git %s failed: %s", args[0], detail)
	}
	text := stdout.String()
	out := gitCommandOutput{bytes: stdout.Len()}
	if out.bytes > limit {
		text = text[:limit] + "\n...[preview-truncated]"
		out.truncated = true
	}
	out.text = string(artifact.Redact([]byte(text)))
	return out, nil
}

// parseGitBranchHeader 解析 `git status --branch` 的分支行（含 ahead/behind）。
func parseGitBranchHeader(header string, result *gitStatusResult) {
	header = strings.TrimSpace(header)
	if header == "" || strings.HasPrefix(header, "HEAD (no branch)") {
		result.Branch = "HEAD"
		return
	}
	parts := strings.SplitN(header, "...", 2)
	result.Branch = strings.TrimSpace(parts[0])
	if len(parts) != 2 {
		return
	}
	tracking := strings.TrimSpace(parts[1])
	open := strings.IndexByte(tracking, '[')
	if open < 0 {
		result.Upstream = tracking
		return
	}
	result.Upstream = strings.TrimSpace(tracking[:open])
	status := tracking[open:]
	result.Ahead = parseGitTrackingCount(status, "ahead ")
	result.Behind = parseGitTrackingCount(status, "behind ")
}

// parseGitTrackingCount 从 `[ahead N, behind M]` 片段读取计数；缺失为 0。
func parseGitTrackingCount(status, prefix string) int {
	idx := strings.Index(status, prefix)
	if idx < 0 {
		return 0
	}
	rest := status[idx+len(prefix):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	value := 0
	for _, ch := range rest[:end] {
		value = value*10 + int(ch-'0')
	}
	return value
}

// pluralY 返回 y/ies 词尾，保持预览文案稳定可测。
func pluralY(count int) string {
	if count == 1 {
		return "y"
	}
	return "ies"
}
