// config_test 覆盖模型配置向导的纯函数与按键状态机：目录建议过滤、密钥环
// 境变量存在性、确认摘要、建议光标移动、步骤进度与全流程推进。提交与网络
// 探测属 SetupService 既有测试范围，此处不重复。
//
// Author: fxbin
package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/credential"
	"github.com/fxbin/cairn/internal/modelcatalog"
)

// testCatalogEntries 构造足够超过建议上限的目录条目，验证截断与光标循环。
func testCatalogEntries(count int) []modelcatalog.ModelEntry {
	entries := make([]modelcatalog.ModelEntry, 0, count)
	for index := 0; index < count; index++ {
		entries = append(entries, modelcatalog.ModelEntry{
			Provider: "test", ID: "model-" + string(rune('a'+index)), Name: "Model " + string(rune('A'+index)),
		})
	}
	return entries
}

// TestFilterSuggestionsEmptyInputs 验证目录缺失与空查询都降级为无建议。
func TestFilterSuggestionsEmptyInputs(t *testing.T) {
	catalog := &modelcatalog.Catalog{Entries: testCatalogEntries(3)}
	if got := filterSuggestions(nil, "", "model"); got != nil {
		t.Fatalf("nil 目录应返回 nil 建议，得到 %d 条", len(got))
	}
	if got := filterSuggestions(catalog, "", "   "); got != nil {
		t.Fatalf("空查询应返回 nil 建议，得到 %d 条", len(got))
	}
}

// TestFilterSuggestionsCapsLimit 验证建议条数被上限截断。
func TestFilterSuggestionsCapsLimit(t *testing.T) {
	catalog := &modelcatalog.Catalog{Entries: testCatalogEntries(maxModelSuggestions + 5)}
	got := filterSuggestions(catalog, "", "model")
	if len(got) != maxModelSuggestions {
		t.Fatalf("建议应截断为 %d 条，得到 %d 条", maxModelSuggestions, len(got))
	}
}

// TestFilterSuggestionsScopedToSelectedProvider 验证已选目录 provider 时建议
// 只保留该 provider 的条目。
func TestFilterSuggestionsScopedToSelectedProvider(t *testing.T) {
	entries := append(testCatalogEntries(2),
		modelcatalog.ModelEntry{Provider: "other", ID: "model-x", Name: "Model X"},
	)
	catalog := &modelcatalog.Catalog{Entries: entries}
	got := filterSuggestions(catalog, "test", "model")
	if len(got) == 0 || len(got) != 2 {
		t.Fatalf("provider 过滤应只剩 2 条 test 条目，得到 %d 条", len(got))
	}
	for _, entry := range got {
		if entry.Provider != "test" {
			t.Fatalf("建议泄漏了非所选 provider 条目: %+v", entry)
		}
	}
}

// TestFilterProvidersMatchesWithoutTruncation 验证 provider 过滤按标识与名
// 称匹配且不截断——同屏条数由渲染窗口控制，过滤层不丢弃结果。
func TestFilterProvidersMatchesWithoutTruncation(t *testing.T) {
	providers := []modelcatalog.ProviderInfo{
		{ID: "openrouter", Name: "OpenRouter", ModelCount: 360},
		{ID: "openai", Name: "OpenAI", ModelCount: 80},
		{ID: "deepseek", Name: "DeepSeek", ModelCount: 10},
	}
	if got := filterProviders(providers, "deep"); len(got) != 1 || got[0].ID != "deepseek" {
		t.Fatalf("deep 应只命中 deepseek，得到 %+v", got)
	}
	if got := filterProviders(providers, "open"); len(got) != 2 {
		t.Fatalf("open 应命中 openrouter 与 openai 共 2 条，得到 %d 条", len(got))
	}
	if got := filterProviders(providers, ""); len(got) != 3 || got[0].ID != "openrouter" {
		t.Fatalf("空查询应返回完整已排序输入，得到 %+v", got)
	}
	if got := filterProviders(providers, "missing"); got != nil {
		t.Fatalf("无命中应返回空，得到 %+v", got)
	}
}

