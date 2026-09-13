// Package modelcatalog 承载外部模型元数据目录（models.dev 数据形态）的获
// 取、缓存与查询：首跑向导用它预填能力卡与预算上限，消解用户侧专家知识。
// 目录数据是预填建议而非事实源——生成的配置仍必须经端点实测探测与合同
// 校验；本包不发起模型调用、不触碰密钥。
//
// Author: fxbin
package modelcatalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultCatalogURL 是模型元数据目录的默认网络源；可用选项换镜像。
const DefaultCatalogURL = "https://models.dev/api.json"

// DefaultCachePath 返回目录缓存的默认磁盘位置；向导各装载点（TUI 启动与
// 配置提交）共用本函数作为缺省缓存路径，保证任何一次网络获取都会落缓存、
// 后续装载命中 24 小时窗口。
func DefaultCachePath() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "cairn", "models-catalog.json"), nil
}

// catalogTTL 是磁盘缓存的刷新间隔；目录仅在首跑向导时消费，24 小时的
// 陈旧度不构成问题，而更短的间隔只会在弱网下制造无谓的失败面。
const catalogTTL = 24 * time.Hour

// cacheSchemaV2 是缓存文件外层结构的当前版本；v2 相比 v1 新增 provider 摘
// 要投影，v1 缓存因缺少该投影而判不可用，由调用方走网络刷新。
const cacheSchemaV2 = 2

// ProviderInfo 是目录 provider 节点的闭合投影：默认端点与推荐密钥环境变
// 量名只作向导预填建议，不构成已验证事实；ModelCount 反映该 provider 在目
// 录中的模型条数，供列表排序与展示。
type ProviderInfo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	APIBaseURL string   `json:"api_base_url,omitempty"`
	EnvNames   []string `json:"env_names,omitempty"`
	ModelCount int      `json:"model_count"`
}

