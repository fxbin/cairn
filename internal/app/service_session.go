// 会话链校验与树投影。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// 生命周期。日志内容或跨工作区身份不一致时失败关闭。
func listRunHistory(ctx context.Context, journal interface {
	ports.Journal
	ports.RunLister
}, workspaceHash string) ([]RunHistoryItem, error) {
	runIDs, err := journal.RunIDs(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sqlite.IsStateCorruptError(err) {
			return nil, fmt.Errorf("%w: list runs: %w", ErrStateCorrupt, err)
		}
		return nil, err
	}
	items := make([]RunHistoryItem, 0, len(runIDs))
	for _, runID := range runIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if runID == domain.QueueRunID {
			continue
		}
		events, loadErr := journal.Load(ctx, runID)
		if loadErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if sqlite.IsStateCorruptError(loadErr) {
				return nil, fmt.Errorf("%w: load run %s: %w", ErrStateCorrupt, runID, loadErr)
			}
			return nil, loadErr
		}
		if len(events) == 0 {
			return nil, fmt.Errorf("%w: run %s has no events", ErrStateCorrupt, runID)
		}
		projection, replayErr := domain.Replay(events)
		if replayErr != nil {
			return nil, fmt.Errorf("%w: replay run %s: %v", ErrStateCorrupt, runID, replayErr)
		}
		if projection.WorkspaceHash != workspaceHash {
			return nil, fmt.Errorf("%w: run %s belongs to workspace %s", ErrStateCorrupt, runID, projection.WorkspaceHash)
		}
		items = append(items, RunHistoryItem{ID: projection.ID, SessionID: projection.SessionID, PrevRunID: projection.PrevRunID, ForkSource: projection.ForkSource, Task: projection.Task, State: projection.State, Phase: projection.Phase(), CreatedAt: projection.CreatedAt, UpdatedAt: projection.UpdatedAt})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// validateSessionLink 校验所有新 Run 的会话边：前驱必须存在于当前工作区、
// 已终结且会话标识一致；同一会话不得创建第二个根。显式回退仍可从合法的
// 历史中间节点分叉，不受唯一链尾门禁约束。跨会话分支只能通过 fork_source
// 指向已终结来源 run，且只允许出现在新会话根上。
func validateSessionLink(ctx context.Context, journal interface {
	ports.Journal
	ports.RunLister
}, stateDir, workspaceHash string, options RunOptions) error {
	if options.ForkSource != nil && options.PrevRunID != "" {
		return fmt.Errorf("%w: fork_source is mutually exclusive with prev_run_id", ErrSessionContinuation)
	}
	if options.ForkSource != nil {
		return validateSessionForkSource(ctx, journal, stateDir, workspaceHash, options)
	}
	if options.PrevRunID != "" {
		events, err := journal.Load(ctx, options.PrevRunID)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return fmt.Errorf("%w: prior run %s not found", ErrSessionContinuation, options.PrevRunID)
		}
		if err := checkSessionRunContract(events, stateDir, workspaceHash, options.PrevRunID); err != nil {
			return err
		}
		projection, err := domain.Replay(events)
		if err != nil {
			return fmt.Errorf("%w: replay prior run %s: %v", ErrStateCorrupt, options.PrevRunID, err)
		}
		if projection.WorkspaceHash != workspaceHash {
			return fmt.Errorf("%w: prior run %s belongs to another workspace", ErrSessionContinuation, options.PrevRunID)
		}
		if !projection.Terminal() {
			return fmt.Errorf("%w: prior run %s is not terminal", ErrSessionContinuation, options.PrevRunID)
		}
		if projection.SessionID == "" {
			return fmt.Errorf("%w: prior run %s has no session identity", ErrSessionContinuation, options.PrevRunID)
		}
		if options.SessionID != "" && projection.SessionID != "" && options.SessionID != projection.SessionID {
			return fmt.Errorf("%w: prior run %s belongs to session %s", ErrSessionContinuation, options.PrevRunID, projection.SessionID)
		}
		if err := validateSessionAncestors(ctx, journal, stateDir, workspaceHash, projection); err != nil {
			return err
		}
		return nil
	}
	if options.SessionID == "" {
		return nil
	}
	items, err := listRunHistory(ctx, journal, workspaceHash)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.SessionID == options.SessionID {
			return fmt.Errorf("%w: session %s already has a root", ErrSessionContinuation, options.SessionID)
		}
	}
	return nil
}

