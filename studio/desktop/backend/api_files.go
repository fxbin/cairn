// 诊断、技能与文件接口。
// Author: fxbin
package desktop

import (
	"context"
	"strings"

	"github.com/fxbin/cairn/internal/app"
)

func (api *API) Doctor(ctx context.Context, request WorkspaceRequest) (app.DoctorReport, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return app.DoctorReport{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return app.DoctorReport{}, err
	}
	return api.service.Doctor(ctx, workspace, resolveStateDir(request.StateDir), configuration)
}

// MCPStatus 返回 MCP 声明与状态。
func (api *API) MCPStatus(_ context.Context, request WorkspaceRequest) ([]app.MCPServerStatus, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return nil, err
	}
	return api.service.MCPStatus(configuration)
}

// ListSkills 返回技能目录摘要。
func (api *API) ListSkills(_ context.Context, request WorkspaceRequest) ([]SkillSummary, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	catalog, err := api.service.Skills(workspace, true)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return []SkillSummary{}, nil
	}
	items := catalog.Entries()
	out := make([]SkillSummary, 0, len(items))
	for _, item := range items {
		out = append(out, SkillSummary{
			Name:        item.Name,
			Description: item.Description,
			Scope:       item.Scope,
			Disabled:    item.Disabled,
			ManualOnly:  item.ManualOnly,
		})
	}
	return out, nil
}

// SkillSummary 是技能只读摘要。
type SkillSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Disabled    bool   `json:"disabled"`
	ManualOnly  bool   `json:"manual_only"`
}

// ListFileTree 返回工作区有界文件树。
func (api *API) ListFileTree(_ context.Context, workspace string) (FileNode, error) {
	return ListFileTree(workspace)
}

// PreviewFile 返回有界文件预览。
func (api *API) PreviewFile(_ context.Context, workspace, relative string) (FilePreview, error) {
	return PreviewFile(workspace, relative)
}

// ListChanges 返回工作区改动列表（不猜测 Agent 归属）。
func (api *API) ListChanges(_ context.Context, workspace string) (ChangeSet, error) {
	return ListChanges(workspace)
}

// DiffFile 返回单文件统一 diff；scope 指定 staged/unstaged/untracked/all。
func (api *API) DiffFile(_ context.Context, workspace, relative string, scope string) (string, error) {
	return DiffFile(workspace, relative, DiffScope(scope))
}

// OpenPathInEditor 使用系统默认应用打开工作区内相对路径；与预览同一边界。
func (api *API) OpenPathInEditor(_ context.Context, workspace, relative string) (OKResult, error) {
	if err := OpenInEditor(workspace, relative); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "(空任务)"
	}
	if idx := strings.IndexAny(text, "\r\n"); idx >= 0 {
		return strings.TrimSpace(text[:idx])
	}
	return text
}

// Shutdown 有界收尾异步运行，供桌面退出钩子调用。

// ListOpenLocations 列出当前平台可用的「打开位置」目标。
func (api *API) ListOpenLocations(_ context.Context, workspace string) ([]OpenLocationTarget, error) {
	if _, err := absWorkspace(workspace); err != nil {
		return nil, err
	}
	return listOpenLocationTargets(), nil
}

// OpenInLocationWithTarget 用指定目标打开工作区路径。
func (api *API) OpenInLocationWithTarget(_ context.Context, workspace, relative, target string) (OKResult, error) {
	if err := OpenInLocation(workspace, relative, target); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}
