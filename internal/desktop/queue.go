package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
)

// 本文件承载队列、恢复对账与验证证据的桌面投影。全部动作经 app.Service，
// 不绕过 Policy，也不把 UI 开关当作权限。

// QueueRequest 是队列操作的公共定位参数。
type QueueRequest struct {
	Workspace string `json:"workspace"`
	StateDir  string `json:"state_dir,omitempty"`
}

// QueueSubmitRequest 向后续队列提交任务。
type QueueSubmitRequest struct {
	Workspace string `json:"workspace"`
	StateDir  string `json:"state_dir,omitempty"`
	Content   string `json:"content"`
	InputID   string `json:"input_id,omitempty"`
}

// QueueActionRequest 确认或取消队列项。
type QueueActionRequest struct {
	Workspace string `json:"workspace"`
	StateDir  string `json:"state_dir,omitempty"`
	InputID   string `json:"input_id"`
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// QueueItem 是队列项视图。
type QueueItem struct {
	InputID        string `json:"input_id"`
	State          string `json:"state"`
	TargetRunID    string `json:"target_run_id,omitempty"`
	Reason         string `json:"reason,omitempty"`
	ContentPreview string `json:"content_preview,omitempty"`
}

// ListQueue 返回持久后续队列。
func (api *API) ListQueue(ctx context.Context, request QueueRequest) ([]QueueItem, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return nil, err
	}
	items, err := api.service.ListQueue(ctx, workspace, resolveStateDir(request.StateDir), configuration)
	if err != nil {
		return nil, err
	}
	out := make([]QueueItem, 0, len(items))
	for _, item := range items {
		out = append(out, QueueItem{
			InputID:        item.InputID,
			State:          string(item.State),
			TargetRunID:    string(item.TargetRunID),
			Reason:         item.Reason,
			ContentPreview: item.ContentPreview,
		})
	}
	return out, nil
}

// SubmitQueue 提交后续任务；与“立即补充当前任务”（steering）语义分离。
func (api *API) SubmitQueue(ctx context.Context, request QueueSubmitRequest) (QueueItem, error) {
	if strings.TrimSpace(request.Content) == "" {
		return QueueItem{}, errInvalid("content is required")
	}
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return QueueItem{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return QueueItem{}, err
	}
	inputID := strings.TrimSpace(request.InputID)
	if inputID == "" {
		inputID = newInputID()
	}
	item, err := api.service.SubmitQueueInput(ctx, app.QueueSubmitOptions{
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
		Content:   request.Content,
		InputID:   inputID,
	})
	if err != nil {
		return QueueItem{}, err
	}
	return QueueItem{InputID: item.InputID, State: string(item.State), TargetRunID: string(item.TargetRunID), Reason: item.Reason}, nil
}

// ConfirmQueue 确认启动队列项；进入统一 RunController，事件与取消完整。
func (api *API) ConfirmQueue(ctx context.Context, request QueueActionRequest) (StartRunAck, error) {
	return api.controller.ConfirmQueue(ctx, request)
}

// CancelQueue 取消队列项。
func (api *API) CancelQueue(ctx context.Context, request QueueActionRequest) (QueueItem, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return QueueItem{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return QueueItem{}, err
	}
	item, err := api.service.CancelQueueInput(ctx, app.QueueCancelOptions{
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
		InputID:   request.InputID,
		Reason:    request.Reason,
	})
	if err != nil {
		return QueueItem{}, err
	}
	return QueueItem{InputID: item.InputID, State: string(item.State), TargetRunID: string(item.TargetRunID), Reason: item.Reason}, nil
}

// SessionTreeRequest 查询会话树。
type SessionTreeRequest struct {
	Workspace string `json:"workspace"`
	StateDir  string `json:"state_dir,omitempty"`
}

