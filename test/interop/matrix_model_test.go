package interop

import (
	"strings"
	"testing"
)

// TestMatrixLookupAndRender 锁定矩阵的归拢与渲染：按行取格、按 CLI 渲染，
// 未探测的行必须显式出现而不是被省略。
func TestMatrixLookupAndRender(t *testing.T) {
	matrix := Matrix{}
	matrix.Add(Observation{
		Capability: CapabilityStart, CLI: "cairn-native",
		Status: StatusMeasured, Summary: "非交互启动成功，退出码 0",
	})
	matrix.Add(Observation{
		Scenario: EdgeSessionLost, CLI: "codex",
		Status: StatusUnavailable, Reason: "外部 CLI 未安装，本机不可得",
	})

	observation, ok := matrix.Lookup("cairn-native", string(CapabilityStart))
	if !ok {
		t.Fatal("应当取到 start 行观测")
	}
	if observation.Summary != "非交互启动成功，退出码 0" {
		t.Fatalf("取到的观测不符: %#v", observation)
	}
	if _, ok := matrix.Lookup("codex", string(CapabilityStart)); ok {
		t.Fatal("不存在的格不应被取到")
	}

	rendered := matrix.Render([]string{"cairn-native", "codex", "claude"})
	for _, capability := range AllCapabilities() {
		if !strings.Contains(rendered, string(capability)) {
			t.Fatalf("渲染结果缺少能力行 %s", capability)
		}
	}
	for _, scenario := range AllEdgeScenarios() {
		if !strings.Contains(rendered, string(scenario)) {
			t.Fatalf("渲染结果缺少边界场景行 %s", scenario)
		}
	}
	if !strings.Contains(rendered, "外部 CLI 未安装，本机不可得") {
		t.Fatalf("渲染结果缺少不可得原因: %s", rendered)
	}
	if !strings.Contains(rendered, "claude") || !strings.Contains(rendered, "未探测") {
		t.Fatalf("未探测的 CLI 必须显式出现: %s", rendered)
	}
}

// TestObservationStatusAnnotationRules 锁定证据等级规则：不可得的格必须
// 写明原因，已实测的格必须给出结论。这条规则保证矩阵里不会出现既没有
// 结论也没有原因的空格。
func TestObservationStatusAnnotationRules(t *testing.T) {
	measured := Observation{Capability: CapabilityUsage, CLI: "codex", Status: StatusMeasured, Summary: "报告 token 用量"}
	if !strings.Contains(measured.Describe(), "报告 token 用量") {
		t.Fatalf("实测格必须给出结论: %s", measured.Describe())
	}
	unavailable := Observation{Capability: CapabilityUsage, CLI: "codex", Status: StatusUnavailable, Reason: "外部 CLI 未安装"}
	described := unavailable.Describe()
	if !strings.Contains(described, string(StatusUnavailable)) || !strings.Contains(described, "外部 CLI 未安装") {
		t.Fatalf("不可得格必须写明原因: %s", described)
	}
	if unavailable.Row() != string(CapabilityUsage) {
		t.Fatalf("行标识 = %q", unavailable.Row())
	}
	edge := Observation{Scenario: EdgeFileModificationEvent, CLI: "claude", Status: StatusMeasured}
	if edge.Row() != string(EdgeFileModificationEvent) {
		t.Fatalf("边界场景行的标识 = %q", edge.Row())
	}
}

// TestAllCapabilitiesCoverAcceptanceRows 锁定能力清单与边界场景清单的覆盖
// 口径：验收要求的八项能力与四个边界场景必须全部在列，且不多不少。
func TestAllCapabilitiesCoverAcceptanceRows(t *testing.T) {
	capabilities := AllCapabilities()
	if len(capabilities) != 8 {
		t.Fatalf("能力数 = %d，期望 8", len(capabilities))
	}
	expected := map[Capability]bool{
		CapabilityStart: true, CapabilityStream: true, CapabilityCancel: true,
		CapabilityResume: true, CapabilityApproval: true, CapabilityArtifact: true,
		CapabilityUsage: true, CapabilityError: true,
	}
	for _, capability := range capabilities {
		if !expected[capability] {
			t.Fatalf("出现未知能力 %s", capability)
		}
		delete(expected, capability)
	}
	if len(expected) != 0 {
		t.Fatalf("缺少能力: %v", expected)
	}

	scenarios := AllEdgeScenarios()
	if len(scenarios) != 4 {
		t.Fatalf("边界场景数 = %d，期望 4", len(scenarios))
	}
	expectedScenarios := map[EdgeScenario]bool{
		EdgeSessionLost: true, EdgeProcessInterrupted: true,
		EdgeRuntimePermissionPrompt: true, EdgeFileModificationEvent: true,
	}
	for _, scenario := range scenarios {
		if !expectedScenarios[scenario] {
			t.Fatalf("出现未知边界场景 %s", scenario)
		}
		delete(expectedScenarios, scenario)
	}
	if len(expectedScenarios) != 0 {
		t.Fatalf("缺少边界场景: %v", expectedScenarios)
	}
}

// TestTaskSetSatisfiesAcceptanceCount 锁定任务集的数量口径：验收要求 5 到
// 10 个任务，任务集必须落在区间内，且任务标识互不重复。
func TestTaskSetSatisfiesAcceptanceCount(t *testing.T) {
	tasks := TaskSet()
	if len(tasks) < 5 || len(tasks) > 10 {
		t.Fatalf("任务数 = %d，期望落在 5 到 10 之间", len(tasks))
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		if task.ID == "" {
			t.Fatal("任务标识不能为空")
		}
		if seen[task.ID] {
			t.Fatalf("任务标识重复: %s", task.ID)
		}
		seen[task.ID] = true
		if len(task.Script.Steps) == 0 {
			t.Fatalf("任务 %s 没有脚本步骤", task.ID)
		}
		if !strings.Contains(task.Prompt(), scriptMarkerOpen) {
			t.Fatalf("任务 %s 的提示词没有携带脚本标记", task.ID)
		}
	}
}
