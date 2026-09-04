// Package tui 承载 Cairn 的薄终端交互层：模型配置向导与后续切片的单任务
// 运行闭环。本包只做展示与输入——配置生成、能力预填、实测探测与原子落盘
// 全部复用 internal/app 的 SetupService；密钥永不显示，UI 退出或中断不改变
// 任何既有事实，向导失败不会留下半配置。
//
// Author: fxbin
package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/credential"
	"github.com/fxbin/cairn/internal/modelcatalog"
)

// wizardStep 是配置向导的步骤序数；转换只经 advance 与 retreat，禁止跳步，
// 保证每一步的输入在进入下一步前都已校验。
type wizardStep int

// 向导步骤全集：provider 选择、端点输入、模型选择、密钥环境变量名输入、
// 密钥值输入、确认摘要、提交执行与终态展示。
const (
	stepProvider wizardStep = iota
	stepBaseURL
	stepModel
	stepAPIKeyEnv
	stepAPIKeySecret
	stepConfirm
	stepCommitting
	stepDone
)

// wizardStepCount 是向导可交互步骤总数（提交与终态不计入进度条）。
const wizardStepCount = 6

// maxModelSuggestions 是模型步同屏展示的目录建议上限；目录仅是预填建议，
// 超出上限的候选不出现在屏幕上，用户始终可以手输任意模型名。
const maxModelSuggestions = 8

// envStatusPresent 与 envStatusMissing 是密钥环境变量在当前进程环境中的
// 两态结论文案；只描述存在性，绝不回显变量值。
const (
	envStatusPresent = "已在当前环境设置"
	envStatusMissing = "未设置（提交时跳过实测探测）"
)

// probeNotice 是确认页对付费探测的前置告知：提交会经 SetupService 发起一次
// 极小请求，密钥缺失时自动跳过。
const probeNotice = "提交时将执行一次极小付费探测（数十 token；密钥未设置则跳过），并原子写入配置。"

// providerCustomSummary 是固定置顶的 custom 项说明：custom 是静态注册表的
// 显式端点成员，选择它时端点与密钥变量名保持向导默认值由用户手改。
const providerCustomSummary = "任意 OpenAI 兼容端点（手动输入 base_url）"

// maxProviderChoices 是 provider 步同屏展示的目录条目上限；搜索词过滤后的
// 超额条目不进入列表，用户始终可用更精确的搜索词或选 custom 手输。
const maxProviderChoices = 10

// catalogLoadedMsg 承载异步目录装载结果：目录不可用时 err 非空，向导降级为
// 纯手动输入，不阻塞流程。
type catalogLoadedMsg struct {
	catalog *modelcatalog.Catalog
	err     error
}

// commitFinishedMsg 承载异步提交结果：outcome 与 err 互斥，二者都非敏感。
type commitFinishedMsg struct {
	outcome *app.SetupOutcome
	err     error
}

// ConfigWizard 是模型配置向导的 bubbletea 模型：持有待补齐的 SetupInput 与
// 各步骤的输入状态，交互输入三要素后经 SetupService 一次性提交。ctx 是调
// 用方传入的进程上下文，提交与目录装载都在其下执行，取消语义由外层信号
// 纪律接管。
type ConfigWizard struct {
	ctx     context.Context
	service *app.SetupService
	input   app.SetupInput

	step wizardStep

	providerSearch    textinput.Model
	providerCursor    int
	providerWindow    int
	selectedProvider  string
	suggestionCursor  int
	suggestionWindow  int
	suggestions       []modelcatalog.ModelEntry
	baseURLInput      textinput.Model
	modelInput        textinput.Model
	apiKeyEnvInput    textinput.Model
	apiKeySecretInput textinput.Model
	catalogSpinner    spinner.Model
	existingConfigTip string

	catalog        *modelcatalog.Catalog
	catalogErr     error
	catalogLoading bool

	outcome   *app.SetupOutcome
	commitErr error
}

// defaultBaseURL 与 defaultAPIKeyEnv 是向导输入框的显式默认值：与 init 问
// 答一致，进入输入步即预填为可编辑内容，回车即接受——不使用仅作视觉提示
// 的 placeholder 承载默认值语义，避免"看着有值回车却无响应"的卡死观感。
const (
	defaultBaseURL   = "https://api.deepseek.com"
	defaultAPIKeyEnv = "AGENT_API_KEY"
)

