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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// queueWriteMu 串行化进程内队列状态检查与追加。
var queueWriteMu sync.Mutex

// queueFileLock 在队列命名空间写入期间持有跨进程排他文件锁；非阻塞获取，
// 被占用时返回错误而非无限等待，避免取消无法结束确认流程。
func queueFileLock(stateDir, workspaceHash string) (func(), error) {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	lockPath := filepath.Join(stateDir, "workspaces", workspaceHash, "queue.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("queue lock is held by another process: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

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
	// OnReserved 仅在确需新执行时、预分配 runID 后回调；登记失败则停止创建运行。
	OnReserved func(runID domain.RunID, sessionID string) error
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
	// ContentPreview 是队列正文的有界摘要，供 UI 展示；不是完整内容。
	ContentPreview string `json:"content_preview,omitempty"`
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
	queueWriteMu.Lock()
	defer queueWriteMu.Unlock()
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return QueueItemView{}, err
	}
	unlock, err := queueFileLock(options.StateDir, workspaceHash)
	if err != nil {
		return QueueItemView{}, err
	}
	defer unlock()
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
		Payload: artifact.MustEncodeRedactedJSON(struct {
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
	queueWriteMu.Lock()
	defer queueWriteMu.Unlock()
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return QueueItemView{}, err
	}
	unlock, err := queueFileLock(options.StateDir, workspaceHash)
	if err != nil {
		return QueueItemView{}, err
	}
	defer unlock()
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
		Payload: artifact.MustEncodeRedactedJSON(struct {
			InputID string `json:"input_id"`
			Reason  string `json:"reason"`
		}{InputID: options.InputID, Reason: options.Reason}),
	}); err != nil {
		return QueueItemView{}, err
	}
	return QueueItemView{InputID: options.InputID, State: domain.QueueItemCancelled, Reason: options.Reason}, nil
}

// ListQueue 只读列出队列项投影；存储/完整性错误上抛，仅不存在时返回空。
func (service Service) ListQueue(ctx context.Context, workspacePath, stateDir string, configuration config.Config) ([]QueueItemView, error) {
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return nil, err
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []QueueItemView{}, nil
		}
		return nil, err
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
		view := QueueItemView{InputID: item.InputID, State: item.State, TargetRunID: item.TargetRunID, Reason: item.Reason}
		if content, contentErr := loadQueueContent(journal, stateDir, workspaceHash, configuration, item); contentErr == nil {
			view.ContentPreview = queueContentPreview(content)
		}
		views = append(views, view)
	}
	return views, nil
}

// queueContentPreview 返回队列正文的单行有界摘要。
func queueContentPreview(content string) string {
	const max = 120
	runes := []rune(strings.TrimSpace(content))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "…"
}

// ConfirmQueueInput 确认启动待消费输入。
func (service Service) ConfirmQueueInput(ctx context.Context, options QueueConfirmOptions) (RunResult, error) {
	return service.ConfirmQueueInputWithEvents(ctx, options, nil, nil)
}

// ConfirmQueueInputWithEvents 确认启动待消费输入，并把目标 run 接入事件泵
// 与 steering 桥；写 queue_input_consumed{linked} 后以预分配 runID 创建新 run。
// 幂等：已 linked 且目标 run 存在则直接回传；linked 而目标缺失则对账重建。
func (service Service) ConfirmQueueInputWithEvents(ctx context.Context, options QueueConfirmOptions, onEvent EventSink, steering *agent.SteeringBridge) (RunResult, error) {
	if options.InputID == "" {
		return RunResult{}, errors.New("queue input id is required")
	}
	content, runID, err := service.reserveQueueLink(ctx, options)
	if err != nil {
		return RunResult{}, err
	}
	if content == "" {
		return RunResult{ID: runID}, nil
	}
	if options.OnReserved != nil {
		if err := options.OnReserved(runID, ""); err != nil {
			return RunResult{ID: runID}, err
		}
	}
	return service.Run(ctx, RunOptions{
		Task: content, Workspace: options.Workspace, StateDir: options.StateDir, Config: options.Config,
		PreassignedRunID: runID, OnEvent: onEvent, Steering: steering,
	})
}

