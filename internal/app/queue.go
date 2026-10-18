// queue.go 实现持久后续队列的应用层用例：提交、确认关联新 run、取消。
// 写入者是 App 层（跨 run 生命周期），事实源是 Journal 的 queue 命名空间；
// 队列索引仅作投影，不另建表或文件。首版确认后才创建新 run，不自动串行。
//
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// QueueSubmitOptions 描述一次后续任务入队输入。
type QueueSubmitOptions struct {
	Workspace string
	StateDir  string
	Config    config.Config
	InputID   string
	Content   string
}

// QueueConfirmOptions 描述一次确认启动：把待消费输入关联到新 run。
type QueueConfirmOptions struct {
	Workspace string
	StateDir  string
	Config    config.Config
	InputID   string
}

// QueueCancelOptions 描述一次取消。
type QueueCancelOptions struct {
	Workspace string
	StateDir  string
	Config    config.Config
	InputID   string
	Reason    string
}

// QueueItemView 是对外只读队列项投影。
type QueueItemView struct {
	InputID     string                `json:"input_id"`
	State       domain.QueueItemState `json:"state"`
	TargetRunID domain.RunID          `json:"target_run_id,omitempty"`
	Reason      string                `json:"reason,omitempty"`
}

// SubmitQueueInput 把后续任务写入队列命名空间；正文入受限脱敏制品，
// 事件只带引用。重复 InputID 幂等（已存在则拒绝重复提交）。
func (service Service) SubmitQueueInput(ctx context.Context, options QueueSubmitOptions) (QueueItemView, error) {
	if options.InputID == "" {
		return QueueItemView{}, errors.New("queue input id is required")
	}
	if options.Content == "" {
		return QueueItemView{}, errors.New("queue input content is required")
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return QueueItemView{}, err
	}
	journal, err := openJournal(options.StateDir, workspaceHash)
	if err != nil {
		return QueueItemView{}, err
	}
	defer journal.Close()
	artifacts, err := openArtifactStore(options.StateDir, workspaceHash, options.Config.Output)
	if err != nil {
		return QueueItemView{}, err
	}
	projection, err := loadQueueProjection(ctx, journal)
	if err != nil {
		return QueueItemView{}, err
	}
	if existing, exists := projection.Items[options.InputID]; exists {
		return QueueItemView{}, fmt.Errorf("queue input %s already exists in state %s", options.InputID, existing.State)
	}
	ref, err := artifacts.Put([]byte(options.Content), "text/plain")
	if err != nil {
		return QueueItemView{}, fmt.Errorf("store queue input content: %w", err)
	}
	nextSeq, err := queueNextSeq(ctx, journal)
	if err != nil {
		return QueueItemView{}, err
	}
	if _, err := journal.Append(ctx, nextSeq, domain.EventDraft{
		EventID: domain.NewID("evt"), RunID: domain.QueueRunID, WorkspaceHash: workspaceHash,
		Type: domain.EventQueueInputSubmitted,
		Payload: artifact.EncodeRedactedJSON(struct {
			InputID    string       `json:"input_id"`
			ContentRef artifact.Ref `json:"content_ref"`
			Kind       string       `json:"kind"`
		}{InputID: options.InputID, ContentRef: ref, Kind: "next_task"}),
	}); err != nil {
		return QueueItemView{}, err
	}
	return QueueItemView{InputID: options.InputID, State: domain.QueueItemPending}, nil
}

// CancelQueueInput 取消待消费输入；已终结项拒绝重复取消。
func (service Service) CancelQueueInput(ctx context.Context, options QueueCancelOptions) (QueueItemView, error) {
	if options.InputID == "" || options.Reason == "" {
		return QueueItemView{}, errors.New("queue input id and reason are required")
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return QueueItemView{}, err
	}
	journal, err := openJournal(options.StateDir, workspaceHash)
	if err != nil {
		return QueueItemView{}, err
	}
	defer journal.Close()
	projection, err := loadQueueProjection(ctx, journal)
	if err != nil {
		return QueueItemView{}, err
	}
	item, exists := projection.Items[options.InputID]
	if !exists {
		return QueueItemView{}, fmt.Errorf("queue input %s not found", options.InputID)
	}
	if item.State != domain.QueueItemPending {
		return QueueItemView{}, fmt.Errorf("queue input %s is already %s", options.InputID, item.State)
	}
	nextSeq, err := queueNextSeq(ctx, journal)
	if err != nil {
		return QueueItemView{}, err
	}
	if _, err := journal.Append(ctx, nextSeq, domain.EventDraft{
		EventID: domain.NewID("evt"), RunID: domain.QueueRunID, WorkspaceHash: workspaceHash,
		Type: domain.EventQueueInputCancelled,
		Payload: artifact.EncodeRedactedJSON(struct {
			InputID string `json:"input_id"`
			Reason  string `json:"reason"`
		}{InputID: options.InputID, Reason: options.Reason}),
	}); err != nil {
		return QueueItemView{}, err
	}
	return QueueItemView{InputID: options.InputID, State: domain.QueueItemCancelled, Reason: options.Reason}, nil
}

