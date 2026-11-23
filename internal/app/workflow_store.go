/*
 * [INPUT]: 依赖workflow SQLite 叶子、制品与状态根。
 * [OUTPUT]: 提供工作流存储路径、打开、追加和任务读取。
 * [POS]: 应用存储连接；不更改事件编码、迁移或链头 CAS。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 */
// Package app 连接工作流能力与既有运行入口。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	workflowsqlite "github.com/fxbin/cairn/internal/capabilities/workflow/storage/sqlite"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/runtime/artifact"
)

// reload 重放工作流链并返回唯一权威投影；存储缺失与链损坏分别失败关闭。
func (execution workflowExecution) reload(ctx context.Context, id workflow.ID) (*workflow.Snapshot, error) {
	events, err := execution.store.Load(ctx, id)
	if err != nil {
		if workflowsqlite.IsStateCorruptError(err) {
			return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
		}
		return nil, err
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrWorkflowNotFound, id)
	}
	snapshot, err := workflow.Project(events)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if snapshot.WorkflowID != id || snapshot.WorkspaceHash != execution.workspaceHash {
		return nil, fmt.Errorf("%w: workflow instance identity mismatch", ErrStateCorrupt)
	}
	return snapshot, nil
}

// appendDraft 以链头 CAS 追加一个工作流事件；本执行器是链的唯一写入方，
// 冲突即失败关闭，不重试以免掩盖并发写入。
func (execution workflowExecution) appendDraft(ctx context.Context, id workflow.ID, eventType workflow.EventType, payload any) (workflow.Event, error) {
	return execution.appendDraftAt(ctx, id, 0, eventType, payload)
}

// appendDraftAt 以已验证的链头和候选重放保护版本化实例的追加。
func (execution workflowExecution) appendDraftAt(ctx context.Context, id workflow.ID, expectedSeq uint64, eventType workflow.EventType, payload any) (workflow.Event, error) {
	events, err := execution.store.Load(ctx, id)
	if err != nil {
		return workflow.Event{}, err
	}
	if len(events) == 0 {
		return workflow.Event{}, fmt.Errorf("%w: %s", ErrWorkflowNotFound, id)
	}
	draft := workflow.EventDraft{
		WorkflowID:    id,
		WorkspaceHash: execution.workspaceHash,
		Type:          eventType,
		Payload:       contract.MustEncodeRedactedJSON(payload),
		OccurredAt:    time.Now().UTC(),
	}
	last := events[len(events)-1]
	if expectedSeq != 0 && expectedSeq != last.Seq {
		return workflow.Event{}, workflow.ErrConflict
	}
	if execution.definition != nil {
		candidate, err := workflow.NewEvent(draft, last.Seq+1, last.EventHash)
		if err != nil {
			return workflow.Event{}, err
		}
		if _, err := workflow.Project(append(events, candidate)); err != nil {
			return workflow.Event{}, err
		}
	}
	return execution.store.Append(ctx, last.Seq, draft)
}

// appendRunFinished 收束工作流终态；resumeIndex 记录首个未成功步骤下标，
// 全部成功时为 -1。
func (execution workflowExecution) appendRunFinished(ctx context.Context, id workflow.ID, state string, resumeIndex int) error {
	_, err := execution.appendDraft(ctx, id, workflow.EventRunFinished, workflow.RunFinishedPayload{State: state, ResumeIndex: resumeIndex})
	return err
}

// stateRoot 解析状态目录的统一根；空值回退平台默认，与装配路径一致。
func (execution workflowExecution) stateRoot() string {
	if execution.stateDir == "" {
		return DefaultStateDir()
	}
	return execution.stateDir
}

// openWorkflowStoreExisting 打开既有工作流存储供追加；库文件不存在时以未
// 找到失败关闭，不隐式建库。
func openWorkflowStoreExisting(stateDir, workspaceHash string) (*workflowsqlite.Store, error) {
	path := workflowDBPath(stateDir, workspaceHash)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: no workflow store in this workspace", ErrWorkflowNotFound)
		}
		return nil, err
	}
	return workflowsqlite.Open(path)
}

// readWorkflowTask 从制品库读回创建事件绑定的任务文本；缺少绑定或制品不
// 可读时失败关闭，不猜测任务正文。
func readWorkflowTask(artifacts *artifact.Store, binding workflow.StepIO) (string, error) {
	if binding.Ref == "" {
		return "", errors.New("workflow created event has no task artifact binding; chains created before the execution slice cannot be resumed")
	}
	content, err := artifacts.Open(contract.Ref{SHA256: binding.Ref})
	if err != nil {
		return "", fmt.Errorf("open workflow task artifact: %w", err)
	}
	return string(content), nil
}

// workflowDBPath 返回工作区专属工作流存储路径，与 runs.db、memory.db 同目录。
func workflowDBPath(stateDir, workspaceHash string) string {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return filepath.Join(stateDir, "workspaces", safeWorkspaceDir(workspaceHash), "workflow.db")
}

// openWorkflowStoreReadOnly 打开既有的工作流存储；尚未创建任何工作流时
// 以未找到错误失败关闭，不做隐式建库。
func openWorkflowStoreReadOnly(stateDir, workspaceHash string) (*workflowsqlite.Store, error) {
	path := workflowDBPath(stateDir, workspaceHash)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: no workflow store in this workspace", ErrWorkflowNotFound)
		}
		return nil, err
	}
	return workflowsqlite.OpenReadOnly(path)
}