// ModelEntry 是向导消费的单模型元数据闭合投影：只声明使用的字段，目录
// 的其余字段在解码时被忽略；消费字段形态非法的条目在装载阶段跳过并计
// 数，绝不静默采用坏数据。
type ModelEntry struct {
	Provider            string  `json:"provider"`
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	Family              string  `json:"family"`
	ToolCall            bool    `json:"tool_call"`
	Reasoning           bool    `json:"reasoning"`
	Temperature         bool    `json:"temperature"`
	ContextTokens       int     `json:"context_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
}

// rawCatalogModel 与 rawCatalogProvider 是目录原始 JSON 的宽松解码形态：
// 只声明本包读取的键，外部 schema 演化（新增字段）不破坏解码；缺失的消
// 费字段在投影阶段判非法并跳过。
type rawCatalogModel struct {
	Name        string `json:"name"`
	Family      string `json:"family"`
	ToolCall    *bool  `json:"tool_call"`
	Reasoning   *bool  `json:"reasoning"`
	Temperature *bool  `json:"temperature"`
	Limit       *struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost *struct {
		Input  *float64 `json:"input"`
		Output *float64 `json:"output"`
	} `json:"cost"`
}

type rawCatalogProvider struct {
	Name   string                     `json:"name"`
	API    string                     `json:"api"`
	Env    []string                   `json:"env"`
	Models map[string]rawCatalogModel `json:"models"`
}

// cacheWrapper 是磁盘缓存的外层结构：版本字段使未来目录 schema 变更可
// 失败关闭而不是把异构数据喂给消费者。
type cacheWrapper struct {
	SchemaVersion int            `json:"schema_version"`
	FetchedAt     time.Time      `json:"fetched_at"`
	Entries       []ModelEntry   `json:"entries"`
	Providers     []ProviderInfo `json:"providers,omitempty"`
	SkippedCount  int            `json:"skipped_entries"`
	_             struct{}       `json:"-"`
}

// Options 描述一次目录装载的全部输入。
type Options struct {
	CatalogURL string
	CachePath  string
	Now        func() time.Time
	Transport  http.RoundTripper
}

// Catalog 是一次成功装载的目录实例。
type Catalog struct {
	Entries        []ModelEntry
	Providers      []ProviderInfo
	FetchedAt      time.Time
	FromStaleCache bool
	SkippedCount   int
}

// Load 装载目录：缓存新鲜即用缓存；否则尝试网络并原子更新缓存；网络失
// 败且存在陈旧缓存时降级返回（FromStaleCache 标注）；两者皆不可得才失败。
func Load(options Options) (*Catalog, error) {
	if options.CatalogURL == "" {
		options.CatalogURL = DefaultCatalogURL
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if cached, err := loadCache(options.CachePath); err == nil && options.Now().Sub(cached.FetchedAt) <= catalogTTL {
		return &Catalog{Entries: cached.Entries, Providers: cached.Providers, FetchedAt: cached.FetchedAt, SkippedCount: cached.SkippedCount}, nil
	}
	client := &http.Client{Transport: options.Transport, Timeout: catalogFetchTimeout}
	response, err := client.Get(options.CatalogURL)
	if err != nil {
		return staleFallback(options.CachePath, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return staleFallback(options.CachePath, fmt.Errorf("catalog endpoint returned %d", response.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, catalogMaxBytes+1))
	if err != nil {
		return staleFallback(options.CachePath, err)
	}
	if len(body) > catalogMaxBytes {
		return nil, fmt.Errorf("catalog payload exceeds the %d byte contract", catalogMaxBytes)
	}
	catalog, err := parseCatalog(body, options.Now().UTC())
	if err != nil {
		return staleFallback(options.CachePath, err)
	}
	// 缓存写失败只损失下次的新鲜度：网络成果已在内存中，降级返回而不作
	// 废整个获取。
	if options.CachePath != "" {
		_ = writeCache(options.CachePath, catalog)
	}
	return catalog, nil
}

// catalogFetchTimeout 与 catalogMaxBytes 是网络获取的边界：目录只在向导
// 时拉取一次，60 秒与 8 MiB 足以覆盖当前体量并给漂移留余量。
const (
	catalogFetchTimeout = 60 * time.Second
	catalogMaxBytes     = 8 << 20
)

// staleFallback 在网络路径失败后尝试陈旧缓存；无缓存则返回原始错误。
func staleFallback(cachePath string, cause error) (*Catalog, error) {
	if cachePath == "" {
		return nil, cause
	}
	cached, err := loadCache(cachePath)
	if err != nil {
		return nil, cause
	}
	return &Catalog{Entries: cached.Entries, Providers: cached.Providers, FetchedAt: cached.FetchedAt, FromStaleCache: true, SkippedCount: cached.SkippedCount}, nil
}

// parseCatalog 解码目录原文并投影为闭合条目：解码宽松、投影严格——消费
// 字段（tool_call、limit）缺失或非法的条目跳过计数；定价缺失按零处理
// （向导会展示并让用户按账单口径修正）。provider 节点同步投影为摘要列
// 表（默认端点与推荐密钥环境变量名仅供向导预填，不是已验证事实），模型
// 条数为零的 provider 不进入摘要。
func parseCatalog(body []byte, fetchedAt time.Time) (*Catalog, error) {
	var providers map[string]rawCatalogProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, fmt.Errorf("decode catalog payload: %w", err)
	}
	catalog := &Catalog{FetchedAt: fetchedAt}
	for providerKey, provider := range providers {
		modelCount := 0
		for modelKey, model := range provider.Models {
			entry, ok := projectEntry(providerKey, modelKey, model)
			if !ok {
				catalog.SkippedCount++
				continue
			}
			catalog.Entries = append(catalog.Entries, entry)
			modelCount++
		}
		if modelCount == 0 {
			continue
		}
		catalog.Providers = append(catalog.Providers, ProviderInfo{
			ID: providerKey, Name: provider.Name, APIBaseURL: provider.API,
			EnvNames: append([]string(nil), provider.Env...), ModelCount: modelCount,
		})
	}
	if len(catalog.Entries) == 0 {
		return nil, fmt.Errorf("catalog carries no consumable model entries (%d skipped)", catalog.SkippedCount)
	}
	sortProviders(catalog.Providers)
	return catalog, nil
}

// sortProviders 对 provider 摘要按模型条数降序排序，同数按标识升序，保证
// 向导列表呈现稳定可复现。
func sortProviders(providers []ProviderInfo) {
	sort.Slice(providers, func(i, j int) bool {
		if providers[i].ModelCount != providers[j].ModelCount {
			return providers[i].ModelCount > providers[j].ModelCount
		}
		return providers[i].ID < providers[j].ID
	})
}

// LookupProvider 按目录标识返回 provider 摘要；未知标识返回错误，不猜测。
func (catalog *Catalog) LookupProvider(id string) (ProviderInfo, error) {
	for _, provider := range catalog.Providers {
		if provider.ID == id {
			return provider, nil
		}
	}
	return ProviderInfo{}, fmt.Errorf("provider %q is not in the catalog", id)
}

// EntriesOf 列出指定 provider 的全部目录模型条目，按模型标识稳定排序；供
// 向导在选定 provider 后默认展示该家模型清单，仅是预填建议。
func (catalog *Catalog) EntriesOf(providerID string) []ModelEntry {
	var entries []ModelEntry
	for _, entry := range catalog.Entries {
		if entry.Provider == providerID {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

// projectEntry 把单条原始条目投影为闭合形态；tool_call 与 limit 为消费
// 必需字段，缺失即整条跳过。
func projectEntry(providerKey, modelKey string, model rawCatalogModel) (ModelEntry, bool) {
	if model.ToolCall == nil || model.Limit == nil || model.Limit.Context <= 0 {
		return ModelEntry{}, false
	}
	entry := ModelEntry{
		Provider: providerKey, ID: modelKey, Name: model.Name, Family: model.Family,
		ToolCall: *model.ToolCall, ContextTokens: model.Limit.Context,
	}
	if model.Reasoning != nil {
		entry.Reasoning = *model.Reasoning
	}
	if model.Temperature != nil {
		entry.Temperature = *model.Temperature
	}
	if model.Limit.Output > 0 {
		entry.OutputTokens = model.Limit.Output
	}
	if model.Cost != nil {
		if model.Cost.Input != nil {
			entry.InputUSDPerMillion = *model.Cost.Input
		}
		if model.Cost.Output != nil {
			entry.OutputUSDPerMillion = *model.Cost.Output
		}
	}
	return entry, true
}

// loadCache 读取并校验缓存文件；版本不符或结构损坏返回错误（调用方决定
// 是否走网络路径）。
func loadCache(path string) (*cacheWrapper, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wrapper cacheWrapper
	if err := json.Unmarshal(content, &wrapper); err != nil {
		return nil, fmt.Errorf("decode catalog cache: %w", err)
	}
	if wrapper.SchemaVersion != cacheSchemaV2 {
		return nil, fmt.Errorf("catalog cache schema_version %d is unsupported", wrapper.SchemaVersion)
	}
	if wrapper.FetchedAt.IsZero() || len(wrapper.Entries) == 0 {
		return nil, fmt.Errorf("catalog cache carries no usable payload")
	}
	return &wrapper, nil
}

// writeCache 以临时文件加原子重命名落盘缓存，目录按 0700 创建。
func writeCache(path string, catalog *Catalog) error {
	wrapper := cacheWrapper{SchemaVersion: cacheSchemaV2, FetchedAt: catalog.FetchedAt, Entries: catalog.Entries, Providers: catalog.Providers, SkippedCount: catalog.SkippedCount}
	encoded, err := json.Marshal(wrapper)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Lookup 按用户输入的模型名做跨 provider 模糊匹配：大小写与连字符归一
// 后比对模型名、ID 与族名；精确、部分、宽松匹配的优先级保持不变，每层
// 候选按 Provider、ID、Name 升序返回，避免目录输入顺序影响向导默认选择。
func (catalog *Catalog) Lookup(query string) []ModelEntry {
	normalized := normalizeModelName(query)
	if normalized == "" {
		return nil
	}
	var exact, partial []ModelEntry
	for _, entry := range catalog.Entries {
		name := normalizeModelName(entry.Name)
		id := normalizeModelName(entry.ID)
		family := normalizeModelName(entry.Family)
		if name == normalized || id == normalized {
			exact = append(exact, entry)
			continue
		}
		if strings.Contains(name, normalized) || strings.Contains(id, normalized) || (family != "" && strings.Contains(family, normalized)) {
			partial = append(partial, entry)
		}
	}
	if len(exact) > 0 {
		sortModelEntries(exact)
		return exact
	}
	if len(partial) > 0 {
		sortModelEntries(partial)
		return partial
	}
	// 点号变体兜底：gpt4.1 与 gpt-4.1 互查——两侧去掉点号与连字符后再比。
	dotsStripped := stripDotsAndDashes(normalized)
	if dotsStripped == "" {
		return nil
	}
	var loose []ModelEntry
	for _, entry := range catalog.Entries {
		if stripDotsAndDashes(normalizeModelName(entry.Name)) == dotsStripped || stripDotsAndDashes(normalizeModelName(entry.ID)) == dotsStripped {
			loose = append(loose, entry)
		}
	}
	sortModelEntries(loose)
	return loose
}

// sortModelEntries 按 provider、模型标识和展示名依次升序排列候选，保证
// Lookup 的同级匹配结果不依赖目录 map 遍历或手工 Entries 输入顺序。
func sortModelEntries(entries []ModelEntry) {
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

// stripDotsAndDashes 移除点号与连字符，供最宽松一级的形态匹配。
func stripDotsAndDashes(value string) string {
	value = strings.ReplaceAll(value, ".", "")
	return strings.ReplaceAll(value, "-", "")
}

// normalizeModelName 把模型名归一为可比形态：小写、空白与连字符统一。
func normalizeModelName(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	lowered = strings.ReplaceAll(lowered, "_", "-")
	lowered = strings.ReplaceAll(lowered, " ", "-")
	return lowered
}