// ListQueue 只读列出队列项投影。
func (service Service) ListQueue(ctx context.Context, workspacePath, stateDir string, configuration config.Config) ([]QueueItemView, error) {
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return nil, err
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if err != nil {
		return []QueueItemView{}, nil
	}
	defer journal.Close()
	projection, err := loadQueueProjection(ctx, journal)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(projection.Items))
	for id := range projection.Items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	views := make([]QueueItemView, 0, len(ids))
	for _, id := range ids {
		item := projection.Items[id]
		views = append(views, QueueItemView{InputID: item.InputID, State: item.State, TargetRunID: item.TargetRunID, Reason: item.Reason})
	}
	return views, nil
}

// ConfirmQueueInput 确认启动待消费输入：写 queue_input_consumed{linked} 并以
// 预分配 runID 创建新 run。幂等：已 linked 直接回传目标 run 标识。崩溃于
// consumed 与 run_created 之间时目标 run 可能缺失，由后续对账处理；
// input_id 已 consumed 不重复投递内容。
func (service Service) ConfirmQueueInput(ctx context.Context, options QueueConfirmOptions) (RunResult, error) {
	if options.InputID == "" {
		return RunResult{}, errors.New("queue input id is required")
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	journal, err := openJournal(options.StateDir, workspaceHash)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := loadQueueProjection(ctx, journal)
	if err != nil {
		_ = journal.Close()
		return RunResult{}, err
	}
	item, exists := projection.Items[options.InputID]
	if !exists {
		_ = journal.Close()
		return RunResult{}, fmt.Errorf("queue input %s not found", options.InputID)
	}
	if item.State == domain.QueueItemLinked {
		// 幂等：已关联则直接续看目标 run，不重复创建。
		_ = journal.Close()
		return RunResult{ID: item.TargetRunID}, nil
	}
	if item.State != domain.QueueItemPending {
		_ = journal.Close()
		return RunResult{}, fmt.Errorf("queue input %s is already %s", options.InputID, item.State)
	}
	content := ""
	if len(item.ContentRef) > 0 {
		var ref artifact.Ref
		if err := json.Unmarshal(item.ContentRef, &ref); err != nil {
			_ = journal.Close()
			return RunResult{}, fmt.Errorf("decode queue content ref: %w", err)
		}
		artifacts, artErr := openArtifactStore(options.StateDir, workspaceHash, options.Config.Output)
		if artErr != nil {
			_ = journal.Close()
			return RunResult{}, artErr
		}
		opened, openErr := artifacts.Open(ref)
		if openErr != nil {
			_ = journal.Close()
			return RunResult{}, fmt.Errorf("open queue content: %w", openErr)
		}
		content = string(opened)
	}
	if content == "" {
		_ = journal.Close()
		return RunResult{}, fmt.Errorf("queue input %s has no content", options.InputID)
	}
	// 事务顺序：consumed{linked, target_run_id} 先写，再以同一预分配 runID
	// 创建 run；input_id 已 consumed 不重复投递内容。崩溃于两步之间的
	// 目标 run 缺口由后续对账处理。
	runID := domain.NewRunID()
	nextSeq, err := queueNextSeq(ctx, journal)
	if err != nil {
		_ = journal.Close()
		return RunResult{}, err
	}
	if _, err := journal.Append(ctx, nextSeq, domain.EventDraft{
		EventID: domain.NewID("evt"), RunID: domain.QueueRunID, WorkspaceHash: workspaceHash,
		Type: domain.EventQueueInputConsumed,
		Payload: artifact.EncodeRedactedJSON(struct {
			InputID     string       `json:"input_id"`
			Outcome     string       `json:"outcome"`
			TargetRunID domain.RunID `json:"target_run_id"`
		}{InputID: options.InputID, Outcome: "linked", TargetRunID: runID}),
	}); err != nil {
		_ = journal.Close()
		return RunResult{}, err
	}
	// 显式关闭后再交由 service.Run 自己装配日志。
	if err := journal.Close(); err != nil {
		return RunResult{}, err
	}
	return service.Run(ctx, RunOptions{
		Task: content, Workspace: options.Workspace, StateDir: options.StateDir, Config: options.Config,
		PreassignedRunID: runID,
	})
}

// loadQueueProjection 从 queue 命名空间事件重建投影。
func loadQueueProjection(ctx context.Context, journal interface {
	Load(ctx context.Context, runID domain.RunID) ([]domain.Event, error)
}) (domain.QueueProjection, error) {
	events, err := journal.Load(ctx, domain.QueueRunID)
	if err != nil {
		return domain.QueueProjection{Items: map[string]*domain.QueueItem{}}, nil
	}
	if len(events) == 0 {
		return domain.QueueProjection{Items: map[string]*domain.QueueItem{}}, nil
	}
	return domain.ProjectQueue(events)
}

// queueNextSeq 计算 queue 命名空间的下一个事件序号。
func queueNextSeq(ctx context.Context, journal interface {
	Load(ctx context.Context, runID domain.RunID) ([]domain.Event, error)
}) (uint64, error) {
	events, err := journal.Load(ctx, domain.QueueRunID)
	if err != nil {
		return 0, err
	}
	return uint64(len(events)), nil
}
