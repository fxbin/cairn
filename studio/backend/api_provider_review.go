// Provider 预设、准入与评审批注接口。
// Author: fxbin
package desktop

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
)

const (
	// defaultModelCatalogLimit 是模型目录查询未显式指定条数时的默认上限。
	defaultModelCatalogLimit = 200
	// maxModelCatalogLimit 是模型目录单次查询允许返回的最大模型条数。
	maxModelCatalogLimit = 500
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

// ModelCatalogRequest 描述 models.dev 模型目录查询与过滤条件。
type ModelCatalogRequest struct {
	Provider      string `json:"provider,omitempty"`
	Query         string `json:"query,omitempty"`
	ToolCallOnly  bool   `json:"tool_call_only,omitempty"`
	ReasoningOnly bool   `json:"reasoning_only,omitempty"`
	Refresh       bool   `json:"refresh,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}

// ModelCatalogProviderView 是目录中单个供应商的非敏感摘要投影；仅供预填参考。
type ModelCatalogProviderView struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	APIBaseURL string   `json:"api_base_url,omitempty"`
	EnvNames   []string `json:"env_names,omitempty"`
	ModelCount int      `json:"model_count"`
}

// ModelCatalogEntryView 是目录中单个模型的元数据投影；目录建议不构成已验证能力。
type ModelCatalogEntryView struct {
	Provider            string  `json:"provider"`
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	Family              string  `json:"family,omitempty"`
	ToolCall            bool    `json:"tool_call"`
	Reasoning           bool    `json:"reasoning"`
	Temperature         bool    `json:"temperature"`
	ContextTokens       int     `json:"context_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
}

// ModelCatalogView 是 models.dev 目录查询的完整返回结果。
type ModelCatalogView struct {
	Providers      []ModelCatalogProviderView `json:"providers"`
	Models         []ModelCatalogEntryView    `json:"models"`
	FetchedAt      string                     `json:"fetched_at,omitempty"`
	FromStaleCache bool                       `json:"from_stale_cache"`
	TotalProviders int                        `json:"total_providers"`
	TotalModels    int                        `json:"total_models"`
}

// ListModelCatalog 通过应用层门面加载或刷新 models.dev 目录，并返回稳定排序的供应商与模型建议列表。
func (api *API) ListModelCatalog(ctx context.Context, request ModelCatalogRequest) (ModelCatalogView, error) {
	if err := ctx.Err(); err != nil {
		return ModelCatalogView{}, err
	}
	limit := request.Limit
	if limit <= 0 {
		limit = defaultModelCatalogLimit
	}
	if limit > maxModelCatalogLimit {
		limit = maxModelCatalogLimit
	}
	options := app.ModelCatalogOptions{}
	if cachePath, err := app.ModelCatalogDefaultCachePath(); err == nil {
		options.CachePath = cachePath
	}
	var (
		catalog *app.ModelCatalog
		err     error
	)
	if request.Refresh {
		catalog, err = app.ModelCatalogRefresh(options)
	} else {
		catalog, err = app.ModelCatalogLoad(options)
	}
	if err != nil {
		return ModelCatalogView{}, err
	}

	providerInfos := append([]app.ProviderInfo(nil), catalog.Providers...)
	sort.Slice(providerInfos, func(i, j int) bool {
		if providerInfos[i].ModelCount != providerInfos[j].ModelCount {
			return providerInfos[i].ModelCount > providerInfos[j].ModelCount
		}
		return providerInfos[i].ID < providerInfos[j].ID
	})
	providers := make([]ModelCatalogProviderView, 0, len(providerInfos))
	for _, info := range providerInfos {
		providers = append(providers, ModelCatalogProviderView{
			ID:         info.ID,
			Name:       info.Name,
			APIBaseURL: info.APIBaseURL,
			EnvNames:   append([]string(nil), info.EnvNames...),
			ModelCount: info.ModelCount,
		})
	}

	providerID := strings.TrimSpace(request.Provider)
	query := strings.TrimSpace(request.Query)
	var candidates []app.ModelEntry
	switch {
	case providerID != "":
		candidates = catalog.EntriesOf(providerID)
		if query != "" {
			lowerQuery := strings.ToLower(query)
			matched := make([]app.ModelEntry, 0, len(candidates))
			for _, entry := range candidates {
				if matchesCatalogQuery(entry, lowerQuery) {
					matched = append(matched, entry)
				}
			}
			candidates = matched
		}
	case query != "":
		candidates = catalog.Lookup(query)
		if len(candidates) == 0 {
			lowerQuery := strings.ToLower(query)
			for _, entry := range catalog.Entries {
				if matchesCatalogQuery(entry, lowerQuery) {
					candidates = append(candidates, entry)
				}
			}
			sortCatalogEntries(candidates)
		}
	default:
		candidates = append([]app.ModelEntry(nil), catalog.Entries...)
		sortCatalogEntries(candidates)
	}

	filtered := make([]app.ModelEntry, 0, len(candidates))
	for _, entry := range candidates {
		if request.ToolCallOnly && !entry.ToolCall {
			continue
		}
		if request.ReasoningOnly && !entry.Reasoning {
			continue
		}
		filtered = append(filtered, entry)
	}
	totalModels := len(filtered)
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	models := make([]ModelCatalogEntryView, 0, len(filtered))
	for _, entry := range filtered {
		models = append(models, ModelCatalogEntryView{
			Provider:            entry.Provider,
			ID:                  entry.ID,
			Name:                entry.Name,
			Family:              entry.Family,
			ToolCall:            entry.ToolCall,
			Reasoning:           entry.Reasoning,
			Temperature:         entry.Temperature,
			ContextTokens:       entry.ContextTokens,
			OutputTokens:        entry.OutputTokens,
			InputUSDPerMillion:  entry.InputUSDPerMillion,
			OutputUSDPerMillion: entry.OutputUSDPerMillion,
		})
	}

	fetchedAt := ""
	if !catalog.FetchedAt.IsZero() {
		fetchedAt = catalog.FetchedAt.UTC().Format(time.RFC3339)
	}
	return ModelCatalogView{
		Providers:      providers,
		Models:         models,
		FetchedAt:      fetchedAt,
		FromStaleCache: catalog.FromStaleCache,
		TotalProviders: len(providers),
		TotalModels:    totalModels,
	}, nil
}

// matchesCatalogQuery 按大小写不敏感匹配模型 ID、展示名称或模型族。
func matchesCatalogQuery(entry app.ModelEntry, lowerQuery string) bool {
	return strings.Contains(strings.ToLower(entry.ID), lowerQuery) ||
		strings.Contains(strings.ToLower(entry.Name), lowerQuery) ||
		(entry.Family != "" && strings.Contains(strings.ToLower(entry.Family), lowerQuery))
}

// sortCatalogEntries 按供应商、模型 ID 与名称稳定升序排列目录条目。
func sortCatalogEntries(entries []app.ModelEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Provider != entries[j].Provider {
			return entries[i].Provider < entries[j].Provider
		}
		if entries[i].ID != entries[j].ID {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Name < entries[j].Name
	})
}