// NewConfigWizard 构造向导模型：input 携带调用方决定的非交互项（workspace、
// 目录选项、配置版本与准入语义），三要素留给交互补齐；端点与密钥变量名输
// 入框预填默认值，密钥值输入框为密码回显且不预填。
func NewConfigWizard(ctx context.Context, input app.SetupInput) ConfigWizard {
	baseURLInput := newTextinput("Provider Base URL")
	baseURLInput.SetValue(defaultBaseURL)
	apiKeyEnvInput := newTextinput("API Key 环境变量名")
	apiKeyEnvInput.SetValue(defaultAPIKeyEnv)
	secretInput := newTextinput("密钥值（粘贴后回车；留空跳过）")
	secretInput.EchoMode = textinput.EchoPassword
	catalogSpinner := spinner.New(spinner.WithSpinner(spinner.Dot))
	wizard := ConfigWizard{
		ctx:               ctx,
		service:           app.NewSetupService(),
		input:             input,
		providerSearch:    newTextinput("搜索 Provider（可选）"),
		baseURLInput:      baseURLInput,
		modelInput:        newTextinput("模型名（如 glm-5.3）"),
		apiKeyEnvInput:    apiKeyEnvInput,
		apiKeySecretInput: secretInput,
		catalogSpinner:    catalogSpinner,
		catalogLoading:    true,
		suggestionCursor:  -1,
	}
	wizard.providerSearch.Focus()
	if path, err := resolveExistingConfig(wizard.input); err == nil {
		wizard.existingConfigTip = fmt.Sprintf("注意: %s 已存在——提交将覆盖它（旧文件备份为 .bak，手调字段会重置为最小配置）", path)
	}
	return wizard
}

// newTextinput 构造统一样式的单行输入框。
func newTextinput(prompt string) textinput.Model {
	field := textinput.New()
	field.Prompt = prompt + ": "
	field.CharLimit = 200
	field.Width = 48
	return field
}

