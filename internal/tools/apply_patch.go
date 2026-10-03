package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/ports"
)

// ApplyPatch 是经审批后执行确定性单文件改写的副作用工具。
type ApplyPatch struct{ Workspace Workspace }

// Replacement 描述一次必须恰好匹配指定次数的文本替换；省略 Occurrence 时安全地按一次处理。
type Replacement struct {
	Old        string `json:"old"`
	New        string `json:"new"`
	Occurrence int    `json:"occurrence"`
}

// PatchArguments 是模型请求的单文件改写参数。
type PatchArguments struct {
	Path           string        `json:"path"`
	Mode           string        `json:"mode"`
	ExpectedSHA256 string        `json:"expected_sha256"`
	Content        *string       `json:"content,omitempty"`
	Replacements   []Replacement `json:"replacements,omitempty"`
}

// PatchPlan 是审批与执行共用的确定性改写计划及前后摘要。
type PatchPlan struct {
	Path       string
	Mode       string
	BeforeHash string
	AfterHash  string
	Before     []byte
	After      []byte
	Exists     bool
}

// CodeStalePrecondition 标识 Patch 的已给定摘要已过期；调用方应先重新读取文件，
// 而不是盲目重试或把该安全前置条件错误误报为策略拒绝。
const CodeStalePrecondition = "stale_precondition"

// PatchPreconditionError 描述 update 或 delete 的已给定摘要与当前文件不一致；
// 只携带相对路径和摘要，绝不携带文件内容，供上层生成可重放的恢复反馈。
type PatchPreconditionError struct {
	Path           string
	ExpectedSHA256 string
	ActualSHA256   string
}

// Error 返回不含文件内容的稳定前置条件失败说明。
func (err *PatchPreconditionError) Error() string {
	return "expected_sha256 does not match current file; read_file is required before retrying apply_patch"
}

// SideEffect 声明 ApplyPatch 会改变工作区内容。
func (tool ApplyPatch) SideEffect() SideEffectClass { return SideEffectWorkspace }