// SaveProviderPresetRequest 是保存命名连接预设的显式请求；只接收环境变量名引用。
type SaveProviderPresetRequest struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
	Protocol  string `json:"protocol,omitempty"`
	Admission string `json:"admission,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
	UserScope bool   `json:"user_scope,omitempty"`
}

// SaveProviderPreset 校验并原子保存命名连接预设；绝不接收或写入明文密钥。
func (api *API) SaveProviderPreset(_ context.Context, request SaveProviderPresetRequest) (OKResult, error) {
	name := strings.TrimSpace(request.Name)
	baseURL := strings.TrimSpace(request.BaseURL)
	model := strings.TrimSpace(request.Model)
	apiKeyEnv := strings.TrimSpace(request.APIKeyEnv)
	if name == "" || baseURL == "" || model == "" || apiKeyEnv == "" {
		return OKResult{}, errInvalid("name, base_url, model and api_key_env are required")
	}
	var path string
	if request.UserScope {
		userPath, err := config.UserConfigPath()
		if err != nil {
			return OKResult{}, err
		}
		path = userPath
	} else {
		abs, err := absWorkspace(request.Workspace)
		if err != nil {
			return OKResult{}, err
		}
		path = config.ResolveDefaultPath(abs)
	}
	protocol := strings.TrimSpace(request.Protocol)
	if protocol == "" {
		protocol = config.ProtocolOpenAIChatCompletions
	}
	preset := config.ProviderPreset{
		Protocol:  protocol,
		BaseURL:   baseURL,
		Model:     model,
		APIKeyEnv: apiKeyEnv,
	}
	if admission := strings.TrimSpace(request.Admission); admission != "" {
		preset.Admission = admission
	}
	if err := config.SaveProviderPreset(path, name, preset, request.Overwrite); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// RemoveProviderPreset 删除未激活的命名连接预设；当前激活项与缺失项失败关闭。
func (api *API) RemoveProviderPreset(_ context.Context, workspace, name string) (OKResult, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return OKResult{}, errInvalid("preset name is required")
	}
	abs, err := absWorkspace(workspace)
	if err != nil {
		return OKResult{}, err
	}
	path := config.ResolveDefaultPath(abs)
	if err := config.RemoveProviderPreset(path, trimmed); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// ProviderSetupRequest 是配置向导的显式输入。
type ProviderSetupRequest struct {
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	APIKeyEnv       string `json:"api_key_env"`
	Workspace       string `json:"workspace"`
	Admission       string `json:"admission,omitempty"`
	Overwrite       bool   `json:"overwrite,omitempty"`
	UserScope       bool   `json:"user_scope,omitempty"`
	CatalogURL      string `json:"catalog_url,omitempty"`
	CatalogCache    string `json:"catalog_cache,omitempty"`
	SavePreset      string `json:"save_preset,omitempty"`
	OverwritePreset bool   `json:"overwrite_preset,omitempty"`
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
	notes := append([]string(nil), outcome.Notes...)
	if presetName := strings.TrimSpace(request.SavePreset); presetName != "" {
		preset := config.ProviderPreset{
			Protocol:  config.ProtocolOpenAIChatCompletions,
			BaseURL:   strings.TrimSpace(request.BaseURL),
			Model:     strings.TrimSpace(request.Model),
			APIKeyEnv: strings.TrimSpace(request.APIKeyEnv),
		}
		if admission := strings.TrimSpace(request.Admission); admission != "" {
			preset.Admission = admission
		}
		if err := config.SaveProviderPreset(outcome.ConfigPath, presetName, preset, request.OverwritePreset); err != nil {
			return ProviderSetupView{}, err
		}
		notes = append(notes, "saved preset "+presetName)
	}
	return ProviderSetupView{
		ConfigPath:     outcome.ConfigPath,
		APIKeyEnv:      outcome.APIKeyEnv,
		CatalogMatched: outcome.CatalogMatched,
		ProbeRan:       outcome.ProbeRan,
		Notes:          notes,
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