// validateSessionAncestors 沿直接前驱的 PrevRunID 回溯完整祖先链；每个祖先
// 都必须属于同一工作区和会话、处于终态，并通过当前 v2 冻结合同校验。该校验
// 与唯一链尾约束分离，使显式回退仍可分叉，但不能从含旧合同的历史继续运行。
func validateSessionAncestors(ctx context.Context, journal ports.Journal, stateDir, workspaceHash string, direct domain.RunProjection) error {
	visited := map[domain.RunID]struct{}{direct.ID: {}}
	for ancestorID := direct.PrevRunID; ancestorID != ""; {
		if _, duplicate := visited[ancestorID]; duplicate {
			return fmt.Errorf("%w: session %s contains a cycle at run %s", ErrStateCorrupt, direct.SessionID, ancestorID)
		}
		visited[ancestorID] = struct{}{}
		events, err := journal.Load(ctx, ancestorID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: load session ancestor %s: %v", ErrStateCorrupt, ancestorID, err)
		}
		if len(events) == 0 {
			return fmt.Errorf("%w: session %s references missing run %s", ErrStateCorrupt, direct.SessionID, ancestorID)
		}
		if err := checkSessionRunContract(events, stateDir, workspaceHash, ancestorID); err != nil {
			return err
		}
		projection, err := domain.Replay(events)
		if err != nil {
			return fmt.Errorf("%w: replay session ancestor %s: %v", ErrStateCorrupt, ancestorID, err)
		}
		if projection.WorkspaceHash != workspaceHash {
			return fmt.Errorf("%w: session ancestor %s belongs to another workspace", ErrStateCorrupt, ancestorID)
		}
		if projection.SessionID != direct.SessionID {
			return fmt.Errorf("%w: run %s crosses session boundary from %s", ErrStateCorrupt, direct.ID, ancestorID)
		}
		if !projection.Terminal() {
			return fmt.Errorf("%w: session %s contains non-terminal ancestor %s", ErrStateCorrupt, direct.SessionID, ancestorID)
		}
		ancestorID = projection.PrevRunID
	}
	return nil
}

// validateSessionForkSource 校验跨会话分支根的来源锚点：来源必须存在于当前
// 工作区、已终结、制品与冻结合同完整、事件边界与锚点一致，且其祖先链无环、
// 无跨会话冒充边。SessionID 若显式给出则必须尚未占用。
func validateSessionForkSource(ctx context.Context, journal interface {
	ports.Journal
	ports.RunLister
}, stateDir, workspaceHash string, options RunOptions) error {
	source := options.ForkSource
	if source == nil {
		return fmt.Errorf("%w: fork source is required", ErrSessionContinuation)
	}
	if source.RunID == "" {
		return fmt.Errorf("%w: fork source run ID is required", ErrSessionContinuation)
	}
	if source.WorkspaceHash != workspaceHash {
		return fmt.Errorf("%w: fork source %s belongs to another workspace", ErrSessionContinuation, source.RunID)
	}
	events, err := journal.Load(ctx, source.RunID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: load fork source %s: %v", ErrStateCorrupt, source.RunID, err)
	}
	if len(events) == 0 {
		return fmt.Errorf("%w: fork source %s not found", ErrSessionContinuation, source.RunID)
	}
	if err := checkSessionRunContract(events, stateDir, workspaceHash, source.RunID); err != nil {
		return err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return fmt.Errorf("%w: replay fork source %s: %v", ErrStateCorrupt, source.RunID, err)
	}
	if projection.WorkspaceHash != workspaceHash {
		return fmt.Errorf("%w: fork source %s belongs to another workspace", ErrSessionContinuation, source.RunID)
	}
	if !projection.Terminal() {
		return fmt.Errorf("%w: fork source %s is not terminal", ErrSessionContinuation, source.RunID)
	}
	if projection.SessionID == "" {
		return fmt.Errorf("%w: fork source %s has no session identity", ErrSessionContinuation, source.RunID)
	}
	last := events[len(events)-1]
	if last.Seq != source.LastSeq || "sha256:"+last.EventHash != source.LastEventHash {
		return fmt.Errorf("%w: fork source %s event boundary mismatch", ErrSessionContinuation, source.RunID)
	}
	if err := validateSessionAncestryAcyclic(ctx, journal, stateDir, workspaceHash, source.RunID); err != nil {
		return err
	}
	if options.SessionID != "" {
		items, listErr := listRunHistory(ctx, journal, workspaceHash)
		if listErr != nil {
			return listErr
		}
		for _, item := range items {
			if item.SessionID == options.SessionID {
				return fmt.Errorf("%w: session %s already has a root", ErrSessionContinuation, options.SessionID)
			}
		}
	}
	return nil
}