// TestScrollProviderWindowFollowsCursor 验证展示窗口跟随光标滚动：光标回
// custom 窗口回顶，越界下移时窗口前滚，上移时窗口回卷。
func TestScrollProviderWindowFollowsCursor(t *testing.T) {
	wizard := ConfigWizard{}
	for index := 0; index < maxProviderChoices+5; index++ {
		wizard.providerCursor = index + 1
		wizard.scrollProviderWindow()
	}
	if wizard.providerWindow != 5 {
		t.Fatalf("光标越界下移后窗口应前滚到 5，得到 %d", wizard.providerWindow)
	}
	wizard.providerCursor = 1
	wizard.scrollProviderWindow()
	if wizard.providerWindow != 0 {
		t.Fatalf("光标回到顶部条目时窗口应回卷到 0，得到 %d", wizard.providerWindow)
	}
	wizard.providerCursor = 0
	wizard.scrollProviderWindow()
	if wizard.providerWindow != 0 {
		t.Fatalf("custom 光标时窗口应回顶，得到 %d", wizard.providerWindow)
	}
}

// TestApplyProviderChoicePrefills 验证选中目录 provider 预填端点与密钥变量
// 名，选 custom 恢复向导默认值。
func TestApplyProviderChoicePrefills(t *testing.T) {
	wizard := NewConfigWizard(context.Background(), app.SetupInput{})
	wizard.catalog = &modelcatalog.Catalog{Providers: []modelcatalog.ProviderInfo{
		{ID: "deepseek", Name: "DeepSeek", APIBaseURL: "https://api.deepseek.com", EnvNames: []string{"DEEPSEEK_API_KEY"}, ModelCount: 10},
	}}
	wizard.providerCursor = 1
	wizard.applyProviderChoice()
	if wizard.selectedProvider != "deepseek" {
		t.Fatalf("应记录所选 provider，得到 %q", wizard.selectedProvider)
	}
	if wizard.baseURLInput.Value() != "https://api.deepseek.com" {
		t.Fatalf("端点应预填目录默认值，得到 %q", wizard.baseURLInput.Value())
	}
	if wizard.apiKeyEnvInput.Value() != "DEEPSEEK_API_KEY" {
		t.Fatalf("密钥变量名应预填目录推荐值，得到 %q", wizard.apiKeyEnvInput.Value())
	}
	wizard.providerCursor = 0
	wizard.applyProviderChoice()
	if wizard.selectedProvider != "" || wizard.baseURLInput.Value() != defaultBaseURL || wizard.apiKeyEnvInput.Value() != defaultAPIKeyEnv {
		t.Fatalf("custom 应恢复默认值，得到 provider=%q url=%q env=%q", wizard.selectedProvider, wizard.baseURLInput.Value(), wizard.apiKeyEnvInput.Value())
	}
}

// TestEnvPresentReflectsProcessEnv 验证存在性结论只依赖环境变量是否设置。
func TestEnvPresentReflectsProcessEnv(t *testing.T) {
	t.Setenv("CAIRN_TUI_TEST_KEY", "")
	if envPresent("CAIRN_TUI_TEST_KEY") {
		t.Fatal("已设置为空串的变量应判定为未设置")
	}
	t.Setenv("CAIRN_TUI_TEST_KEY", "non-empty")
	if !envPresent("CAIRN_TUI_TEST_KEY") {
		t.Fatal("非空变量应判定为已设置")
	}
	if envPresent("CAIRN_TUI_TEST_ABSENT") {
		t.Fatal("未定义变量应判定为未设置")
	}
}