// Spec 返回 apply_patch 的受限 JSON 参数定义。
func (tool ApplyPatch) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "apply_patch", Description: "Apply a deterministic, single-file patch after approval. expected_sha256 is required: for update/delete copy the exact sha256 returned by a fresh read_file; for create use absent. A replacement without occurrence must match exactly once.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path","mode","expected_sha256"],"properties":{"path":{"type":"string"},"mode":{"type":"string","enum":["create","update","delete"]},"expected_sha256":{"type":"string","minLength":1},"content":{"type":"string"},"replacements":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["old","new"],"properties":{"old":{"type":"string"},"new":{"type":"string"},"occurrence":{"type":"integer","minimum":1}}}}}}`)}
}

// Plan 在不写入文件的前提下校验参数并计算确定性的改写前后状态。
func (tool ApplyPatch) Plan(raw json.RawMessage) (PatchPlan, error) {
	arguments := PatchArguments{}
	if err := decodeArguments(raw, &arguments); err != nil {
		return PatchPlan{}, err
	}
	if arguments.Path == "" {
		return PatchPlan{}, errors.New("path is required")
	}
	filePath, relative, err := tool.Workspace.Resolve(arguments.Path)
	if err != nil {
		return PatchPlan{}, err
	}
	if tool.Workspace.Ignore(relative) {
		return PatchPlan{}, errors.New("path is ignored")
	}
	content, err := os.ReadFile(filePath)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return PatchPlan{}, err
	}
	plan := PatchPlan{Path: relative, Mode: arguments.Mode, Before: content, Exists: exists, BeforeHash: "absent"}
	if exists {
		plan.BeforeHash = sha256Reference(content)
	}
	switch arguments.Mode {
	case "create":
		if exists {
			return PatchPlan{}, errors.New("create target already exists")
		}
		if arguments.Content == nil || len(arguments.Replacements) != 0 {
			return PatchPlan{}, errors.New("invalid_patch: mode=create requires content and must omit replacements; retry with only content")
		}
		plan.After = []byte(*arguments.Content)
	case "update":
		if !exists || arguments.Content != nil || len(arguments.Replacements) == 0 {
			// 拒绝后反馈必须可执行：明确互斥参数与唯一合法重提形状。
			return PatchPlan{}, errors.New("invalid_patch: mode=update requires an existing file and replacements, and must omit content; retry with only replacements after a fresh read_file")
		}
		if strings.TrimSpace(arguments.ExpectedSHA256) == "" {
			return PatchPlan{}, errors.New("invalid_patch: mode=update requires expected_sha256 copied from read_file; retry after a fresh read_file")
		}
		if !sameSHA256(arguments.ExpectedSHA256, plan.BeforeHash) {
			return PatchPlan{}, &PatchPreconditionError{Path: relative, ExpectedSHA256: arguments.ExpectedSHA256, ActualSHA256: plan.BeforeHash}
		}
		updated, err := applyReplacements(content, arguments.Replacements)
		if err != nil {
			return PatchPlan{}, err
		}
		plan.After = updated
	case "delete":
		if !exists || arguments.Content != nil || len(arguments.Replacements) != 0 {
			return PatchPlan{}, errors.New("invalid_patch: mode=delete requires an existing file and must omit content and replacements; retry with only path/mode/expected_sha256")
		}
		if strings.TrimSpace(arguments.ExpectedSHA256) == "" {
			return PatchPlan{}, errors.New("invalid_patch: mode=delete requires expected_sha256 copied from read_file; retry after a fresh read_file")
		}
		if !sameSHA256(arguments.ExpectedSHA256, plan.BeforeHash) {
			return PatchPlan{}, &PatchPreconditionError{Path: relative, ExpectedSHA256: arguments.ExpectedSHA256, ActualSHA256: plan.BeforeHash}
		}
		plan.AfterHash = "absent"
	default:
		return PatchPlan{}, errors.New("invalid_patch: mode must be create, update, or delete")
	}
	if arguments.Mode != "delete" {
		plan.AfterHash = sha256Reference(plan.After)
	}
	return plan, nil
}

// Execute 按计划原子地创建、更新或删除单个文件，并校验写后摘要。
func (tool ApplyPatch) Execute(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Failure("cancelled", err.Error())
	}
	plan, err := tool.Plan(raw)
	if err != nil {
		return Failure("invalid", err.Error())
	}
	filePath, _, err := tool.Workspace.Resolve(plan.Path)
	if err != nil {
		return Failure("invalid_path", err.Error())
	}
	if plan.Mode == "delete" {
		if err := os.Remove(filePath); err != nil {
			return Failure("io", err.Error())
		}
		if err := syncDirectory(filepath.Dir(filePath)); err != nil {
			return Failure("io", err.Error())
		}
		if _, err := os.Stat(filePath); !errors.Is(err, os.ErrNotExist) {
			return Failure("io", "delete postcondition was not met")
		}
		return Success(struct {
			Path       string `json:"path"`
			BeforeHash string `json:"before_hash"`
			AfterHash  string `json:"after_hash"`
		}{Path: plan.Path, BeforeHash: plan.BeforeHash, AfterHash: plan.AfterHash}, "deleted "+plan.Path, 0, "", false)
	}
	mode := os.FileMode(0o600)
	if plan.Exists {
		if info, err := os.Stat(filePath); err == nil {
			mode = info.Mode().Perm()
		}
	}
	// 先清扫历史崩溃残留的同前缀临时文件，防止其被后续工具观察或积累；
	// 单写入者循环保证不存在并发补丁，删除是安全的。
	sweepPatchTemporaries(filepath.Dir(filePath))
	temporary, err := os.CreateTemp(filepath.Dir(filePath), ".cairn-patch-*")
	if err != nil {
		return Failure("io", err.Error())
	}
	if err := publishPatch(temporary, filePath, mode, plan.After); err != nil {
		return Failure("io", err.Error())
	}
	actual, err := os.ReadFile(filePath)
	if err != nil || sha256Reference(actual) != plan.AfterHash {
		return Failure("io", "patch postcondition hash mismatch")
	}
	return Success(struct {
		Path       string `json:"path"`
		BeforeHash string `json:"before_hash"`
		AfterHash  string `json:"after_hash"`
	}{Path: plan.Path, BeforeHash: plan.BeforeHash, AfterHash: plan.AfterHash}, "updated "+plan.Path, int64(len(actual)), plan.AfterHash, false)
}

// patchTemporaryFile 限定单文件补丁发布所需的临时文件操作，生产实现由 os.File 提供。
type patchTemporaryFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// publishPatch 完成临时补丁的写入、同步和关闭后才发布，退出时清理临时路径。
func publishPatch(temporary patchTemporaryFile, filePath string, mode os.FileMode, content []byte) error {
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	err := temporary.Chmod(mode)
	if err == nil {
		var written int
		written, err = temporary.Write(content)
		if err == nil && written != len(content) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(filePath))
}

// applyReplacements 仅在每段旧文本出现次数与声明值一致时执行替换；缺省次数按一次校验。
func applyReplacements(content []byte, replacements []Replacement) ([]byte, error) {
	updated := string(content)
	for _, replacement := range replacements {
		occurrence := replacement.Occurrence
		if occurrence == 0 {
			occurrence = 1
		}
		if replacement.Old == "" || occurrence < 1 {
			return nil, errors.New("each replacement requires non-empty old text and occurrence >= 1")
		}
		count := strings.Count(updated, replacement.Old)
		if count != occurrence {
			return nil, fmt.Errorf("replacement old text occurs %d time(s), want %d", count, occurrence)
		}
		updated = strings.ReplaceAll(updated, replacement.Old, replacement.New)
	}
	return []byte(updated), nil
}

// sha256Reference 生成带 sha256: 前缀的内容摘要引用。
func sha256Reference(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// sameSHA256 忽略大小写和可选前缀，比较两个非空 SHA-256 摘要。
func sameSHA256(candidate, expected string) bool {
	candidate = strings.TrimPrefix(strings.ToLower(candidate), "sha256:")
	expected = strings.TrimPrefix(strings.ToLower(expected), "sha256:")
	return candidate != "" && candidate == expected
}

// syncDirectory 同步目录元数据，增强原子重命名后的耐久性。
func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// sweepPatchTemporaries 删除目录内由历史崩溃残留的 .cairn-patch-* 临时文件；
// 清扫失败不阻断补丁本身，残留最多存活到下一次成功补丁。
func sweepPatchTemporaries(directory string) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), ".cairn-patch-") {
			_ = os.Remove(filepath.Join(directory, entry.Name()))
		}
	}
}