// validateSessionAncestryAcyclic 沿 prev_run_id 与会话根的 fork_source 回溯
// 完整祖先图，拒绝环、缺失节点和同时携带两种边的冒充记录。
func validateSessionAncestryAcyclic(ctx context.Context, journal ports.Journal, stateDir, workspaceHash string, start domain.RunID) error {
	visited := map[domain.RunID]struct{}{}
	current := start
	for current != "" {
		if _, duplicate := visited[current]; duplicate {
			return fmt.Errorf("%w: session ancestry contains a cycle at run %s", ErrStateCorrupt, current)
		}
		visited[current] = struct{}{}
		events, err := journal.Load(ctx, current)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: load ancestry run %s: %v", ErrStateCorrupt, current, err)
		}
		if len(events) == 0 {
			return fmt.Errorf("%w: ancestry references missing run %s", ErrStateCorrupt, current)
		}
		if err := checkSessionRunContract(events, stateDir, workspaceHash, current); err != nil {
			return err
		}
		projection, err := domain.Replay(events)
		if err != nil {
			return fmt.Errorf("%w: replay ancestry run %s: %v", ErrStateCorrupt, current, err)
		}
		if projection.WorkspaceHash != workspaceHash {
			return fmt.Errorf("%w: ancestry run %s belongs to another workspace", ErrStateCorrupt, current)
		}
		if projection.PrevRunID != "" && projection.ForkSource != nil {
			return fmt.Errorf("%w: run %s carries both prev_run_id and fork_source", ErrStateCorrupt, current)
		}
		if projection.PrevRunID != "" {
			current = projection.PrevRunID
			continue
		}
		if projection.ForkSource != nil {
			if projection.ForkSource.WorkspaceHash != workspaceHash {
				return fmt.Errorf("%w: ancestry fork source of %s belongs to another workspace", ErrStateCorrupt, current)
			}
			current = projection.ForkSource.RunID
			continue
		}
		current = ""
	}
	return nil
}

// ResolveSessionContinuation 只读验证指定历史运行是否为当前工作区某条完整
// 会话链的唯一终态链尾。返回值仅供下一次 Run 续链，不恢复历史回退能力。
func (service Service) ResolveSessionContinuation(ctx context.Context, workspacePath, stateDir string, configuration config.Config, runID domain.RunID) (SessionContinuation, error) {
	if runID == "" {
		return SessionContinuation{}, fmt.Errorf("%w: run ID is required", ErrSessionContinuation)
	}
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return SessionContinuation{}, err
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionContinuation{}, fmt.Errorf("%w: run %s not found", ErrSessionContinuation, runID)
		}
		if ctx.Err() != nil {
			return SessionContinuation{}, ctx.Err()
		}
		if sqlite.IsStateCorruptError(err) {
			return SessionContinuation{}, fmt.Errorf("%w: open run journal: %w", ErrStateCorrupt, err)
		}
		return SessionContinuation{}, err
	}
	defer journal.Close()
	return resolveSessionContinuation(ctx, journal, stateDir, workspaceHash, runID)
}

