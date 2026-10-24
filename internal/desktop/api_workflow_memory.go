// 工作流与记忆扩展接口。
// Author: fxbin
package desktop

import (
	"context"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

func (api *API) ListProjectInstructions(_ context.Context, workspace string) (ProjectInstructionsView, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return ProjectInstructionsView{}, err
	}
	ws, err := tools.NewWorkspace(abs, nil, 0, 0, 0)
	if err != nil {
		return ProjectInstructionsView{}, err
	}
	snapshot, err := tools.LoadProjectInstructions(ws)
	if err != nil {
		return ProjectInstructionsView{}, err
	}
	out := make([]ProjectInstructionDoc, 0, len(snapshot.Documents))
	for _, doc := range snapshot.Documents {
		excerpt := ""
		if len(doc.Content) > 200 {
			excerpt = string(doc.Content[:200]) + "…"
		} else {
			excerpt = string(doc.Content)
		}
		out = append(out, ProjectInstructionDoc{
			Source:  string(doc.Source),
			Path:    doc.Path,
			SHA256:  doc.SourceSHA256,
			Bytes:   len(doc.Content),
			Excerpt: excerpt,
		})
	}
	return ProjectInstructionsView{Documents: out}, nil
}

// ListWorkflows 列出工作区工作流定义。
func (api *API) ListWorkflows(_ context.Context, workspace string) ([]WorkflowDefinition, error) {
	if api.prefs == nil {
		return nil, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	return api.prefs.ListWorkflows(abs), nil
}

// SaveWorkflow 保存工作流定义。
func (api *API) SaveWorkflow(_ context.Context, def WorkflowDefinition) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(def.Workspace)
	if err != nil {
		return OKResult{}, err
	}
	def.Workspace = abs
	if def.ID == "" {
		def.ID = domain.NewID("wfl")
	}
	if def.MaxSteps == 0 {
		def.MaxSteps = 8
	}
	if err := api.prefs.SaveWorkflow(def); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// RunWorkflow 预演或启动工作流。
func (api *API) RunWorkflow(ctx context.Context, workspace, workflowID string, dryRun bool) (WorkflowRunAck, error) {
	return api.controller.RunWorkflow(ctx, workspace, workflowID, dryRun)
}

// MemoryCandidateView 是记忆候选的只读投影。
type MemoryCandidateView struct {
	ID                  string `json:"id"`
	Kind                string `json:"kind"`
	RunID               string `json:"run_id,omitempty"`
	Outcome             string `json:"outcome"`
	Status              string `json:"status"`
	TrustedVerification bool   `json:"trusted_verification"`
	CreatedAt           string `json:"created_at,omitempty"`
	Summary             string `json:"summary,omitempty"`
}

// ListMemories 召回当前工作区记忆候选；空结果与失败有区别。
func (api *API) ListMemories(ctx context.Context, request WorkspaceRequest) ([]MemoryCandidateView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return nil, err
	}
	candidates, err := api.service.RecallMemory(ctx, app.MemoryRecallOptions{
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
		Limit:     50,
	})
	if err != nil {
		return nil, err
	}
	out := make([]MemoryCandidateView, 0, len(candidates))
	for _, item := range candidates {
		out = append(out, MemoryCandidateView{
			ID:                  item.ID,
			Kind:                item.Kind,
			RunID:               item.RunID,
			Outcome:             item.Outcome,
			Status:              item.Status,
			TrustedVerification: item.TrustedVerification,
			CreatedAt:           item.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	return out, nil
}

// RevokeMemory 撤销记忆候选；原因使用有限枚举，不接受自由备注。
func (api *API) RevokeMemory(ctx context.Context, request struct {
	Workspace   string `json:"workspace"`
	StateDir    string `json:"state_dir,omitempty"`
	CandidateID string `json:"candidate_id"`
	Reason      string `json:"reason"`
}) (MemoryCandidateView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return MemoryCandidateView{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return MemoryCandidateView{}, err
	}
	item, err := api.service.RevokeMemory(ctx, app.MemoryRevokeOptions{
		CandidateID: request.CandidateID,
		Reason:      request.Reason,
		Workspace:   workspace,
		StateDir:    resolveStateDir(request.StateDir),
		Config:      configuration,
	})
	if err != nil {
		return MemoryCandidateView{}, err
	}
	return MemoryCandidateView{
		ID:      item.ID,
		Kind:    item.Kind,
		Outcome: item.Outcome,
		Status:  item.Status,
	}, nil
}

// CommandInfo 描述一个内置命令能力。
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
}

// ListCommands 返回与 CLI/GUI 同源的内置命令目录（只读）。
func (api *API) ListCommands(_ context.Context) ([]CommandInfo, error) {
	return []CommandInfo{
		{Name: "run", Description: "启动一次 Agent 运行", Scope: "workspace"},
		{Name: "resume", Description: "恢复/审批/对账/Abort", Scope: "workspace"},
		{Name: "doctor", Description: "只读诊断", Scope: "workspace"},
		{Name: "provider", Description: "Provider 配置/探测/准入", Scope: "user"},
		{Name: "mcp", Description: "MCP 配置与本机认证查询", Scope: "user"},
		{Name: "skills", Description: "技能目录与安装", Scope: "user/workspace"},
		{Name: "memory", Description: "记忆召回/整理/演化", Scope: "workspace"},
		{Name: "queue", Description: "后续队列提交/确认/取消", Scope: "workspace"},
		{Name: "bench", Description: "冻结评测（显式本地配置）", Scope: "benchmark"},
	}, nil
}

// InstallSkill 从明确来源安装技能到原生目录；不执行包内脚本。
func (api *API) InstallSkill(_ context.Context, workspace, scope, source string) (SkillSummary, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return SkillSummary{}, err
	}
	store, err := api.service.SkillStoreFor(abs, scope)
	if err != nil {
		return SkillSummary{}, err
	}
	name, err := store.Install(source)
	if err != nil {
		return SkillSummary{}, err
	}
	return SkillSummary{Name: name, Scope: scope}, nil
}

// SetSkillEnabled 启用或停用技能；只读共享来源不可写入。
func (api *API) SetSkillEnabled(_ context.Context, workspace, scope, name string, enabled bool) (OKResult, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return OKResult{}, err
	}
	store, err := api.service.SkillStoreFor(abs, scope)
	if err != nil {
		return OKResult{}, err
	}
	if err := store.SetEnabled(name, enabled); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// RemoveSkill 卸载技能；不删除会话历史。
func (api *API) RemoveSkill(_ context.Context, workspace, scope, name string) (OKResult, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return OKResult{}, err
	}
	store, err := api.service.SkillStoreFor(abs, scope)
	if err != nil {
		return OKResult{}, err
	}
	if err := store.Remove(name); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ConsolidateMemories 显式整理记忆派生候选；结果来自真实存储，不覆盖 Journal。
func (api *API) ConsolidateMemories(ctx context.Context, request WorkspaceRequest) (int, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return 0, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return 0, err
	}
	results, err := api.service.ConsolidateMemory(ctx, app.MemoryConsolidateOptions{
		Workspace: workspace,
		StateDir:  resolveStateDir(request.StateDir),
		Config:    configuration,
	})
	if err != nil {
		return 0, err
	}
	return len(results), nil
}

// SaveReviewNote 保存行级审查备注（本地元数据）。
