// Provider 预设、准入与评审批注接口。
// Author: fxbin
package desktop

import (
	"context"
	"github.com/fxbin/cairn/internal/config"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/app"
)

func (api *API) SaveReviewNote(_ context.Context, note ReviewNote) (OKResult, error) {
	if api.prefs == nil {
		return OKResult{}, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(note.Workspace)
	if err != nil {
		return OKResult{}, err
	}
	note.Workspace = abs
	if err := api.prefs.SaveReviewNote(note); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ListReviewNotes 列出审查备注。
func (api *API) ListReviewNotes(_ context.Context, workspace, relative string) ([]ReviewNote, error) {
	if api.prefs == nil {
		return nil, errInvalid("preferences store unavailable")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	return api.prefs.ListReviewNotes(abs, relative), nil
}

// ProviderProbeView 是显式探测的只读结论；不含密钥与消息正文。
type ProviderProbeView struct {
	EvidenceFingerprint       string `json:"evidence_fingerprint,omitempty"`
	ConfigFingerprint         string `json:"config_fingerprint,omitempty"`
	RequestProfileFingerprint string `json:"request_profile_fingerprint,omitempty"`
	ToolRoundtrip             string `json:"tool_roundtrip,omitempty"`
	UsageStreamed             bool   `json:"usage_streamed"`
	Result                    string `json:"result"`
	ProviderKind              string `json:"provider_kind,omitempty"`
	Model                     string `json:"model,omitempty"`
	Error                     string `json:"error,omitempty"`
}

// ProbeProvider 由用户显式发起合同探测；打开页面不自动消耗额度。
func (api *API) ProbeProvider(ctx context.Context, request WorkspaceRequest) (ProviderProbeView, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return ProviderProbeView{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return ProviderProbeView{}, err
	}
	result, err := api.service.CheckProvider(ctx, app.CheckProviderOptions{
		Config:     configuration,
		Workspace:  workspace,
		StateDir:   resolveStateDir(request.StateDir),
		ProbeTools: true,
	})
	if err != nil {
		return ProviderProbeView{}, err
	}
	return ProviderProbeView{
		EvidenceFingerprint: result.EvidenceFingerprint, ConfigFingerprint: result.ConfigFingerprint, RequestProfileFingerprint: result.RequestProfileFingerprint,
		ToolRoundtrip: result.ToolRoundtrip, UsageStreamed: result.UsageStreamed,
		Result:       result.Result,
		ProviderKind: result.ProviderKind,
		Model:        result.Model,
		Error:        result.Error,
	}, nil
}

// WriteProjectInstructions 写入工作区 AGENTS.md（原子、固定路径）。
func (api *API) WriteProjectInstructions(_ context.Context, workspace, content string) (OKResult, error) {
	if err := WriteProjectInstructions(workspace, content); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// MemoryDetail 是记忆详情投影。
type MemoryDetail struct {
	Candidate MemoryCandidateView `json:"candidate"`
	Audit     []string            `json:"audit,omitempty"`
}

// InspectMemory 查看记忆来源链与审计；不授予权限。
func (api *API) InspectMemory(ctx context.Context, workspace, candidateID string) (MemoryDetail, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return MemoryDetail{}, err
	}
	configuration, err := loadConfiguration(abs, "")
	if err != nil {
		return MemoryDetail{}, err
	}
	result, err := api.service.InspectMemory(ctx, app.MemoryInspectOptions{
		CandidateID: candidateID,
		Workspace:   abs,
		Config:      configuration,
	})
	if err != nil {
		return MemoryDetail{}, err
	}
	audit := make([]string, 0, len(result.Audit))
	for _, entry := range result.Audit {
		audit = append(audit, entry.Action)
	}
	return MemoryDetail{
		Candidate: MemoryCandidateView{
			ID:      result.Candidate.ID,
			Kind:    result.Candidate.Kind,
			RunID:   result.Candidate.RunID,
			Outcome: result.Candidate.Outcome,
			Status:  result.Candidate.Status,
		},
		Audit: audit,
	}, nil
}

// ProviderPresetView 是命名连接预设的非敏感投影。
type ProviderPresetView struct {
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	Model     string `json:"model"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	Admission string `json:"admission"`
	Active    bool   `json:"active"`
}

// ListProviderPresets 列出命名连接预设；不返回密钥值。
func (api *API) ListProviderPresets(_ context.Context, request WorkspaceRequest) ([]ProviderPresetView, error) {
	abs, err := absWorkspace(request.Workspace)
	if err != nil {
		return nil, err
	}
	configuration, err := loadConfiguration(abs, request.StateDir)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderPresetView, 0, len(configuration.Provider.Presets))
	for name, preset := range configuration.Provider.Presets {
		protocol := preset.Protocol
		if protocol == "" {
			protocol = "openai-chat-completions"
		}
		admission := preset.Admission
		if admission == "" {
			admission = "required"
		}
		out = append(out, ProviderPresetView{
			Name: name, Protocol: protocol, Model: preset.Model,
			BaseURL: preset.BaseURL, APIKeyEnv: preset.APIKeyEnv,
			Admission: admission, Active: configuration.Provider.Use == name,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UseProviderPreset 切换激活的命名连接预设；仅影响下一新 run。
func (api *API) UseProviderPreset(_ context.Context, workspace, name string) (OKResult, error) {
	abs, err := absWorkspace(workspace)
	if err != nil {
		return OKResult{}, err
	}
	if strings.TrimSpace(name) == "" {
		return OKResult{}, errInvalid("preset name is required")
	}
	configuration, err := loadConfiguration(abs, "")
	if err != nil {
		return OKResult{}, err
	}
	if _, exists := configuration.Provider.Presets[name]; !exists {
		return OKResult{}, errInvalid("preset is not defined in provider.presets")
	}
	// 与 CLI writeProviderUse 同语义：清除内联连接字段并写入 use。
	// 配置文件原子替换，失败不落盘。
	path := config.ResolveDefaultPath(abs)
	if err := config.UseProviderPreset(path, name); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ProviderSetupRequest 是配置向导的显式输入。
type ProviderSetupRequest struct {
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKeyEnv    string `json:"api_key_env"`
	Workspace    string `json:"workspace"`
	Admission    string `json:"admission,omitempty"`
	Overwrite    bool   `json:"overwrite,omitempty"`
	UserScope    bool   `json:"user_scope,omitempty"`
	CatalogURL   string `json:"catalog_url,omitempty"`
	CatalogCache string `json:"catalog_cache,omitempty"`
}

// ProviderSetupView 是配置生成结果的非敏感投影。
type ProviderSetupView struct {
	ConfigPath     string   `json:"config_path"`
	APIKeyEnv      string   `json:"api_key_env"`
	CatalogMatched bool     `json:"catalog_matched"`
	ProbeRan       bool     `json:"probe_ran"`
	Notes          []string `json:"notes,omitempty"`
}

// RunProviderSetup 生成 v2 配置；覆盖既有配置需显式 overwrite。
func (api *API) RunProviderSetup(ctx context.Context, request ProviderSetupRequest) (ProviderSetupView, error) {
	workspace := request.Workspace
	if strings.TrimSpace(workspace) == "" {
		return ProviderSetupView{}, errInvalid("workspace is required")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return ProviderSetupView{}, err
	}
	if strings.TrimSpace(request.BaseURL) == "" || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.APIKeyEnv) == "" {
		return ProviderSetupView{}, errInvalid("base_url, model and api_key_env are required")
	}
	outcome, err := app.NewSetupService().Run(ctx, app.SetupInput{
		BaseURL:           request.BaseURL,
		Model:             request.Model,
		APIKeyEnv:         request.APIKeyEnv,
		CatalogURL:        request.CatalogURL,
		CatalogCacheDir:   request.CatalogCache,
		Workspace:         abs,
		Admission:         request.Admission,
		OverwriteExisting: request.Overwrite,
		ScopeUserConfig:   request.UserScope,
	})
	if err != nil {
		return ProviderSetupView{}, err
	}
	return ProviderSetupView{
		ConfigPath:     outcome.ConfigPath,
		APIKeyEnv:      outcome.APIKeyEnv,
		CatalogMatched: outcome.CatalogMatched,
		ProbeRan:       outcome.ProbeRan,
		Notes:          outcome.Notes,
	}, nil
}

// ProviderAdmissionRequest 携带用户刚复核的非敏感绑定，不接受密钥或自造身份。
type ProviderAdmissionRequest struct {
	Workspace                 string `json:"workspace"`
	StateDir                  string `json:"state_dir,omitempty"`
	EvidenceFingerprint       string `json:"evidence_fingerprint"`
	ConfigFingerprint         string `json:"config_fingerprint"`
	RequestProfileFingerprint string `json:"request_profile_fingerprint"`
	Reason                    string `json:"reason"`
	Supersedes                string `json:"supersedes,omitempty"`
}

// ConfirmProviderAdmission 重新装载当前配置并记录一次用户显式准入决定。
func (api *API) ConfirmProviderAdmission(ctx context.Context, request ProviderAdmissionRequest) (OKResult, error) {
	workspace, err := absWorkspace(request.Workspace)
	if err != nil {
		return OKResult{}, err
	}
	configuration, err := loadConfiguration(workspace, request.StateDir)
	if err != nil {
		return OKResult{}, err
	}
	_, err = api.service.ConfirmProviderAdmission(ctx, app.ConfirmProviderAdmissionOptions{
		Workspace: workspace, StateDir: resolveStateDir(request.StateDir), Config: configuration,
		EvidenceFingerprint: request.EvidenceFingerprint, ConfigFingerprint: request.ConfigFingerprint,
		RequestProfileFingerprint: request.RequestProfileFingerprint, Reason: request.Reason, Supersedes: request.Supersedes,
	})
	if err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}