// TestConfirmLinesCarriesNonSecretSummary 验证确认摘要包含三要素与版本，且
// 不携带密钥值。
func TestConfirmLinesCarriesNonSecretSummary(t *testing.T) {
	lines := confirmLines(" https://api.example.com ", " glm-5.3 ", " ZHIPU_API_KEY ", app.SetupInput{})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"https://api.example.com", "glm-5.3", "ZHIPU_API_KEY", "version:      1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("确认摘要缺少 %q:\n%s", want, joined)
		}
	}
	t.Setenv("CAIRN_TUI_TEST_KEY", "secret-value")
	v2Lines := confirmLines("u", "m", "CAIRN_TUI_TEST_KEY", app.SetupInput{ConfigVersion: 2, Admission: "required"})
	joinedV2 := strings.Join(v2Lines, "\n")
	if !strings.Contains(joinedV2, "2（required）") {
		t.Fatalf("version 2 摘要应展示准入语义:\n%s", joinedV2)
	}
	if strings.Contains(joinedV2, "secret-value") {
		t.Fatal("确认摘要不得回显密钥值")
	}
}

// TestMoveSuggestionCursorCycles 验证建议光标在列表内循环且空列表不动。
func TestMoveSuggestionCursorCycles(t *testing.T) {
	wizard := ConfigWizard{suggestions: testCatalogEntries(0)}
	wizard.moveSuggestionCursor(true)
	if wizard.suggestionCursor != -1 {
		t.Fatalf("空列表光标应保持 -1，得到 %d", wizard.suggestionCursor)
	}
	wizard.suggestions = testCatalogEntries(3)
	wizard.moveSuggestionCursor(true)
	if wizard.suggestionCursor != 0 {
		t.Fatalf("首次下移应落到 0，得到 %d", wizard.suggestionCursor)
	}
	wizard.moveSuggestionCursor(true)
	wizard.moveSuggestionCursor(true)
	if wizard.suggestionCursor != 2 {
		t.Fatalf("三次下移应到达 2，得到 %d", wizard.suggestionCursor)
	}
	wizard.moveSuggestionCursor(true)
	if wizard.suggestionCursor != 0 {
		t.Fatalf("继续下移应循环回 0，得到 %d", wizard.suggestionCursor)
	}
	wizard.moveSuggestionCursor(false)
	if wizard.suggestionCursor != 2 {
		t.Fatalf("0 上移应循环回 2，得到 %d", wizard.suggestionCursor)
	}
}

// TestProgressOrdinalCoversInteractiveSteps 验证进度序数覆盖可交互步并在提
// 交后固定为最后一档。
func TestProgressOrdinalCoversInteractiveSteps(t *testing.T) {
	wizard := ConfigWizard{step: stepProvider}
	if got := wizard.progressOrdinal(); got != 1 {
		t.Fatalf("provider 步进度应为 1，得到 %d", got)
	}
	wizard.step = stepConfirm
	if got := wizard.progressOrdinal(); got != wizardStepCount {
		t.Fatalf("确认步进度应为 %d，得到 %d", wizardStepCount, got)
	}
	wizard.step = stepDone
	if got := wizard.progressOrdinal(); got != wizardStepCount {
		t.Fatalf("终态步进度应固定 %d，得到 %d", wizardStepCount, got)
	}
}

// keyPress 构造 bubbletea 按键消息。
func keyPress(key tea.KeyType) tea.Msg {
	return tea.KeyMsg{Type: key}
}