// resolveSessionContinuation 在已打开日志上重建并验证会话链；整条会话必须
// 使用当前 v2 冻结合同、连通、无环、无分叉且均已终结，所选运行必须是
// 唯一链尾。
func resolveSessionContinuation(ctx context.Context, journal interface {
	ports.Journal
	ports.RunLister
}, stateDir, workspaceHash string, runID domain.RunID) (SessionContinuation, error) {
	items, err := listRunHistory(ctx, journal, workspaceHash)
	if err != nil {
		return SessionContinuation{}, err
	}
	byID := make(map[domain.RunID]RunHistoryItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	selected, exists := byID[runID]
	if !exists {
		return SessionContinuation{}, fmt.Errorf("%w: run %s not found", ErrSessionContinuation, runID)
	}
	if selected.SessionID == "" {
		return SessionContinuation{}, fmt.Errorf("%w: run %s has no session identity", ErrSessionContinuation, runID)
	}
	members := make(map[domain.RunID]RunHistoryItem)
	children := make(map[domain.RunID]int)
	for _, item := range items {
		if item.PrevRunID != "" {
			parent, parentExists := byID[item.PrevRunID]
			if parentExists && parent.SessionID != item.SessionID {
				return SessionContinuation{}, fmt.Errorf("%w: run %s crosses session boundary from %s", ErrStateCorrupt, item.ID, item.PrevRunID)
			}
			children[item.PrevRunID]++
		}
		if item.SessionID != selected.SessionID {
			continue
		}
		if item.State != domain.RunCompleted && item.State != domain.RunFailed && item.State != domain.RunCancelled {
			return SessionContinuation{}, fmt.Errorf("%w: session %s contains non-terminal run %s", ErrSessionContinuation, selected.SessionID, item.ID)
		}
		members[item.ID] = item
	}
	for memberID := range members {
		events, err := journal.Load(ctx, memberID)
		if err != nil {
			return SessionContinuation{}, fmt.Errorf("%w: load session run %s: %v", ErrStateCorrupt, memberID, err)
		}
		if err := checkSessionRunContract(events, stateDir, workspaceHash, memberID); err != nil {
			return SessionContinuation{}, err
		}
	}
	for parent, count := range children {
		if _, selectedMember := members[parent]; !selectedMember {
			continue
		}
		if count > 1 {
			return SessionContinuation{}, fmt.Errorf("%w: session %s forks after run %s", ErrSessionContinuation, selected.SessionID, parent)
		}
	}
	for _, item := range members {
		if item.PrevRunID == "" {
			continue
		}
		if _, exists := members[item.PrevRunID]; !exists {
			return SessionContinuation{}, fmt.Errorf("%w: session %s references missing run %s", ErrStateCorrupt, selected.SessionID, item.PrevRunID)
		}
	}
	if children[runID] != 0 {
		return SessionContinuation{}, fmt.Errorf("%w: run %s is not the session tail", ErrSessionContinuation, runID)
	}
	visited := make(map[domain.RunID]struct{}, len(members))
	current := selected
	for {
		if _, duplicate := visited[current.ID]; duplicate {
			return SessionContinuation{}, fmt.Errorf("%w: session %s contains a cycle", ErrSessionContinuation, selected.SessionID)
		}
		visited[current.ID] = struct{}{}
		if current.PrevRunID == "" {
			break
		}
		current = members[current.PrevRunID]
	}
	if len(visited) != len(members) {
		return SessionContinuation{}, fmt.Errorf("%w: run %s is not the unique session tail", ErrSessionContinuation, runID)
	}
	return SessionContinuation{SessionID: selected.SessionID, PrevRunID: selected.ID}, nil
}

// ResolveSessionFork 只读验证指定历史运行能否作为新会话分支的来源，并返回
// 创建锚点。它不创建运行、不修改工作区，也不授予历史回退能力。
func (service Service) ResolveSessionFork(ctx context.Context, workspacePath, stateDir string, configuration config.Config, runID domain.RunID) (SessionForkAnchor, error) {
	if runID == "" {
		return SessionForkAnchor{}, fmt.Errorf("%w: run ID is required", ErrSessionContinuation)
	}
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return SessionForkAnchor{}, err
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionForkAnchor{}, fmt.Errorf("%w: run %s not found", ErrSessionContinuation, runID)
		}
		if ctx.Err() != nil {
			return SessionForkAnchor{}, ctx.Err()
		}
		if sqlite.IsStateCorruptError(err) {
			return SessionForkAnchor{}, fmt.Errorf("%w: open run journal: %w", ErrStateCorrupt, err)
		}
		return SessionForkAnchor{}, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		if ctx.Err() != nil {
			return SessionForkAnchor{}, ctx.Err()
		}
		return SessionForkAnchor{}, fmt.Errorf("%w: load fork source %s: %v", ErrStateCorrupt, runID, err)
	}
	if len(events) == 0 {
		return SessionForkAnchor{}, fmt.Errorf("%w: run %s not found", ErrSessionContinuation, runID)
	}
	if err := checkSessionRunContract(events, stateDir, workspaceHash, runID); err != nil {
		return SessionForkAnchor{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return SessionForkAnchor{}, fmt.Errorf("%w: replay fork source %s: %v", ErrStateCorrupt, runID, err)
	}
	if projection.WorkspaceHash != workspaceHash {
		return SessionForkAnchor{}, fmt.Errorf("%w: run %s belongs to another workspace", ErrSessionContinuation, runID)
	}
	if !projection.Terminal() {
		return SessionForkAnchor{}, fmt.Errorf("%w: run %s is not terminal", ErrSessionContinuation, runID)
	}
	if projection.SessionID == "" {
		return SessionForkAnchor{}, fmt.Errorf("%w: run %s has no session identity", ErrSessionContinuation, runID)
	}
	last := events[len(events)-1]
	if err := validateSessionAncestryAcyclic(ctx, journal, stateDir, workspaceHash, runID); err != nil {
		return SessionForkAnchor{}, err
	}
	return SessionForkAnchor{
		Source: domain.SessionForkSource{
			RunID:         projection.ID,
			WorkspaceHash: projection.WorkspaceHash,
			LastSeq:       last.Seq,
			LastEventHash: "sha256:" + last.EventHash,
		},
		SourceSessionID: projection.SessionID,
		SourceState:     projection.State,
		SourceTask:      projection.Task,
	}, nil
}