// resolveExistingConfig 返回提交目标位置的既有配置路径；不存在时返回错
// 误。目标与 SetupService 的写入位置保持一致：用户级 scope 解析用户级目
// 录，否则工作区目录。
func resolveExistingConfig(input app.SetupInput) (string, error) {
	path, err := targetConfigPath(input)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

// targetConfigPath 按 scope 解析配置写入目标，语义与 SetupService.Run 的
// 落盘分支一致。
func targetConfigPath(input app.SetupInput) (string, error) {
	if input.ScopeUserConfig {
		return config.UserConfigPath()
	}
	return config.DefaultPath(input.Workspace), nil
}

// Init 启动向导：异步装载模型目录、启动搜索框光标闪烁与目录装载指示。
func (wizard ConfigWizard) Init() tea.Cmd {
	return tea.Batch(loadCatalogCmd(wizard.input.CatalogURL, wizard.input.CatalogCacheDir), wizard.providerSearch.Focus(), wizard.catalogSpinner.Tick)
}

// loadCatalogCmd 异步装载模型元数据目录；缺省缓存路径与配置提交路径共用
// 单一来源，保证 TUI 启动装载同样读写 24 小时磁盘缓存而不是每次走网络。
func loadCatalogCmd(catalogURL, cacheDir string) tea.Cmd {
	options := modelcatalog.Options{CatalogURL: catalogURL}
	if cacheDir != "" {
		options.CachePath = cacheDir
	} else if cachePath, err := modelcatalog.DefaultCachePath(); err == nil {
		options.CachePath = cachePath
	}
	return func() tea.Msg {
		catalog, err := modelcatalog.Load(options)
		return catalogLoadedMsg{catalog: catalog, err: err}
	}
}

// commitCmd 异步执行 SetupService 提交：目录预填、可选实测探测、落盘与回读
// 自证一体完成；结果经 commitFinishedMsg 回到事件循环。
func commitCmd(service *app.SetupService, ctx context.Context, input app.SetupInput) tea.Cmd {
	return func() tea.Msg {
		outcome, err := service.Run(ctx, input)
		return commitFinishedMsg{outcome: outcome, err: err}
	}
}

// Update 处理向导的全部按键与异步消息：Ctrl+C 任意时刻退出；Enter 推进、
// Esc 后退；提交步不可后退，终态步 Enter 退出。
func (wizard ConfigWizard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if message, ok := msg.(tea.KeyMsg); ok {
		switch message.String() {
		case "ctrl+c":
			return wizard, tea.Quit
		}
	}
	if catalogMsg, ok := msg.(catalogLoadedMsg); ok {
		return wizard.receiveCatalog(catalogMsg)
	}
	if tick, ok := msg.(spinner.TickMsg); ok {
		var cmd tea.Cmd
		wizard.catalogSpinner, cmd = wizard.catalogSpinner.Update(tick)
		if !wizard.catalogLoading {
			return wizard, nil
		}
		return wizard, cmd
	}
	if finished, ok := msg.(commitFinishedMsg); ok && wizard.step == stepCommitting {
		wizard.step = stepDone
		wizard.outcome = finished.outcome
		wizard.commitErr = finished.err
		return wizard, nil
	}
	switch wizard.step {
	case stepProvider:
		return wizard.updateProvider(msg)
	case stepBaseURL:
		return wizard.updateBaseURL(msg)
	case stepModel:
		return wizard.updateModel(msg)
	case stepAPIKeyEnv:
		return wizard.updateAPIKeyEnv(msg)
	case stepAPIKeySecret:
		return wizard.updateAPIKeySecret(msg)
	case stepConfirm:
		return wizard.updateConfirm(msg)
	case stepCommitting:
		return wizard.updateCommitting(msg)
	case stepDone:
		return wizard.updateDone(msg)
	}
	return wizard, nil
}

// updateProvider 处理 provider 选择步：搜索词过滤目录 provider，上下移动在
// custom 与过滤结果间线性选择并滚动展示窗口，Enter 选定并预填端点与密钥变
// 量名后进入端点输入。移动到边界即停，不循环——配合计数行明示还有更多
// 条目。
func (wizard ConfigWizard) updateProvider(msg tea.Msg) (tea.Model, tea.Cmd) {
	message, ok := msg.(tea.KeyMsg)
	if !ok {
		return wizard, nil
	}
	choiceCount := 1 + len(wizard.filteredProviders())
	switch message.String() {
	case "up", "k":
		if wizard.providerCursor > 0 {
			wizard.providerCursor--
		}
		wizard.scrollProviderWindow()
	case "down", "j":
		if wizard.providerCursor < choiceCount-1 {
			wizard.providerCursor++
		}
		wizard.scrollProviderWindow()
	case "enter":
		wizard.applyProviderChoice()
		wizard.step = stepBaseURL
		return wizard, wizard.baseURLInput.Focus()
	default:
		var cmd tea.Cmd
		wizard.providerSearch, cmd = wizard.providerSearch.Update(msg)
		if wizard.providerCursor >= choiceCount {
			wizard.providerCursor = choiceCount - 1
		}
		wizard.scrollProviderWindow()
		return wizard, cmd
	}
	return wizard, nil
}

// scrollProviderWindow 让展示窗口跟随光标：光标指向 custom 时窗口回顶，指
// 向目录条目时保证该条目落在窗口内；窗口上限为 maxProviderChoices 条目。
func (wizard *ConfigWizard) scrollProviderWindow() {
	if wizard.providerCursor == 0 {
		wizard.providerWindow = 0
		return
	}
	index := wizard.providerCursor - 1
	if index < wizard.providerWindow {
		wizard.providerWindow = index
	}
	if index >= wizard.providerWindow+maxProviderChoices {
		wizard.providerWindow = index - maxProviderChoices + 1
	}
}

// applyProviderChoice 按当前光标应用 provider 选择：custom 恢复向导默认值；
// 目录 provider 则把目录声明的默认端点与推荐密钥变量名预填进输入框——目
// 录值只是预填建议，用户可在后续步骤改写，也不构成任何准入事实。
func (wizard *ConfigWizard) applyProviderChoice() {
	if wizard.providerCursor == 0 {
		wizard.selectedProvider = ""
		wizard.baseURLInput.SetValue(defaultBaseURL)
		wizard.apiKeyEnvInput.SetValue(defaultAPIKeyEnv)
		return
	}
	filtered := wizard.filteredProviders()
	index := wizard.providerCursor - 1
	if index < 0 || index >= len(filtered) {
		wizard.selectedProvider = ""
		return
	}
	choice := filtered[index]
	wizard.selectedProvider = choice.ID
	if choice.APIBaseURL != "" {
		wizard.baseURLInput.SetValue(choice.APIBaseURL)
	}
	if len(choice.EnvNames) > 0 {
		wizard.apiKeyEnvInput.SetValue(choice.EnvNames[0])
	} else {
		wizard.apiKeyEnvInput.SetValue(defaultAPIKeyEnv)
	}
}

// filteredProviders 按搜索词筛选目录 provider 摘要：空搜索词返回精选常用
// 序，非空搜索词全目录包含匹配；展示截断由滚动窗口负责，过滤层不做丢弃。
func (wizard ConfigWizard) filteredProviders() []modelcatalog.ProviderInfo {
	if wizard.catalog == nil {
		return nil
	}
	return filterProviders(wizard.catalog.Providers, wizard.providerSearch.Value())
}

// featuredProviderIDs 是默认视图的常用 provider 精选序：直连厂商优先、主
// 流网关其次，面向本产品的主流使用场景排序。这是产品展示选择而非能力或
// 准入事实；目录中不存在的成员静默跳过，全量目录始终可以经搜索词到达。
var featuredProviderIDs = []string{
	"deepseek", "siliconflow-cn", "moonshotai", "alibaba",
	"openrouter", "siliconflow", "openai", "anthropic", "google",
}

// filterProviders 是 provider 过滤的纯函数形态：空搜索词返回精选序（目录
// 未收录的精选成员跳过），非空搜索词对标识与显示名做大小写不敏感的包含
// 匹配并保持目录排序——同屏展示条数由渲染窗口控制，过滤层不截断。
func filterProviders(providers []modelcatalog.ProviderInfo, query string) []modelcatalog.ProviderInfo {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return featuredProviders(providers)
	}
	var matched []modelcatalog.ProviderInfo
	for _, provider := range providers {
		if strings.Contains(strings.ToLower(provider.ID), query) || strings.Contains(strings.ToLower(provider.Name), query) {
			matched = append(matched, provider)
		}
	}
	return matched
}

