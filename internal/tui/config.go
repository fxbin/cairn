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
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/modelcatalog"
)

// wizardStep 是配置向导的步骤序数；转换只经 advance 与 retreat，禁止跳步，
// 保证每一步的输入在进入下一步前都已校验。
type wizardStep int

// 向导步骤全集：provider 选择、端点输入、模型选择、密钥环境变量名输入、
// 确认摘要、提交执行与终态展示。
const (
	stepProvider wizardStep = iota
	stepBaseURL
	stepModel
	stepAPIKeyEnv
	stepConfirm
	stepCommitting
	stepDone
)

// wizardStepCount 是向导可交互步骤总数（提交与终态不计入进度条）。
const wizardStepCount = 5

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
	selectedProvider  string
	suggestionCursor  int
	suggestions       []modelcatalog.ModelEntry
	baseURLInput      textinput.Model
	modelInput        textinput.Model
	apiKeyEnvInput    textinput.Model
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
// 入框预填默认值。
func NewConfigWizard(ctx context.Context, input app.SetupInput) ConfigWizard {
	baseURLInput := newTextinput("Provider Base URL")
	baseURLInput.SetValue(defaultBaseURL)
	apiKeyEnvInput := newTextinput("API Key 环境变量名")
	apiKeyEnvInput.SetValue(defaultAPIKeyEnv)
	wizard := ConfigWizard{
		ctx:              ctx,
		service:          app.NewSetupService(),
		input:            input,
		providerSearch:   newTextinput("搜索 Provider（可选）"),
		baseURLInput:     baseURLInput,
		modelInput:       newTextinput("模型名（如 glm-5.3）"),
		apiKeyEnvInput:   apiKeyEnvInput,
		catalogLoading:   true,
		suggestionCursor: -1,
	}
	wizard.providerSearch.Focus()
	if path, err := resolveExistingConfig(input.Workspace); err == nil {
		wizard.existingConfigTip = fmt.Sprintf("注意: %s 已存在——向导不覆盖既有配置，需先处理该文件", path)
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

// resolveExistingConfig 返回目标 workspace 的既有配置路径；不存在时返回错误。
func resolveExistingConfig(workspace string) (string, error) {
	path := configPath(workspace)
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

// configPath 与 SetupService 的落盘目标保持单一来源：空 workspace 即当前
// 目录语义由 config.DefaultPath 承担。
func configPath(workspace string) string {
	return config.DefaultPath(workspace)
}

// Init 启动向导：异步装载模型目录并启动搜索框光标闪烁。
func (wizard ConfigWizard) Init() tea.Cmd {
	return tea.Batch(loadCatalogCmd(wizard.input.CatalogURL, wizard.input.CatalogCacheDir), wizard.providerSearch.Focus())
}

// loadCatalogCmd 异步装载模型元数据目录；失败只影响建议列表，不阻塞向导。
func loadCatalogCmd(catalogURL, cacheDir string) tea.Cmd {
	options := modelcatalog.Options{CatalogURL: catalogURL}
	if cacheDir != "" {
		options.CachePath = cacheDir
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
// custom 与过滤结果间选择，Enter 选定并预填端点与密钥变量名后进入端点输入。
func (wizard ConfigWizard) updateProvider(msg tea.Msg) (tea.Model, tea.Cmd) {
	message, ok := msg.(tea.KeyMsg)
	if !ok {
		return wizard, nil
	}
	choiceCount := 1 + len(wizard.filteredProviders())
	switch message.String() {
	case "up", "k":
		wizard.providerCursor = (wizard.providerCursor + choiceCount - 1) % choiceCount
	case "down", "j":
		wizard.providerCursor = (wizard.providerCursor + 1) % choiceCount
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
		return wizard, cmd
	}
	return wizard, nil
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

// filteredProviders 按搜索词过滤目录 provider 摘要：空搜索词取排序后的前
// 缀清单；非空搜索词对标识与显示名做大小写不敏感的包含匹配。
func (wizard ConfigWizard) filteredProviders() []modelcatalog.ProviderInfo {
	if wizard.catalog == nil {
		return nil
	}
	return filterProviders(wizard.catalog.Providers, wizard.providerSearch.Value(), maxProviderChoices)
}

// filterProviders 是 provider 过滤的纯函数形态；目录摘要已按模型条数降序，
// 过滤结果按原顺序截断到上限。
func filterProviders(providers []modelcatalog.ProviderInfo, query string, limit int) []modelcatalog.ProviderInfo {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		if len(providers) > limit {
			return providers[:limit]
		}
		return providers
	}
	var matched []modelcatalog.ProviderInfo
	for _, provider := range providers {
		if strings.Contains(strings.ToLower(provider.ID), query) || strings.Contains(strings.ToLower(provider.Name), query) {
			matched = append(matched, provider)
			if len(matched) == limit {
				break
			}
		}
	}
	return matched
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

// moveSuggestionCursor 在目录建议列表内移动光标；无建议时不动。
func (wizard *ConfigWizard) moveSuggestionCursor(down bool) {
	if len(wizard.suggestions) == 0 {
		wizard.suggestionCursor = -1
		return
	}
	if wizard.suggestionCursor < 0 {
		if down {
			wizard.suggestionCursor = 0
		}
		return
	}
	if down {
		wizard.suggestionCursor = (wizard.suggestionCursor + 1) % len(wizard.suggestions)
	} else {
		wizard.suggestionCursor = (wizard.suggestionCursor + len(wizard.suggestions) - 1) % len(wizard.suggestions)
	}
}

// updateAPIKeyEnv 处理密钥环境变量名输入步：实时展示存在性状态，Enter 进
// 入确认页，Esc 返回模型步。
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
		wizard.step = stepConfirm
		wizard.apiKeyEnvInput.Blur()
	case "esc":
		wizard.step = stepModel
		wizard.apiKeyEnvInput.Blur()
		return wizard, wizard.modelInput.Focus()
	default:
		var cmd tea.Cmd
		wizard.apiKeyEnvInput, cmd = wizard.apiKeyEnvInput.Update(msg)
		return wizard, cmd
	}
	return wizard, nil
}

// updateConfirm 处理确认页：Enter 组装 SetupInput 并发起异步提交，Esc 返回
// 密钥环境变量名步。
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
		wizard.step = stepCommitting
		return wizard, commitCmd(wizard.service, wizard.ctx, wizard.input)
	case "esc":
		wizard.step = stepAPIKeyEnv
		return wizard, wizard.apiKeyEnvInput.Focus()
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
// 光标。
func (wizard *ConfigWizard) refreshSuggestions() {
	wizard.suggestions = filterSuggestions(wizard.catalog, wizard.selectedProvider, wizard.modelInput.Value())
	wizard.suggestionCursor = -1
}

// filterSuggestions 按查询词截取目录建议前缀清单；已选目录 provider 时建议
// 收窄到该 provider 的模型，custom 或未选时跨全目录。目录缺失或查询为空时
// 返回空切片，由调用方降级为纯手动输入。
func filterSuggestions(catalog *modelcatalog.Catalog, selectedProvider, query string) []modelcatalog.ModelEntry {
	if catalog == nil || strings.TrimSpace(query) == "" {
		return nil
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
	if len(entries) > maxModelSuggestions {
		entries = entries[:maxModelSuggestions]
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

// viewProvider 渲染 provider 选择列表：custom 固定置顶，目录 provider 按搜
// 索词过滤展示默认端点与模型条数。
func (wizard ConfigWizard) viewProvider() string {
	lines := []string{wizard.providerSearch.View(), "选择 Provider（↑↓ 移动，Enter 选定，输入即过滤）:"}
	cursorMark, style := "  ", itemStyle
	if wizard.providerCursor == 0 {
		cursorMark, style = "> ", itemSelectedStyle
	}
	lines = append(lines, style.Render(fmt.Sprintf("%s%s — %s", cursorMark, contract.ProviderIDCustom, providerCustomSummary)))
	for index, choice := range wizard.filteredProviders() {
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
	if wizard.catalogLoading {
		lines = append(lines, dimStyle.Render("目录装载中…"))
	} else if wizard.catalogErr != nil {
		lines = append(lines, warnStyle.Render("目录不可用（仅可选 custom 手动输入）"))
	}
	lines = append(lines, dimStyle.Render("目录清单与端点仅为预填建议，不构成已验证能力。"))
	return strings.Join(lines, "\n")
}

// viewModel 渲染模型输入与目录建议列表。
func (wizard ConfigWizard) viewModel() string {
	lines := []string{wizard.modelInput.View()}
	if wizard.catalogLoading {
		lines = append(lines, dimStyle.Render("目录装载中…（不可用时降级为手动输入）"))
	} else if wizard.catalogErr != nil {
		lines = append(lines, warnStyle.Render("目录不可用（手动输入模型名）: "+wizard.catalogErr.Error()))
	}
	if len(wizard.suggestions) > 0 {
		lines = append(lines, dimStyle.Render("目录建议（↑↓ 选择，Enter/Tab 采纳或按输入框原文继续）:"))
		for index, entry := range wizard.suggestions {
			cursor := "  "
			style := itemStyle
			if index == wizard.suggestionCursor {
				cursor = "> "
				style = itemSelectedStyle
			}
			lines = append(lines, style.Render(fmt.Sprintf("%s%s [%s] ctx %d / out %d / $%.2f $%.2f per M",
				cursor, entry.ID, entry.Provider, entry.ContextTokens, entry.OutputTokens, entry.InputUSDPerMillion, entry.OutputUSDPerMillion)))
		}
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

// viewConfirm 渲染提交前摘要：三要素、配置版本与付费探测告知。
func (wizard ConfigWizard) viewConfirm() string {
	lines := confirmLines(wizard.baseURLInput.Value(), wizard.modelInput.Value(), wizard.apiKeyEnvInput.Value(), wizard.input)
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

// viewDone 渲染终态页：成功展示结果与观测注记，失败展示错误与重试出口。
func (wizard ConfigWizard) viewDone() string {
	if wizard.commitErr != nil {
		return errStyle.Render("提交失败: "+wizard.commitErr.Error()) + "\n" + dimStyle.Render("Esc 重试 / Enter 退出")
	}
	lines := []string{okStyle.Render("配置已生成: " + wizard.outcome.ConfigPath)}
	if wizard.outcome.ProbeRan {
		lines = append(lines, okStyle.Render("实测探测已执行（详见注记）"))
	}
	for _, note := range wizard.outcome.Notes {
		lines = append(lines, "  "+note)
	}
	lines = append(lines, "", dimStyle.Render(fmt.Sprintf("下一步: export %s=<密钥> 后运行 cairn provider probe 或 cairn run", wizard.outcome.APIKeyEnv)), dimStyle.Render("Enter 退出"))
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