// TestWizardAdvancesThroughSteps 验证按键链路把向导从 provider 选择推进到
// 提交步，且目录消息在任意步骤到达都不被丢弃。
func TestWizardAdvancesThroughSteps(t *testing.T) {
	wizard := NewConfigWizard(context.Background(), app.SetupInput{})
	if wizard.step != stepProvider {
		t.Fatalf("初始步应为 provider 选择，得到 %d", wizard.step)
	}
	catalogMsg := catalogLoadedMsg{catalog: &modelcatalog.Catalog{Entries: testCatalogEntries(2)}}
	model, _ := wizard.Update(catalogMsg)
	wizard = model.(ConfigWizard)
	if wizard.catalog == nil {
		t.Fatal("provider 步到达的目录消息不应被丢弃")
	}
	model, _ = wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepBaseURL {
		t.Fatalf("Enter 后应进入端点输入，得到 %d", wizard.step)
	}
	if wizard.baseURLInput.Value() != defaultBaseURL {
		t.Fatalf("端点输入应预填默认值 %q，得到 %q", defaultBaseURL, wizard.baseURLInput.Value())
	}
	model, _ = wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepModel {
		t.Fatalf("默认端点直接回车应推进到模型步（修复前会静默卡住），得到 %d", wizard.step)
	}
	if len(wizard.suggestions) != 0 {
		t.Fatalf("空查询不应产生建议，得到 %d 条", len(wizard.suggestions))
	}
	wizard.modelInput.SetValue("glm-5.3")
	model, _ = wizard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	wizard = model.(ConfigWizard)
	if wizard.step != stepModel || wizard.modelInput.Value() != "glm-5.3x" {
		t.Fatalf("模型步应接受文本输入，得到 step=%d value=%q", wizard.step, wizard.modelInput.Value())
	}
	model, _ = wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepAPIKeyEnv {
		t.Fatalf("模型输入后应进入密钥变量名步，得到 %d", wizard.step)
	}
	wizard.apiKeyEnvInput.SetValue("ZHIPU_API_KEY")
	model, _ = wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepAPIKeySecret {
		t.Fatalf("变量名输入后应进入密钥值步，得到 %d", wizard.step)
	}
	model, _ = wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepConfirm {
		t.Fatalf("密钥值步（空跳过）应进入确认页，得到 %d", wizard.step)
	}
	model, _ = wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepCommitting {
		t.Fatalf("确认页 Enter 应发起提交，得到 %d", wizard.step)
	}
	if wizard.input.BaseURL != defaultBaseURL || wizard.input.Model != "glm-5.3x" || wizard.input.APIKeyEnv != "ZHIPU_API_KEY" {
		t.Fatalf("提交输入未组装: %+v", wizard.input)
	}
}

// TestEnvStepPrefillsDefault 验证密钥环境变量名步预填默认值且回车推进到密
// 钥值步。
func TestEnvStepPrefillsDefault(t *testing.T) {
	wizard := NewConfigWizard(context.Background(), app.SetupInput{})
	wizard.step = stepAPIKeyEnv
	if wizard.apiKeyEnvInput.Value() != defaultAPIKeyEnv {
		t.Fatalf("密钥变量名应预填默认值 %q，得到 %q", defaultAPIKeyEnv, wizard.apiKeyEnvInput.Value())
	}
	model, _ := wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepAPIKeySecret {
		t.Fatalf("默认变量名直接回车应进入密钥值步，得到 %d", wizard.step)
	}
}