// featuredProviders 按精选序取目录命中成员；目录缺失的精选成员静默跳过，
// 不猜测其端点或能力。
func featuredProviders(providers []modelcatalog.ProviderInfo) []modelcatalog.ProviderInfo {
	byID := make(map[string]modelcatalog.ProviderInfo, len(providers))
	for _, provider := range providers {
		byID[provider.ID] = provider
	}
	var featured []modelcatalog.ProviderInfo
	for _, id := range featuredProviderIDs {
		if provider, ok := byID[id]; ok {
			featured = append(featured, provider)
		}
	}
	return featured
}

// updateBaseURL 处理端点输入步：Enter 校验非空后进入模型步，Esc 返回。
func (wizard ConfigWizard) updateBaseURL(msg tea.Msg) (tea.Model, tea.Cmd) {
	message, ok := msg.(tea.KeyMsg)
	if !ok {
		return wizard, nil
	}
	switch message.String() {
	case "enter":
		if strings.TrimSpace(wizard.baseURLInput.Value()) == "" {
			return wizard, nil
		}
		wizard.step = stepModel
		wizard.baseURLInput.Blur()
		wizard.refreshSuggestions()
		return wizard, wizard.modelInput.Focus()
	case "esc":
		wizard.step = stepProvider
		wizard.baseURLInput.Blur()
	}
	return wizard, nil
}

// updateModel 处理模型选择步：输入即刷新目录建议；上下键在建议列表移动，
// Enter 采纳命中建议或输入框原文，Esc 返回。
func (wizard ConfigWizard) updateModel(msg tea.Msg) (tea.Model, tea.Cmd) {
	message, ok := msg.(tea.KeyMsg)
	if !ok {
		return wizard, nil
	}
	switch message.String() {
	case "up", "down":
		wizard.moveSuggestionCursor(message.String() == "down")
		return wizard, nil
	case "tab", "enter":
		if wizard.suggestionCursor >= 0 && wizard.suggestionCursor < len(wizard.suggestions) {
			wizard.modelInput.SetValue(wizard.suggestions[wizard.suggestionCursor].ID)
		}
		if strings.TrimSpace(wizard.modelInput.Value()) == "" {
			return wizard, nil
		}
		wizard.step = stepAPIKeyEnv
		wizard.modelInput.Blur()
		return wizard, wizard.apiKeyEnvInput.Focus()
	case "esc":
		wizard.step = stepBaseURL
		wizard.modelInput.Blur()
		return wizard, wizard.baseURLInput.Focus()
	default:
		var cmd tea.Cmd
		wizard.modelInput, cmd = wizard.modelInput.Update(msg)
		wizard.refreshSuggestions()
		return wizard, cmd
	}
}

// receiveCatalog 消费异步目录结果：成功填充建议，失败置降级提示。
func (wizard ConfigWizard) receiveCatalog(message catalogLoadedMsg) (tea.Model, tea.Cmd) {
	wizard.catalogLoading = false
	wizard.catalogErr = message.err
	wizard.catalog = message.catalog
	if wizard.step == stepModel {
		wizard.refreshSuggestions()
	}
	return wizard, nil
}

// moveSuggestionCursor 在目录建议列表内线性移动光标并让展示窗口跟随：到
// 边界即停不循环，配合计数行明示还有更多条目；无建议时不动。
func (wizard *ConfigWizard) moveSuggestionCursor(down bool) {
	if len(wizard.suggestions) == 0 {
		wizard.suggestionCursor = -1
		wizard.suggestionWindow = 0
		return
	}
	if wizard.suggestionCursor < 0 {
		if down {
			wizard.suggestionCursor = 0
		}
		wizard.scrollSuggestionWindow()
		return
	}
	if down && wizard.suggestionCursor < len(wizard.suggestions)-1 {
		wizard.suggestionCursor++
	}
	if !down && wizard.suggestionCursor > 0 {
		wizard.suggestionCursor--
	}
	wizard.scrollSuggestionWindow()
}

// scrollSuggestionWindow 让模型建议展示窗口跟随光标，窗口上限为
// maxModelSuggestions 条目。
func (wizard *ConfigWizard) scrollSuggestionWindow() {
	if wizard.suggestionCursor < wizard.suggestionWindow {
		wizard.suggestionWindow = wizard.suggestionCursor
	}
	if wizard.suggestionCursor >= wizard.suggestionWindow+maxModelSuggestions {
		wizard.suggestionWindow = wizard.suggestionCursor - maxModelSuggestions + 1
	}
	if wizard.suggestionWindow < 0 {
		wizard.suggestionWindow = 0
	}
}

