package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
			return PatchPlan{}, errors.New("create requires content and forbids replacements")
		}
		plan.After = []byte(*arguments.Content)
	case "update":
		if !exists || arguments.Content != nil || len(arguments.Replacements) == 0 {
			return PatchPlan{}, errors.New("update requires an existing file and replacements")
		}
		if !sameSHA256(arguments.ExpectedSHA256, plan.BeforeHash) {
			return PatchPlan{}, errors.New("expected_sha256 does not match current file")
		}
		updated, err := applyReplacements(content, arguments.Replacements)
		if err != nil {
			return PatchPlan{}, err
		}
		plan.After = updated
	case "delete":
		if !exists || arguments.Content != nil || len(arguments.Replacements) != 0 {
			return PatchPlan{}, errors.New("delete requires an existing file and no content or replacements")
		}
		if !sameSHA256(arguments.ExpectedSHA256, plan.BeforeHash) {
			return PatchPlan{}, errors.New("expected_sha256 does not match current file")
		}
		plan.AfterHash = "absent"
	default:
		return PatchPlan{}, errors.New("mode must be create, update, or delete")
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
	temporary, err := os.CreateTemp(filepath.Dir(filePath), ".cairn-patch-*")
	if err != nil {
		return Failure("io", err.Error())
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err == nil {
		_, err = temporary.Write(plan.After)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Failure("io", err.Error())
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		return Failure("io", err.Error())
	}
	if err := syncDirectory(filepath.Dir(filePath)); err != nil {
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