// SessionNode 是会话树节点。
type SessionNode struct {
	RunID     string   `json:"run_id"`
	SessionID string   `json:"session_id,omitempty"`
	PrevRunID string   `json:"prev_run_id,omitempty"`
	Task      string   `json:"task"`
	State     string   `json:"state"`
	Phase     string   `json:"phase,omitempty"`
	Children  []string `json:"children,omitempty"`
}

// SessionTreeView 是会话树投影。
type SessionTreeView struct {
	Nodes map[string]SessionNode `json:"nodes"`
	Roots []string               `json:"roots"`
}

// SessionTree 返回历史会话树，用于续接与 Fork 选择。
func (api *API) SessionTree(ctx context.Context, request SessionTreeRequest) (SessionTreeView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return SessionTreeView{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return SessionTreeView{}, err
	}
	tree, err := api.service.ListSessionTree(ctx, workspace, resolveStateDir(request.StateDir), configuration)
	if err != nil {
		return SessionTreeView{}, err
	}
	view := SessionTreeView{Nodes: map[string]SessionNode{}, Roots: make([]string, 0, len(tree.Roots))}
	for id, node := range tree.Nodes {
		children := make([]string, 0, len(node.Children))
		for _, child := range node.Children {
			children = append(children, string(child))
		}
		view.Nodes[string(id)] = SessionNode{
			RunID:     string(node.ID),
			SessionID: node.SessionID,
			PrevRunID: string(node.PrevRunID),
			Task:      node.Task,
			State:     string(node.State),
			Phase:     node.Phase,
			Children:  children,
		}
	}
	for _, root := range tree.Roots {
		view.Roots = append(view.Roots, string(root))
	}
	return view, nil
}

// EvidenceVerification 是完成证据中的验证摘要。
type EvidenceVerification struct {
	Seq uint64 `json:"verification_seq,omitempty"`
	OK  bool   `json:"verification_ok"`
}

// EvidenceMetrics 是非敏感用量摘要；未知项由前端显示为“—”。
type EvidenceMetrics struct {
	Turns              int   `json:"turns"`
	ToolCalls          int   `json:"tool_calls"`
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	UsageEstimated     bool  `json:"usage_estimated"`
	ContextCompactions int   `json:"context_compactions"`
	WallTimeMS         int64 `json:"wall_time_ms"`
}

// EvidenceView 是完成证据投影。
type EvidenceView struct {
	State        string               `json:"state"`
	Outcome      string               `json:"outcome"`
	FailureClass string               `json:"failure_class,omitempty"`
	StopReason   string               `json:"stop_reason,omitempty"`
	Trusted      bool                 `json:"trusted"`
	Verification EvidenceVerification `json:"verification"`
	Metrics      EvidenceMetrics      `json:"metrics"`
}

// Evidence 查询运行完成证据（不以自然语言回复代替）。
func (api *API) Evidence(ctx context.Context, request InspectRequest) (EvidenceView, error) {
	result, err := api.EvidenceFor(ctx, request)
	if err != nil {
		return EvidenceView{}, err
	}
	return EvidenceView{
		State:        result.State,
		Outcome:      result.Outcome,
		FailureClass: result.FailureClass,
		StopReason:   result.StopReason,
		Trusted:      result.Evidence.Trusted,
		Verification: EvidenceVerification{
			Seq: result.Evidence.VerificationSeq,
			OK:  result.Evidence.VerificationOK,
		},
		Metrics: EvidenceMetrics{
			Turns:              result.Metrics.Turns,
			ToolCalls:          result.Metrics.ToolCalls,
			InputTokens:        result.Metrics.InputTokens,
			OutputTokens:       result.Metrics.OutputTokens,
			UsageEstimated:     result.Metrics.UsageEstimated,
			ContextCompactions: result.Metrics.ContextCompactions,
			WallTimeMS:         result.Metrics.WallTimeMS,
		},
	}, nil
}

func newInputID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "q-fallback"
	}
	return "q-" + hex.EncodeToString(buf[:])
}

// 确保 domain 引用保持（队列状态可能为字符串）。
var _ = domain.RunID("")
