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

type ApplyPatch struct{ Workspace Workspace }

type Replacement struct {
	Old        string `json:"old"`
	New        string `json:"new"`
	Occurrence int    `json:"occurrence"`
}

type PatchArguments struct {
	Path           string        `json:"path"`
	Mode           string        `json:"mode"`
	ExpectedSHA256 string        `json:"expected_sha256"`
	Content        *string       `json:"content,omitempty"`
	Replacements   []Replacement `json:"replacements,omitempty"`
}

type PatchPlan struct {
	Path       string
	Mode       string
	BeforeHash string
	AfterHash  string
	Before     []byte
	After      []byte
	Exists     bool
}

func (tool ApplyPatch) SideEffect() SideEffectClass { return SideEffectWorkspace }

func (tool ApplyPatch) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "apply_patch", Description: "Apply a deterministic, single-file patch after approval.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path","mode"],"properties":{"path":{"type":"string"},"mode":{"type":"string","enum":["create","update","delete"]},"expected_sha256":{"type":"string"},"content":{"type":"string"},"replacements":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["old","new","occurrence"],"properties":{"old":{"type":"string"},"new":{"type":"string"},"occurrence":{"type":"integer","minimum":1}}}}}}`)}
}

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

func applyReplacements(content []byte, replacements []Replacement) ([]byte, error) {
	updated := string(content)
	for _, replacement := range replacements {
		if replacement.Old == "" || replacement.Occurrence < 1 {
			return nil, errors.New("each replacement requires non-empty old text and occurrence >= 1")
		}
		count := strings.Count(updated, replacement.Old)
		if count != replacement.Occurrence {
			return nil, fmt.Errorf("replacement old text occurs %d time(s), want %d", count, replacement.Occurrence)
		}
		updated = strings.ReplaceAll(updated, replacement.Old, replacement.New)
	}
	return []byte(updated), nil
}

func sha256Reference(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func sameSHA256(candidate, expected string) bool {
	candidate = strings.TrimPrefix(strings.ToLower(candidate), "sha256:")
	expected = strings.TrimPrefix(strings.ToLower(expected), "sha256:")
	return candidate != "" && candidate == expected
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
