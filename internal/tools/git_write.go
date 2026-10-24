// git_write.go 提供 git_commit 受控提交工具：仅 stage 明确路径并创建提交，
// 不执行任意 git 子命令；策略层按 workspace 写处理。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// gitCommitTimeout 限制提交命令墙钟。
const gitCommitTimeout = 30 * time.Second

// GitCommit 仅允许 `git add <paths> && git commit -m` 语义的最小写工具。
type GitCommit struct{ Workspace Workspace }

// SideEffect 声明 git_commit 会改变工作区 Git 状态。
func (GitCommit) SideEffect() SideEffectClass { return SideEffectWorkspace }

// Spec 返回 git_commit 的受限 JSON 参数定义。
func (tool GitCommit) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "git_commit",
		Description: "Create a git commit from explicit workspace-relative paths only (git add <paths> + git commit -m). Does not support flags, hooks bypass, or amend. Requires approval like other writes.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["message","paths"],"properties":{"message":{"type":"string","minLength":1},"paths":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"string","minLength":1}}}}`),
	}
}

// gitCommitResult 是 git_commit 稳定结果。
type gitCommitResult struct {
	Commit  string   `json:"commit,omitempty"`
	Paths   []string `json:"paths"`
	Message string   `json:"message"`
}

// Execute 执行最小提交；路径含 .. 或绝对路径失败关闭。
func (tool GitCommit) Execute(ctx context.Context, raw json.RawMessage) Result {
	var args struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return Failure("invalid_arguments", "git_commit requires message and paths")
	}
	if strings.TrimSpace(args.Message) == "" || len(args.Paths) == 0 {
		return Failure("invalid_arguments", "message and paths are required")
	}
	if strings.HasPrefix(args.Message, "-") {
		return Failure("invalid_arguments", "message must not look like a git flag")
	}
	relative := make([]string, 0, len(args.Paths))
	for _, item := range args.Paths {
		if strings.TrimSpace(item) == "" || strings.Contains(item, "..") || strings.HasPrefix(item, "/") {
			return Failure("invalid_arguments", "paths must be workspace-relative without ..")
		}
		_, rel, err := tool.Workspace.Resolve(item)
		if err != nil {
			return Failure("invalid_path", err.Error())
		}
		relative = append(relative, rel)
	}
	if _, err := gitVersion(ctx, tool.Workspace.Root); err != nil {
		return Failure("git_unavailable", err.Error())
	}
	addArgs := append([]string{"add", "--"}, relative...)
	if _, err := runGitRead(ctx, tool.Workspace.Root, 4096, addArgs...); err != nil {
		return Failure("git_add_failed", err.Error())
	}
	commitArgs := []string{"commit", "-m", args.Message}
	output, err := runGitRead(ctx, tool.Workspace.Root, 8192, commitArgs...)
	if err != nil {
		return Failure("git_commit_failed", err.Error())
	}
	revOutput, revErr := runGitRead(ctx, tool.Workspace.Root, 256, "rev-parse", "HEAD")
	commitID := ""
	if revErr == nil {
		commitID = strings.TrimSpace(revOutput.text)
	}
	data := gitCommitResult{Commit: commitID, Paths: relative, Message: args.Message}
	preview := "committed " + commitID
	if commitID == "" {
		preview = "commit created"
	}
	return Success(data, preview, int64(len(output.text)), "", false)
}

// GitLog 读取有限条数的提交摘要。
type GitLog struct{ Workspace Workspace }

// SideEffect 声明 GitLog 无副作用。
func (GitLog) SideEffect() SideEffectClass { return SideEffectNone }

// Spec 返回 git_log 的受限 JSON 参数定义。
func (tool GitLog) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "git_log",
		Description: "Read recent git commits (oneline, limited count) without changing the repository.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"limit":{"type":"integer","minimum":1,"maximum":100}}}`),
	}
}

// Execute 返回 oneline 日志。
func (tool GitLog) Execute(ctx context.Context, raw json.RawMessage) Result {
	var args struct {
		Limit int `json:"limit"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return Failure("invalid_arguments", "git_log arguments invalid")
		}
	}
	if args.Limit <= 0 {
		args.Limit = 20
	}
	if args.Limit > 100 {
		args.Limit = 100
	}
	output, err := runGitRead(ctx, tool.Workspace.Root, gitReadMaxOutput,
		"log", fmt.Sprintf("-%d", args.Limit), "--oneline", "--no-decorate")
	if err != nil {
		return Failure("git_log_failed", err.Error())
	}
	return Success(map[string]any{"limit": args.Limit, "log": output.text}, "read log", int64(output.bytes), "", output.truncated)
}
