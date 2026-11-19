// [INPUT]: 依赖工作流事实索引、实例重放与当前工作区。
// [OUTPUT]: 拒绝使用普通界面入口改变版本化工作流尝试。
// [POS]: UI 通过工作流入口控制绑定 Run。CLI 原运行入口与旧工作流兼容。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件区分独立 Run 和已绑定工作流的尝试。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

const workflowRunLinkMediaType = "application/vnd.cairn.workflow-run+json"

// workflowRunLink 锚定 Agent Run 的工作流身份。脚本类 Run 自带冻结计划引用。
type workflowRunLink struct {
	WorkflowID string `json:"workflow_id"`
	StepID     string `json:"step_id"`
}

// storeWorkflowRunLink 写入只读归属制品，保证 Journal 自身可识别新尝试。
func storeWorkflowRunLink(boot *Boot, link workflowRunLink) (contract.Ref, error) {
	if link.WorkflowID == "" || link.StepID == "" {
		return contract.Ref{}, errors.New("workflow run link requires instance and step identifiers")
	}
	ref, err := boot.Artifacts.Put(contract.MustEncodeRedactedJSON(link), "application/json")
	if err != nil {
		return contract.Ref{}, err
	}
	ref.MediaType, ref.TruncatedPreview = workflowRunLinkMediaType, ""
	return ref, nil
}

// ErrWorkflowRunControlled 表示应从对应工作流实例进行人工控制。
var ErrWorkflowRunControlled = errors.New("workflow run requires control through its workflow instance")

// RequireStandaloneRun 查找版本化实例的历史绑定，仅供表面入口分派控制。
func (Service) RequireStandaloneRun(ctx context.Context, scope WorkflowScope, runID domain.RunID) error {
	if scope.Config.Preset == config.PresetMinimal {
		return nil
	}
	scope, hash, err := scope.resolve()
	if err != nil {
		return err
	}
	owned, err := journalMarksWorkflowRun(ctx, scope.StateDir, hash, runID)
	if err != nil {
		return err
	}
	if owned {
		return ErrWorkflowRunControlled
	}
	return checkHistoricalWorkflowBinding(ctx, scope.StateDir, hash, runID)
}

// checkHistoricalWorkflowBinding 核对尚未带 Run 归属制品的旧版实例尝试。
func checkHistoricalWorkflowBinding(ctx context.Context, stateDir, hash string, runID domain.RunID) error {
	store, err := openWorkflowStoreReadOnly(stateDir, hash)
	if errors.Is(err, ErrWorkflowNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer store.Close()
	ids, err := store.InstancesWithRun(ctx, hash, runID)
	if err != nil {
		return err
	}
	execution := workflowExecution{store: store, workspaceHash: hash, stateDir: stateDir}
	for _, id := range ids {
		snapshot, err := execution.reload(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.Definition == nil {
			continue
		}
		for _, attempts := range snapshot.Attempts {
			for _, attempt := range attempts {
				if attempt.RunID == runID {
					return fmt.Errorf("%w: %s", ErrWorkflowRunControlled, id)
				}
			}
		}
		return fmt.Errorf("%w: workflow run index mismatch", ErrStateCorrupt)
	}
	return nil
}

// journalMarksWorkflowRun 从不可变创建回执识别归属，缺失工作流库不会降级成普通批准。
func journalMarksWorkflowRun(ctx context.Context, stateDir, workspaceHash string, runID domain.RunID) (bool, error) {
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer journal.Close()
	return journalMarksWorkflowRunFrom(ctx, journal, workspaceHash, runID)
}

// journalMarksWorkflowRunFrom 复用已打开的 Run Journal。最小配置不打开能力库。
func journalMarksWorkflowRunFrom(ctx context.Context, journal ports.Journal, workspaceHash string, runID domain.RunID) (bool, error) {
	events, err := journal.Load(ctx, runID)
	if err != nil || len(events) == 0 {
		return false, err
	}
	if events[0].Type != domain.EventRunCreated || events[0].WorkspaceHash != workspaceHash || events[0].RunID != runID {
		return false, fmt.Errorf("%w: invalid run creation while checking workflow ownership", ErrStateCorrupt)
	}
	var created struct {
		TaskRef *contract.Ref `json:"task_ref"`
	}
	if err := json.Unmarshal(events[0].Payload, &created); err != nil {
		return false, fmt.Errorf("%w: invalid run creation: %w", ErrStateCorrupt, err)
	}
	return created.TaskRef != nil && (created.TaskRef.MediaType == workflowRunLinkMediaType || created.TaskRef.MediaType == workflowScriptPlanMediaType), nil
}

// requireStandaloneContinuation 在 Run 锁内校验普通续链的前驱归属。
func requireStandaloneContinuation(ctx context.Context, boot *Boot, options RunOptions) error {
	owned, err := journalMarksWorkflowRunFrom(ctx, boot.Journal, boot.WorkspaceHash, options.PrevRunID)
	if err != nil {
		return err
	}
	if owned {
		return ErrWorkflowRunControlled
	}
	if options.Config.Preset == config.PresetMinimal {
		return nil
	}
	stateDir := options.StateDir
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return checkHistoricalWorkflowBinding(ctx, stateDir, boot.WorkspaceHash, options.PrevRunID)
}
