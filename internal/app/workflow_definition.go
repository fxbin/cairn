/*
 * [INPUT]: 依赖定义合同、工作区身份、SQLite 索引与制品库。
 * [OUTPUT]: 提供工作流定义保存、列表、按修订查询和冻结锚点。
 * [POS]: 应用门面验证正文摘要与工作区归属。保存不触发执行。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 */
// 本文件连接宿主提交的工作流定义与持久化修订。
// Author: fxbin
package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	workflowsqlite "github.com/fxbin/cairn/internal/capabilities/workflow/storage/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/runtime/artifact"
)

var (
	// ErrWorkflowDisabled 表示最小配置未装配工作流能力。
	ErrWorkflowDisabled = errors.New("workflow is disabled in the minimal preset")
	// ErrWorkflowDefinitionNotFound 表示定义或明确请求的修订不存在。
	ErrWorkflowDefinitionNotFound = errors.New("workflow definition not found")
	// ErrWorkflowDefinitionConflict 表示定义已被另一调用推进。
	ErrWorkflowDefinitionConflict = workflow.ErrConflict
)

// WorkflowScope 绑定应用门面的工作区、状态根与有效配置。
type WorkflowScope struct {
	Workspace string
	StateDir  string
	Config    config.Config
}

// WorkflowDefinitionID 是工作区内的工作流模板标识。
type WorkflowDefinitionID = workflow.DefinitionID

// WorkflowDefinitionDraft 是宿主显式保存的节点和模板名称。
type WorkflowDefinitionDraft = workflow.DefinitionDraft

// WorkflowDefinition 是不可变的模板修订。
type WorkflowDefinition = workflow.Definition

// WorkflowDefinitionRef 定位某一冻结模板修订的正文制品。
type WorkflowDefinitionRef = workflow.DefinitionRef

// WorkflowNode 声明线性工作流中的单个节点。
type WorkflowNode = workflow.Node

// SaveWorkflowDefinition 先校验并保存脱敏正文，再按预期修订追加索引。
func (Service) SaveWorkflowDefinition(ctx context.Context, scope WorkflowScope, draft WorkflowDefinitionDraft, expectedRevision uint64) (WorkflowDefinition, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowDefinition{}, err
	}
	scope, hash, err := scope.resolve()
	if err != nil {
		return WorkflowDefinition{}, err
	}
	if expectedRevision >= math.MaxInt64 || expectedRevision != 0 && draft.ID == "" {
		return WorkflowDefinition{}, errors.New("invalid expected workflow definition revision")
	}
	definition, err := workflow.NewDefinition(hash, draft, expectedRevision+1)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	content, err := json.Marshal(definition)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	if len(content) > workflow.MaxDefinitionArtifactBytes {
		return WorkflowDefinition{}, errors.New("workflow definition exceeds artifact limit")
	}
	artifacts, err := openArtifactStore(scope.StateDir, hash, scope.Config.Output)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	ref, err := artifacts.Put(content, "application/json")
	if err != nil {
		return WorkflowDefinition{}, fmt.Errorf("write workflow definition artifact: %w", err)
	}
	ref.TruncatedPreview = ""
	reference := workflow.DefinitionRef{ID: definition.ID, Revision: definition.Revision, Artifact: ref}
	frozen, err := readWorkflowDefinitionArtifact(scope.StateDir, hash, reference)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	store, err := workflowsqlite.Open(workflowDBPath(scope.StateDir, hash))
	if err != nil {
		return WorkflowDefinition{}, workflowDefinitionStorageError(err)
	}
	defer store.Close()
	if err := store.SaveDefinition(ctx, hash, reference, expectedRevision); err != nil {
		return WorkflowDefinition{}, workflowDefinitionStorageError(err)
	}
	return frozen, nil
}

// ReadWorkflowDefinition 查询一个经过正文和归属校验的修订。零表示最新。
func (service Service) ReadWorkflowDefinition(ctx context.Context, scope WorkflowScope, id WorkflowDefinitionID, revision uint64) (WorkflowDefinition, error) {
	definition, _, err := service.ReadWorkflowDefinitionSnapshot(ctx, scope, id, revision)
	return definition, err
}