// ListSessionTree 只读投影当前工作区的会话树：同 session 内 prev_run_id 形成
// 兄弟/子链，跨 session 的 fork_source 形成分支边。索引可重建，不启动模型、
// 不连接外部 MCP、不写工作区。
func (service Service) ListSessionTree(ctx context.Context, workspacePath, stateDir string, configuration config.Config) (SessionTree, error) {
	if err := ctx.Err(); err != nil {
		return SessionTree{}, err
	}
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return SessionTree{}, err
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionTree{Nodes: map[domain.RunID]SessionTreeNode{}}, nil
		}
		if ctx.Err() != nil {
			return SessionTree{}, ctx.Err()
		}
		if sqlite.IsStateCorruptError(err) {
			return SessionTree{}, fmt.Errorf("%w: open run journal: %w", ErrStateCorrupt, err)
		}
		return SessionTree{}, err
	}
	defer journal.Close()
	return buildSessionTree(ctx, journal, workspaceHash)
}

// buildSessionTree 从已打开日志回放全部运行并组装会话树投影。
func buildSessionTree(ctx context.Context, journal interface {
	ports.Journal
	ports.RunLister
}, workspaceHash string) (SessionTree, error) {
	items, err := listRunHistory(ctx, journal, workspaceHash)
	if err != nil {
		return SessionTree{}, err
	}
	tree := SessionTree{Nodes: make(map[domain.RunID]SessionTreeNode, len(items))}
	for _, item := range items {
		tree.Nodes[item.ID] = SessionTreeNode{
			ID: item.ID, SessionID: item.SessionID, PrevRunID: item.PrevRunID, ForkSource: item.ForkSource,
			Task: item.Task, State: item.State, Phase: item.Phase, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}
	}
	children := make(map[domain.RunID][]domain.RunID)
	for _, item := range items {
		if item.PrevRunID == "" {
			continue
		}
		if _, exists := tree.Nodes[item.PrevRunID]; !exists {
			return SessionTree{}, fmt.Errorf("%w: session %s references missing run %s", ErrStateCorrupt, item.SessionID, item.PrevRunID)
		}
		children[item.PrevRunID] = append(children[item.PrevRunID], item.ID)
	}
	for id, node := range tree.Nodes {
		node.Children = children[id]
		tree.Nodes[id] = node
	}
	roots := make([]domain.RunID, 0)
	for _, item := range items {
		if item.PrevRunID == "" {
			roots = append(roots, item.ID)
		}
	}
	sort.Slice(roots, func(left, right int) bool {
		leftNode := tree.Nodes[roots[left]]
		rightNode := tree.Nodes[roots[right]]
		if leftNode.CreatedAt.Equal(rightNode.CreatedAt) {
			return leftNode.ID < rightNode.ID
		}
		return leftNode.CreatedAt.Before(rightNode.CreatedAt)
	})
	tree.Roots = roots
	return tree, nil
}

// checkSessionRunContract 在历史运行进入会话链或前情投影前校验全部制品引用
// 与当前 v2 冻结合同；旧 Config JSON、缺失 config_ref 或被篡改制品均按状态
// 损坏失败关闭。
func checkSessionRunContract(events []domain.Event, stateDir, workspaceHash string, runID domain.RunID) error {
	if err := checkRunArtifactReferences(events, stateDir, workspaceHash); err != nil {
		return fmt.Errorf("%w: validate session run %s artifacts: %v", ErrStateCorrupt, runID, err)
	}
	if err := checkFrozenRunContract(events, stateDir, workspaceHash); err != nil {
		return fmt.Errorf("%w: validate session run %s frozen config: %v", ErrStateCorrupt, runID, err)
	}
	return nil
}

// Evidence 返回一次运行的统一证据视图（V1.5）：以 contract.Result 为唯一
// 载体，从事件与冻结配置制品回放任务身份、运行身份、Provider 卡、终局、