// updateAPIKeyEnv 处理密钥环境变量名输入步：实时展示存在性状态，Enter 进
// 入密钥值输入步，Esc 返回模型步。
func (wizard ConfigWizard) updateAPIKeyEnv(msg tea.Msg) (tea.Model, tea.Cmd) {
	message, ok := msg.(tea.KeyMsg)
	if !ok {
		return wizard, nil
	}
	switch message.String() {
	case "enter":
		if strings.TrimSpace(wizard.apiKeyEnvInput.Value()) == "" {
			return wizard, nil
		}
		wizard.step = stepAPIKeySecret
		wizard.apiKeyEnvInput.Blur()
		return wizard, wizard.apiKeySecretInput.Focus()
	case "esc":
		wizard.step = stepModel
		wizard.apiKeyEnvInput.Blur()
		return wizard, wizard.modelInput.Focus()
	default:
		var cmd tea.Cmd
		wizard.apiKeyEnvInput, cmd = wizard.apiKeyEnvInput.Update(msg)
		return wizard, cmd
	}
}

// updateAPIKeySecret 处理密钥值输入步：密码回显、可空跳过，Enter 进入确认
// 页，Esc 返回变量名步。输入的密钥值在提交时才写盘，退出向导不产生任何
// 残留。
func (wizard ConfigWizard) updateAPIKeySecret(msg tea.Msg) (tea.Model, tea.Cmd) {
	message, ok := msg.(tea.KeyMsg)
	if !ok {
		return wizard, nil
	}
	switch message.String() {
	case "enter":
		wizard.step = stepConfirm
		wizard.apiKeySecretInput.Blur()
	case "esc":
		wizard.step = stepAPIKeyEnv
		wizard.apiKeySecretInput.Blur()
		return wizard, wizard.apiKeyEnvInput.Focus()
	default:
		var cmd tea.Cmd
		wizard.apiKeySecretInput, cmd = wizard.apiKeySecretInput.Update(msg)
		return wizard, cmd
	}
	return wizard, nil
}

// updateConfirm 处理确认页：Enter 组装 SetupInput、按需持久化密钥并发起异
// 步提交，Esc 返回密钥值输入步。
func (wizard ConfigWizard) updateConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	message, ok := msg.(tea.KeyMsg)
	if !ok {
		return wizard, nil
	}
	switch message.String() {
	case "enter":
		wizard.input.BaseURL = strings.TrimSpace(wizard.baseURLInput.Value())
		wizard.input.Model = strings.TrimSpace(wizard.modelInput.Value())
		wizard.input.APIKeyEnv = strings.TrimSpace(wizard.apiKeyEnvInput.Value())
		wizard.input.OverwriteExisting = true
		if secret := strings.TrimSpace(wizard.apiKeySecretInput.Value()); secret != "" {
			secretPath, err := keysPath(wizard.input)
			if err != nil {
				wizard.step = stepDone
				wizard.commitErr = fmt.Errorf("解析凭据文件路径失败（配置未提交）: %w", err)
				return wizard, nil
			}
			if err := credential.UpsertEnvFile(secretPath, wizard.input.APIKeyEnv, secret); err != nil {
				wizard.step = stepDone
				wizard.commitErr = fmt.Errorf("写入本机凭据文件失败（配置未提交）: %w", err)
				return wizard, nil
			}
			if os.Getenv(wizard.input.APIKeyEnv) == "" {
				if err := os.Setenv(wizard.input.APIKeyEnv, secret); err != nil {
					wizard.step = stepDone
					wizard.commitErr = fmt.Errorf("注入进程环境失败（配置未提交）: %w", err)
					return wizard, nil
				}
			}
		}
		wizard.step = stepCommitting
		return wizard, commitCmd(wizard.service, wizard.ctx, wizard.input)
	case "esc":
		wizard.step = stepAPIKeySecret
		return wizard, wizard.apiKeySecretInput.Focus()
	}
	return wizard, nil
}

// updateCommitting 处理提交执行步：结果消费在 Update 顶层完成，本步不响应
// 导航键，Ctrl+C 由全局处理退出。
func (wizard ConfigWizard) updateCommitting(msg tea.Msg) (tea.Model, tea.Cmd) {
	return wizard, nil
}

// updateDone 处理终态页：Enter 退出；提交失败时 Esc 允许回到开头重试。
func (wizard ConfigWizard) updateDone(msg tea.Msg) (tea.Model, tea.Cmd) {
	message, ok := msg.(tea.KeyMsg)
	if !ok {
		return wizard, nil
	}
	switch message.String() {
	case "enter":
		return wizard, tea.Quit
	case "esc":
		if wizard.commitErr != nil {
			wizard.step = stepProvider
			wizard.outcome = nil
			wizard.commitErr = nil
		}
	}
	return wizard, nil
}