// reserveQueueLink 在互斥与跨进程锁内完成队列状态检查与 consumed 追加；
// 返回待执行正文与预分配 runID。content 为空表示幂等命中已存在目标。
func (service Service) reserveQueueLink(ctx context.Context, options QueueConfirmOptions) (string, domain.RunID, error) {
	queueWriteMu.Lock()
	defer queueWriteMu.Unlock()
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return "", "", err
	}
	unlock, err := queueFileLock(options.StateDir, workspaceHash)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	journal, err := openJournal(options.StateDir, workspaceHash)
	if err != nil {
		return "", "", err
	}
	defer journal.Close()
	projection, err := loadQueueProjection(ctx, journal)
	if err != nil {
		return "", "", err
	}
	item, exists := projection.Items[options.InputID]
	if !exists {
		return "", "", fmt.Errorf("queue input %s not found", options.InputID)
	}
	if item.State == domain.QueueItemLinked {
		if item.TargetRunID != "" {
			runEvents, loadErr := journal.Load(ctx, item.TargetRunID)
			if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
				return "", "", fmt.Errorf("load linked queue target: %w", loadErr)
			}
			if len(runEvents) > 0 {
				return "", item.TargetRunID, nil
			}
		}
		content, contentErr := loadQueueContent(journal, options.StateDir, workspaceHash, options.Config, item)
		if contentErr != nil {
			return "", "", contentErr
		}
		runID := item.TargetRunID
		if runID == "" {
			runID = domain.NewRunID()
		}
		return content, runID, nil
	}
	if item.State != domain.QueueItemPending {
		return "", "", fmt.Errorf("queue input %s is already %s", options.InputID, item.State)
	}
	content, contentErr := loadQueueContent(journal, options.StateDir, workspaceHash, options.Config, item)
	if contentErr != nil {
		return "", "", contentErr
	}
	runID := domain.NewRunID()
	nextSeq, err := queueNextSeq(ctx, journal)
	if err != nil {
		return "", "", err
	}
	if _, err := journal.Append(ctx, nextSeq, domain.EventDraft{
		EventID: domain.NewID("evt"), RunID: domain.QueueRunID, WorkspaceHash: workspaceHash,
		Type: domain.EventQueueInputConsumed,
		Payload: artifact.MustEncodeRedactedJSON(struct {
			InputID     string       `json:"input_id"`
			Outcome     string       `json:"outcome"`
			TargetRunID domain.RunID `json:"target_run_id"`
		}{InputID: options.InputID, Outcome: "linked", TargetRunID: runID}),
	}); err != nil {
		return "", "", err
	}
	return content, runID, nil
}

// loadQueueContent 从制品引用读取队列正文。
func loadQueueContent(journal interface {
	Load(ctx context.Context, runID domain.RunID) ([]domain.Event, error)
}, stateDir, workspaceHash string, configuration config.Config, item *domain.QueueItem) (string, error) {
	if len(item.ContentRef) == 0 {
		return "", fmt.Errorf("queue input %s has no content", item.InputID)
	}
	var ref artifact.Ref
	if err := json.Unmarshal(item.ContentRef, &ref); err != nil {
		return "", fmt.Errorf("decode queue content ref: %w", err)
	}
	artifacts, err := openArtifactStore(stateDir, workspaceHash, configuration.Output)
	if err != nil {
		return "", err
	}
	opened, err := artifacts.Open(ref)
	if err != nil {
		return "", fmt.Errorf("open queue content: %w", err)
	}
	content := string(opened)
	if content == "" {
		return "", fmt.Errorf("queue input %s has empty content", item.InputID)
	}
	return content, nil
}

// loadQueueProjection 从 queue 命名空间事件重建投影；不存在时返回空投影，
// 其余存储/完整性错误向上抛出。
func loadQueueProjection(ctx context.Context, journal interface {
	Load(ctx context.Context, runID domain.RunID) ([]domain.Event, error)
}) (domain.QueueProjection, error) {
	events, err := journal.Load(ctx, domain.QueueRunID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.QueueProjection{Items: map[string]*domain.QueueItem{}}, nil
		}
		return domain.QueueProjection{}, fmt.Errorf("load queue namespace: %w", err)
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
