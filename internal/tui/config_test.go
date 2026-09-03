// config_test 覆盖模型配置向导的纯函数与按键状态机：目录建议过滤、密钥环
// 境变量存在性、确认摘要、建议光标移动、步骤进度与全流程推进。提交与网络
// 探测属 SetupService 既有测试范围，此处不重复。
//
// Author: fxbin
package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fxbin/cairn/internal/app"
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
	if got := filterSuggestions(nil, "model"); got != nil {
		t.Fatalf("nil 目录应返回 nil 建议，得到 %d 条", len(got))
	}
	if got := filterSuggestions(catalog, "   "); got != nil {
		t.Fatalf("空查询应返回 nil 建议，得到 %d 条", len(got))
	}
}

// TestFilterSuggestionsCapsLimit 验证建议条数被上限截断。
func TestFilterSuggestionsCapsLimit(t *testing.T) {
	catalog := &modelcatalog.Catalog{Entries: testCatalogEntries(maxModelSuggestions + 5)}
	got := filterSuggestions(catalog, "model")
	if len(got) != maxModelSuggestions {
		t.Fatalf("建议应截断为 %d 条，得到 %d 条", maxModelSuggestions, len(got))
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
	if wizard.step != stepConfirm {
		t.Fatalf("变量名输入后应进入确认页，得到 %d", wizard.step)
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

// TestEnvStepPrefillsDefault 验证密钥环境变量名步预填默认值且回车直接推进。
func TestEnvStepPrefillsDefault(t *testing.T) {
	wizard := NewConfigWizard(context.Background(), app.SetupInput{})
	wizard.step = stepAPIKeyEnv
	if wizard.apiKeyEnvInput.Value() != defaultAPIKeyEnv {
		t.Fatalf("密钥变量名应预填默认值 %q，得到 %q", defaultAPIKeyEnv, wizard.apiKeyEnvInput.Value())
	}
	model, _ := wizard.Update(keyPress(tea.KeyEnter))
	wizard = model.(ConfigWizard)
	if wizard.step != stepConfirm {
		t.Fatalf("默认变量名直接回车应进入确认页，得到 %d", wizard.step)
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