// RunConfigWizard 以终端模式运行模型配置向导并返回进程级结论；ctx 承载取
// 消语义，非 TTY 环境由调用方先行拦截。
func RunConfigWizard(ctx context.Context, input app.SetupInput) error {
	program := tea.NewProgram(NewConfigWizard(ctx, input))
	_, err := program.Run()
	return err
}

// refreshSuggestions 按当前输入与所选 provider 刷新目录建议列表并重置建议
// 光标与窗口。
func (wizard *ConfigWizard) refreshSuggestions() {
	wizard.suggestions = filterSuggestions(wizard.catalog, wizard.selectedProvider, wizard.modelInput.Value())
	wizard.suggestionCursor = -1
	wizard.suggestionWindow = 0
}

// filterSuggestions 组装目录建议清单：已选目录 provider 且查询为空时默认列
// 出该家全部模型（按标识稳定排序）；查询非空时按名称模糊匹配，已选目录
// provider 时结果收窄到该家，custom 跨全目录；custom 且查询为空时无默认清
// 单（目录不承载 custom 端点的模型事实），由用户搜索或手输。过滤层不截
// 断，同屏条数由渲染窗口控制。
func filterSuggestions(catalog *modelcatalog.Catalog, selectedProvider, query string) []modelcatalog.ModelEntry {
	if catalog == nil {
		return nil
	}
	if strings.TrimSpace(query) == "" {
		if selectedProvider == "" {
			return nil
		}
		return catalog.EntriesOf(selectedProvider)
	}
	var entries []modelcatalog.ModelEntry
	if selectedProvider == "" {
		entries = catalog.Lookup(query)
	} else {
		for _, entry := range catalog.Lookup(query) {
			if entry.Provider == selectedProvider {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

// envPresent 判断密钥环境变量是否已在当前进程环境设置；只读存在性。
func envPresent(name string) bool {
	return os.Getenv(name) != ""
}

// View 渲染向导当前步骤的全屏内容。
func (wizard ConfigWizard) View() string {
	var sections []string
	sections = append(sections, titleStyle.Render("Cairn 模型配置向导")+dimStyle.Render("  (ctrl+c 退出)"))
	if wizard.existingConfigTip != "" {
		sections = append(sections, warnStyle.Render(wizard.existingConfigTip))
	}
	switch wizard.step {
	case stepProvider:
		sections = append(sections, wizard.viewProvider())
	case stepBaseURL:
		sections = append(sections, wizard.baseURLInput.View())
	case stepModel:
		sections = append(sections, wizard.viewModel())
	case stepAPIKeyEnv:
		sections = append(sections, wizard.viewAPIKeyEnv())
	case stepAPIKeySecret:
		sections = append(sections, wizard.viewAPIKeySecret())
	case stepConfirm:
		sections = append(sections, wizard.viewConfirm())
	case stepCommitting:
		sections = append(sections, wizard.viewCommitting())
	case stepDone:
		sections = append(sections, wizard.viewDone())
	}
	sections = append(sections, dimStyle.Render(fmt.Sprintf("步骤 %d/%d", wizard.progressOrdinal(), wizardStepCount)))
	return lipgloss.JoinVertical(lipgloss.Left, sections...) + "\n"
}

// progressOrdinal 返回当前可交互步骤序数（从 1 起）；提交与终态页固定为最
// 后一档。
func (wizard ConfigWizard) progressOrdinal() int {
	if wizard.step > stepConfirm {
		return wizardStepCount
	}
	return int(wizard.step) + 1
}

// viewProvider 渲染 provider 选择列表：custom 固定置顶，目录 provider 以滚
// 动窗口展示（窗口随光标滚动），底部计数行明示过滤结果总量，装载期间以
// 转圈指示占位并告知可直接选 custom 继续。
func (wizard ConfigWizard) viewProvider() string {
	lines := []string{wizard.providerSearch.View(), "选择 Provider（↑↓ 移动滚动，Enter 选定，输入即过滤）:"}
	cursorMark, style := "  ", itemStyle
	if wizard.providerCursor == 0 {
		cursorMark, style = "> ", itemSelectedStyle
	}
	lines = append(lines, style.Render(fmt.Sprintf("%s%s — %s", cursorMark, contract.ProviderIDCustom, providerCustomSummary)))
	filtered := wizard.filteredProviders()
	switch {
	case wizard.catalogLoading:
		lines = append(lines, dimStyle.Render(wizard.catalogSpinner.View()+" 正在拉取模型目录（约 2MB，24 小时内缓存；可直接选 custom 继续）…"))
	case wizard.catalogErr != nil:
		lines = append(lines, warnStyle.Render("目录不可用（仅可选 custom 手动输入）"))
	case len(filtered) == 0:
		lines = append(lines, dimStyle.Render("没有匹配的 Provider——换个关键词，或选 custom 手动输入。"))
	default:
		end := wizard.providerWindow + maxProviderChoices
		if end > len(filtered) {
			end = len(filtered)
		}
		for index := wizard.providerWindow; index < end; index++ {
			choice := filtered[index]
			cursorMark, style := "  ", itemStyle
			if index+1 == wizard.providerCursor {
				cursorMark, style = "> ", itemSelectedStyle
			}
			endpoint := choice.APIBaseURL
			if endpoint == "" {
				endpoint = "端点见厂商文档"
			}
			lines = append(lines, style.Render(fmt.Sprintf("%s%s [%s] · %d 个模型 · %s", cursorMark, choice.Name, choice.ID, choice.ModelCount, endpoint)))
		}
		if strings.TrimSpace(wizard.providerSearch.Value()) == "" {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("常用 %d 家 · 输入关键词搜索全部 %d 家", len(filtered), len(wizard.catalog.Providers))))
		} else {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("第 %d–%d 条 / 共 %d 家 · ↑↓ 滚动浏览", wizard.providerWindow+1, end, len(filtered))))
		}
	}
	lines = append(lines, dimStyle.Render("目录清单与端点仅为预填建议，不构成已验证能力。"))
	return strings.Join(lines, "\n")
}