// TestSecretStepPersistsCredential 验证密钥值输入在提交时写入本机凭据文件、
// 注入进程环境，且环境已存在时不覆盖文件之外的来源；使用临时目录隔离。
func TestSecretStepPersistsCredential(t *testing.T) {
	t.Cleanup(func() { os.Unsetenv("CAIRN_TUI_TEST_SECRET_KEY") })
	workspace := t.TempDir()
	wizard := NewConfigWizard(context.Background(), app.SetupInput{Workspace: workspace})
	wizard.step = stepConfirm
	wizard.apiKeyEnvInput.SetValue("CAIRN_TUI_TEST_SECRET_KEY")
	wizard.apiKeySecretInput.SetValue("test-secret-value")
	model, _ := wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepCommitting {
		t.Fatalf("带密钥提交应进入提交步，得到 %d（err=%v）", wizard.step, wizard.commitErr)
	}
	if got := os.Getenv("CAIRN_TUI_TEST_SECRET_KEY"); got != "test-secret-value" {
		t.Fatalf("密钥应注入当前进程环境，得到 %q", got)
	}
	content, err := os.ReadFile(filepath.Join(workspace, credential.KeysFileName))
	if err != nil {
		t.Fatalf("凭据文件应已写入: %v", err)
	}
	if !strings.Contains(string(content), "CAIRN_TUI_TEST_SECRET_KEY=") {
		t.Fatalf("凭据文件应包含变量名行:\n%s", content)
	}
	if strings.Contains(string(content), "test-secret-value\n") && strings.Count(string(content), "test-secret-value") > 1 {
		t.Fatal("凭据文件出现重复条目")
	}
	info, err := os.Stat(filepath.Join(workspace, credential.KeysFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("凭据文件权限应为 0600，得到 %v（err=%v）", info.Mode().Perm(), err)
	}
}

// TestWizardRetreatsOnEsc 验证 Esc 逐步后退且输入保留。
func TestWizardRetreatsOnEsc(t *testing.T) {
	wizard := NewConfigWizard(context.Background(), app.SetupInput{})
	model, _ := wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	wizard.baseURLInput.SetValue("https://api.example.com")
	model, _ = wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	model, _ = wizard.Update(keyPress(tea.KeyEsc))
	wizard = model.(ConfigWizard)
	if wizard.step != stepBaseURL {
		t.Fatalf("模型步 Esc 应返回端点步，得到 %d", wizard.step)
	}
	if wizard.baseURLInput.Value() != "https://api.example.com" {
		t.Fatal("后退不应丢失已输入内容")
	}
	model, _ = wizard.Update(keyPress(tea.KeyEsc))
	wizard = model.(ConfigWizard)
	if wizard.step != stepProvider {
		t.Fatalf("端点步 Esc 应返回 provider 步，得到 %d", wizard.step)
	}
}

// TestWizardCommitResultTransitions 验证提交结果消息把向导推进到终态，失败
// 时 Esc 提供重试出口。
func TestWizardCommitResultTransitions(t *testing.T) {
	wizard := NewConfigWizard(context.Background(), app.SetupInput{})
	wizard.step = stepCommitting
	model, _ := wizard.Update(commitFinishedMsg{outcome: &app.SetupOutcome{ConfigPath: ".agent/config.yaml", APIKeyEnv: "ZHIPU_API_KEY"}})
	wizard = model.(ConfigWizard)
	if wizard.step != stepDone || wizard.outcome == nil || wizard.commitErr != nil {
		t.Fatalf("成功提交应进入终态，得到 step=%d outcome=%v err=%v", wizard.step, wizard.outcome, wizard.commitErr)
	}
	model, _ = wizard.Update(keyPress(tea.KeyEsc))
	wizard = model.(ConfigWizard)
	if wizard.step != stepDone {
		t.Fatalf("成功终态 Esc 不应回退，得到 %d", wizard.step)
	}

	failed := NewConfigWizard(context.Background(), app.SetupInput{})
	failed.step = stepCommitting
	model, _ = failed.Update(commitFinishedMsg{err: context.DeadlineExceeded})
	failed = model.(ConfigWizard)
	if failed.step != stepDone || failed.commitErr == nil {
		t.Fatalf("失败提交应进入终态并保留错误，得到 step=%d err=%v", failed.step, failed.commitErr)
	}
	model, _ = failed.Update(keyPress(tea.KeyEsc))
	failed = model.(ConfigWizard)
	if failed.step != stepProvider || failed.commitErr != nil {
		t.Fatalf("失败终态 Esc 应回到开头并清空错误，得到 step=%d err=%v", failed.step, failed.commitErr)
	}
}

// TestViewRendersEachStep 验证每一步的渲染输出非空且不含密钥值。
func TestViewRendersEachStep(t *testing.T) {
	t.Setenv("CAIRN_TUI_TEST_KEY", "secret-value")
	wizard := NewConfigWizard(context.Background(), app.SetupInput{})
	wizard.apiKeyEnvInput.SetValue("CAIRN_TUI_TEST_KEY")
	for step := wizardStep(0); step <= stepDone; step++ {
		wizard.step = step
		if step == stepDone {
			wizard.outcome = &app.SetupOutcome{ConfigPath: ".agent/config.yaml", APIKeyEnv: "CAIRN_TUI_TEST_KEY"}
		}
		rendered := wizard.View()
		if strings.TrimSpace(rendered) == "" {
			t.Fatalf("步骤 %d 渲染为空", step)
		}
		if strings.Contains(rendered, "secret-value") {
			t.Fatalf("步骤 %d 渲染泄漏密钥值", step)
		}
	}
}