// ReadWorkflowDefinitionSnapshot 返回已核验的修订及其冻结锚点。
func (Service) ReadWorkflowDefinitionSnapshot(ctx context.Context, scope WorkflowScope, id WorkflowDefinitionID, revision uint64) (WorkflowDefinition, WorkflowDefinitionRef, error) {
	scope, hash, err := scope.resolve()
	if err != nil {
		return WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	return loadWorkflowDefinition(ctx, scope, hash, id, revision)
}

// loadWorkflowDefinition 在已规范化的作用域中读取一个索引快照和对应制品。
func loadWorkflowDefinition(ctx context.Context, scope WorkflowScope, hash string, id WorkflowDefinitionID, revision uint64) (WorkflowDefinition, WorkflowDefinitionRef, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	if !id.Valid() || revision > math.MaxInt64 {
		return WorkflowDefinition{}, WorkflowDefinitionRef{}, errors.New("invalid workflow definition identity")
	}
	store, err := openWorkflowStoreReadOnly(scope.StateDir, hash)
	if err != nil {
		if errors.Is(err, ErrWorkflowNotFound) {
			err = ErrWorkflowDefinitionNotFound
		}
		return WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	defer store.Close()
	reference, err := store.ReadDefinition(ctx, hash, id, revision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowDefinition{}, WorkflowDefinitionRef{}, ErrWorkflowDefinitionNotFound
		}
		return WorkflowDefinition{}, WorkflowDefinitionRef{}, workflowDefinitionStorageError(err)
	}
	definition, err := readWorkflowDefinitionArtifact(scope.StateDir, hash, reference)
	if err != nil {
		return WorkflowDefinition{}, WorkflowDefinitionRef{}, err
	}
	return definition, reference, nil
}

// ListWorkflowDefinitions 返回每个定义的最新修订。空工作区的查询不创建库。
func (Service) ListWorkflowDefinitions(ctx context.Context, scope WorkflowScope) ([]WorkflowDefinition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, hash, err := scope.resolve()
	if err != nil {
		return nil, err
	}
	store, err := openWorkflowStoreReadOnly(scope.StateDir, hash)
	if err != nil {
		if errors.Is(err, ErrWorkflowNotFound) {
			return []WorkflowDefinition{}, nil
		}
		return nil, err
	}
	defer store.Close()
	references, err := store.ListDefinitions(ctx, hash)
	if err != nil {
		return nil, workflowDefinitionStorageError(err)
	}
	definitions := make([]WorkflowDefinition, 0, len(references))
	for _, reference := range references {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		definition, err := readWorkflowDefinitionArtifact(scope.StateDir, hash, reference)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

// workflowDefinitionStorageError 将持久化损坏与并发、权限等环境错误分开。
func workflowDefinitionStorageError(err error) error {
	if workflowsqlite.IsStateCorruptError(err) {
		return fmt.Errorf("%w: workflow definition index: %w", ErrStateCorrupt, err)
	}
	return err
}

// readWorkflowDefinitionArtifact 只读核验已固定的定义。恢复不需要查询最新索引。
func readWorkflowDefinitionArtifact(stateDir, hash string, reference WorkflowDefinitionRef) (WorkflowDefinition, error) {
	if err := reference.Validate(); err != nil {
		return WorkflowDefinition{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	artifacts := artifact.Store{Root: manifestArtifactRoot(stateDir, hash)}
	content, err := artifacts.OpenBounded(reference.Artifact, workflow.MaxDefinitionArtifactBytes)
	if err != nil {
		return WorkflowDefinition{}, fmt.Errorf("%w: read workflow definition artifact: %w", ErrStateCorrupt, err)
	}
	if int64(len(content)) != reference.Artifact.Bytes {
		return WorkflowDefinition{}, fmt.Errorf("%w: workflow definition artifact size mismatch", ErrStateCorrupt)
	}
	var definition WorkflowDefinition
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&definition); err != nil {
		return WorkflowDefinition{}, fmt.Errorf("%w: decode workflow definition artifact: %w", ErrStateCorrupt, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return WorkflowDefinition{}, fmt.Errorf("%w: extra workflow definition artifact content", ErrStateCorrupt)
	}
	if definition.WorkspaceHash != hash || definition.ID != reference.ID || definition.Revision != reference.Revision {
		return WorkflowDefinition{}, fmt.Errorf("%w: workflow definition artifact identity mismatch", ErrStateCorrupt)
	}
	if err := definition.Validate(); err != nil {
		return WorkflowDefinition{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return definition, nil
}

// resolve 规范化工作区和状态根，并在最小配置下拒绝打开能力存储。
func (scope WorkflowScope) resolve() (WorkflowScope, string, error) {
	if scope.Config.Preset == config.PresetMinimal {
		return WorkflowScope{}, "", ErrWorkflowDisabled
	}
	workspace, hash, err := workspaceFor(scope.Workspace, scope.Config)
	if err != nil {
		return WorkflowScope{}, "", err
	}
	scope.Workspace = workspace.Root
	if scope.StateDir == "" {
		scope.StateDir = DefaultStateDir()
	}
	scope.StateDir, err = filepath.Abs(scope.StateDir)
	return scope, hash, err
}