// viewModel 渲染模型输入与目录建议列表。
func (wizard ConfigWizard) viewModel() string {
	lines := []string{wizard.modelInput.View()}
	if wizard.catalogLoading {
		lines = append(lines, dimStyle.Render(wizard.catalogSpinner.View()+" 目录装载中…（不可用时降级为手动输入）"))
	} else if wizard.catalogErr != nil {
		lines = append(lines, warnStyle.Render("目录不可用（手动输入模型名）: "+wizard.catalogErr.Error()))
	}
	switch {
	case len(wizard.suggestions) > 0:
		lines = append(lines, dimStyle.Render("目录模型（↑↓ 选择，Enter/Tab 采纳或按输入框原文继续）:"))
		end := wizard.suggestionWindow + maxModelSuggestions
		if end > len(wizard.suggestions) {
			end = len(wizard.suggestions)
		}
		for index := wizard.suggestionWindow; index < end; index++ {
			entry := wizard.suggestions[index]
			cursor := "  "
			style := itemStyle
			if index == wizard.suggestionCursor {
				cursor = "> "
				style = itemSelectedStyle
			}
			lines = append(lines, style.Render(fmt.Sprintf("%s%s [%s] ctx %d / out %d / $%.2f $%.2f per M",
				cursor, entry.ID, entry.Provider, entry.ContextTokens, entry.OutputTokens, entry.InputUSDPerMillion, entry.OutputUSDPerMillion)))
		}
		if len(wizard.suggestions) > maxModelSuggestions {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("第 %d–%d 个 / 共 %d 个 · ↑↓ 滚动浏览", wizard.suggestionWindow+1, end, len(wizard.suggestions))))
		}
	case wizard.catalog != nil && wizard.catalogErr == nil && wizard.selectedProvider == "" && strings.TrimSpace(wizard.modelInput.Value()) == "":
		lines = append(lines, dimStyle.Render("custom 端点没有目录模型清单——输入关键词搜索目录（如 glm、deepseek），或直接输入自定义模型名。"))
	}
	lines = append(lines, dimStyle.Render("建议仅为预填来源，能力以提交时的实测探测与合同校验为准。"))
	return strings.Join(lines, "\n")
}

// viewAPIKeyEnv 渲染密钥环境变量名输入与两态存在性提示。
func (wizard ConfigWizard) viewAPIKeyEnv() string {
	lines := []string{wizard.apiKeyEnvInput.View()}
	name := strings.TrimSpace(wizard.apiKeyEnvInput.Value())
	if name == "" {
		lines = append(lines, dimStyle.Render("只接受环境变量名——Cairn 永不读取或显示密钥值。"))
	} else if envPresent(name) {
		lines = append(lines, okStyle.Render(envStatusPresent))
	} else {
		lines = append(lines, warnStyle.Render(envStatusMissing))
	}
	return strings.Join(lines, "\n")
}

// viewAPIKeySecret 渲染密钥值输入步：密码回显、展示当前来源状态，输入值
// 只在本向导进程内流转，提交时才写盘。启动装载链（显式导出 > 工作区 >
// 用户级）已把两级凭据文件注入环境，因此环境存在性即代表全部已配置场景。
func (wizard ConfigWizard) viewAPIKeySecret() string {
	name := strings.TrimSpace(wizard.apiKeyEnvInput.Value())
	lines := []string{wizard.apiKeySecretInput.View()}
	switch {
	case name != "" && envPresent(name):
		lines = append(lines, okStyle.Render("该变量已配置（显式导出或本机凭据文件）——输入可覆盖凭据文件值（当前进程中的环境值优先）"))
	case name != "":
		lines = append(lines, dimStyle.Render("留空跳过：提交时不做实测探测，后续仍可写入或 export。"))
	}
	lines = append(lines, dimStyle.Render("输入的密钥只写入 0600 本机凭据文件并注入本进程，不进入配置、事件或任何提交内容。"))
	return strings.Join(lines, "\n")
}

// keysPath 返回密钥值写入目标：与配置写入目标同 scope（用户级配置写用户
// 级凭据文件，工作区配置写工作区凭据文件），保持配置与凭据的可解析性一
// 致。
func keysPath(input app.SetupInput) (string, error) {
	if input.ScopeUserConfig {
		return credential.UserKeysPath()
	}
	return filepath.Join(input.Workspace, credential.KeysFileName), nil
}

// viewConfirm 渲染提交前摘要：三要素、凭据去向、配置版本与付费探测告知。
func (wizard ConfigWizard) viewConfirm() string {
	lines := confirmLines(wizard.baseURLInput.Value(), wizard.modelInput.Value(), wizard.apiKeyEnvInput.Value(), wizard.input)
	scope := "工作区"
	if wizard.input.ScopeUserConfig {
		scope = "用户级（全部工作区可用）"
	}
	lines = append(lines, fmt.Sprintf("  写入位置:     %s", scope))
	if strings.TrimSpace(wizard.apiKeySecretInput.Value()) != "" {
		if secretPath, err := keysPath(wizard.input); err == nil {
			lines = append(lines, fmt.Sprintf("  密钥:         写入本机凭据文件 %s（0600）", secretPath))
		}
	}
	lines = append(lines, "", warnStyle.Render(probeNotice), "", dimStyle.Render("Enter 提交 / Esc 返回"))
	return strings.Join(lines, "\n")
}

// confirmLines 组装确认页摘要行：全部内容非敏感。
func confirmLines(baseURL, model, apiKeyEnv string, input app.SetupInput) []string {
	version := "1"
	if input.ConfigVersion == 2 {
		version = "2（" + input.Admission + "）"
	}
	envTail := envStatusMissing
	if envPresent(strings.TrimSpace(apiKeyEnv)) {
		envTail = envStatusPresent
	}
	return []string{
		"将写入以下配置：",
		fmt.Sprintf("  base_url:     %s", strings.TrimSpace(baseURL)),
		fmt.Sprintf("  model:        %s", strings.TrimSpace(model)),
		fmt.Sprintf("  api_key_env:  %s（%s）", strings.TrimSpace(apiKeyEnv), envTail),
		fmt.Sprintf("  version:      %s", version),
	}
}

// viewCommitting 渲染提交执行页。
func (wizard ConfigWizard) viewCommitting() string {
	return dimStyle.Render("提交中：目录预填 → 实测探测 → 原子落盘 → 回读自证…")
}

// viewDone 渲染终态页：成功展示完成状态、观测注记与下一步指引（不暴露配
// 置文件路径——对向导用户那是内部细节），失败展示错误与重试出口。
func (wizard ConfigWizard) viewDone() string {
	if wizard.commitErr != nil {
		return errStyle.Render("提交失败: "+wizard.commitErr.Error()) + "\n" + dimStyle.Render("Esc 重试 / Enter 退出")
	}
	scope := "对所有工作区可用"
	if !wizard.input.ScopeUserConfig {
		scope = "仅当前工作区"
	}
	lines := []string{okStyle.Render("配置完成（" + scope + "）")}
	if wizard.outcome.ProbeRan {
		lines = append(lines, okStyle.Render("端点实测探测已执行（结果见注记）"))
	}
	for _, note := range wizard.outcome.Notes {
		lines = append(lines, "  "+note)
	}
	lines = append(lines, "")
	if envPresent(wizard.outcome.APIKeyEnv) {
		lines = append(lines, dimStyle.Render(`下一步: cairn run "你的任务" 即可开始使用`))
	} else {
		lines = append(lines, warnStyle.Render(fmt.Sprintf("尚未配置密钥（%s）——重跑向导粘贴，或 export 该变量后直接使用", wizard.outcome.APIKeyEnv)))
	}
	lines = append(lines, dimStyle.Render("Enter 退出"))
	return strings.Join(lines, "\n")
}

// titleStyle、dimStyle、okStyle、warnStyle、errStyle、itemStyle 与
// itemSelectedStyle 是向导的语义化文本样式集合。
var (
	titleStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	okStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warnStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	itemStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	itemSelectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
)
