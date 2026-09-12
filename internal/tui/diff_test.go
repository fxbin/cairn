// diff_test 覆盖补丁差异视图的纯函数：行级编辑投影、统一差异分块、参数级
// 降级渲染与替换语义重建。
package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/tools"
)

// joinDiff 把差异行去掉样式后拼接，便于断言内容。
func joinDiff(lines []string) string {
	stripped := make([]string, 0, len(lines))
	for _, line := range lines {
		stripped = append(stripped, stripANSI(line))
	}
	return strings.Join(stripped, "\n")
}

// stripANSI 去除 lipgloss 样式转义序列，只保留文本内容。
func stripANSI(text string) string {
	var builder strings.Builder
	inEscape := false
	for _, symbol := range text {
		switch {
		case symbol == '\x1b':
			inEscape = true
		case inEscape && symbol == 'm':
			inEscape = false
		case !inEscape:
			builder.WriteRune(symbol)
		}
	}
	return builder.String()
}

// TestSplitLinesTrailingNewline 锁定行切分：结尾换行不产生幻影空行。
func TestSplitLinesTrailingNewline(t *testing.T) {
	if got := splitLines("a\nb\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitLines=%q", got)
	}
	if got := splitLines(""); len(got) != 0 {
		t.Fatalf("empty split=%q", got)
	}
	if got := splitLines("single"); len(got) != 1 || got[0] != "single" {
		t.Fatalf("single split=%q", got)
	}
}

// TestRenderUnifiedDiffReplaceWithContext 锁定统一差异结构：@@ 头、上下文行、
// 删除行与新增行成对出现。
func TestRenderUnifiedDiffReplaceWithContext(t *testing.T) {
	before := []string{"l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8", "l9", "l10"}
	after := []string{"l1", "l2", "l3", "l4", "l5", "l6", "changed", "l8", "l9", "l10"}
	rendered := joinDiff(renderUnifiedDiff(before, after))
	if !strings.Contains(rendered, "@@ -4,7 +4,7 @@") {
		t.Fatalf("hunk header missing or wrong: %s", rendered)
	}
	if !strings.Contains(rendered, "- l7") || !strings.Contains(rendered, "+ changed") {
		t.Fatalf("changed lines missing: %s", rendered)
	}
	if !strings.Contains(rendered, "  l4") || !strings.Contains(rendered, "  l10") {
		t.Fatalf("context lines missing: %s", rendered)
	}
}

// TestRenderUnifiedDiffNoChange 锁定无差异的显式反馈。
func TestRenderUnifiedDiffNoChange(t *testing.T) {
	rendered := joinDiff(renderUnifiedDiff([]string{"a", "b"}, []string{"a", "b"}))
	if !strings.Contains(rendered, "（内容无差异）") {
		t.Fatalf("expected no-change marker: %s", rendered)
	}
}

// TestRenderUnifiedDiffFarApartChangesSplitHunks 锁定相距较远的变化分成多个块。
func TestRenderUnifiedDiffFarApartChangesSplitHunks(t *testing.T) {
	before := make([]string, 40)
	after := make([]string, 40)
	for index := range before {
		before[index] = "line"
		after[index] = "line"
	}
	before[2], after[2] = "old-a", "new-a"
	before[35], after[35] = "old-b", "new-b"
	rendered := joinDiff(renderUnifiedDiff(before, after))
	if count := strings.Count(rendered, "@@ -"); count != 2 {
		t.Fatalf("expected two hunks, got %d: %s", count, rendered)
	}
}

// TestLineDiffPrefersContiguousEdits 锁定相邻删增保持删除在前新增在后的顺
// 序（统一差异惯例）。
func TestLineDiffPrefersContiguousEdits(t *testing.T) {
	ops := lineDiff([]string{"a", "b", "c"}, []string{"a", "x", "c"})
	kinds := make([]diffOpKind, 0, len(ops))
	for _, op := range ops {
		kinds = append(kinds, op.kind)
	}
	want := []diffOpKind{diffEqual, diffRemove, diffAdd, diffEqual}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v", kinds)
	}
	for index := range want {
		if kinds[index] != want[index] {
			t.Fatalf("kinds=%v want %v", kinds, want)
		}
	}
}

// TestApplyReplacementsTextMirrorsToolOrder 锁定展示推导与工具层替换语义一
// 致：按声明顺序全量替换（前一步结果可被后一步继续替换）。
func TestApplyReplacementsTextMirrorsToolOrder(t *testing.T) {
	replacements := []tools.Replacement{
		{Old: "a", New: "b"},
		{Old: "bb", New: "c"},
	}
	if got := applyReplacementsText("aa\n", replacements); got != "c\n" {
		t.Fatalf("sequential replace=%q", got)
	}
}

// patchArguments 构造补丁参数 JSON。
func patchArguments(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestRenderPatchDiffCreate 锁定 create 渲染为全文新增。
func TestRenderPatchDiffCreate(t *testing.T) {
	arguments := patchArguments(t, map[string]any{"path": "fresh.txt", "mode": "create", "content": "hello\nworld\n"})
	rendered := joinDiff(renderPatchDiff(arguments, nil, false))
	if !strings.Contains(rendered, "补丁 create · fresh.txt") {
		t.Fatalf("header missing: %s", rendered)
	}
	if !strings.Contains(rendered, "+ hello") || !strings.Contains(rendered, "+ world") {
		t.Fatalf("added lines missing: %s", rendered)
	}
	if strings.Contains(rendered, "- hello") {
		t.Fatalf("create must not render removals: %s", rendered)
	}
}

// TestRenderPatchDiffDeleteWithBefore 锁定 delete 渲染为 before 全文删除；
// 无 before 时如实标注。
func TestRenderPatchDiffDeleteWithBefore(t *testing.T) {
	arguments := patchArguments(t, map[string]any{"path": "gone.txt", "mode": "delete"})
	rendered := joinDiff(renderPatchDiff(arguments, []byte("gone body\n"), true))
	if !strings.Contains(rendered, "- gone body") {
		t.Fatalf("removed lines missing: %s", rendered)
	}
	degraded := joinDiff(renderPatchDiff(arguments, nil, false))
	if !strings.Contains(degraded, "before 快照不可用") {
		t.Fatalf("degradation note missing: %s", degraded)
	}
}

// TestRenderPatchDiffUpdateRebuildsAfter 锁定 update 以制品内容重建 after 后
// 行比较；无制品时降级为无定位替换块。
func TestRenderPatchDiffUpdateRebuildsAfter(t *testing.T) {
	arguments := patchArguments(t, map[string]any{
		"path": "note.txt", "mode": "update",
		"replacements": []map[string]string{{"old": "original", "new": "restored"}},
	})
	before := "header\noriginal body\nfooter\n"
	rendered := joinDiff(renderPatchDiff(arguments, []byte(before), true))
	if !strings.Contains(rendered, "- original body") || !strings.Contains(rendered, "+ restored body") {
		t.Fatalf("replacement diff missing: %s", rendered)
	}
	if !strings.Contains(rendered, "  header") || !strings.Contains(rendered, "  footer") {
		t.Fatalf("context missing: %s", rendered)
	}
	fallback := joinDiff(renderPatchDiff(arguments, nil, false))
	if !strings.Contains(fallback, "仅显示替换块") || !strings.Contains(fallback, "- original") || !strings.Contains(fallback, "+ restored") {
		t.Fatalf("fallback blocks missing: %s", fallback)
	}
}

// TestRenderPatchDiffUndecodableArguments 锁定参数不可解码的失败关闭提示。
func TestRenderPatchDiffUndecodableArguments(t *testing.T) {
	rendered := joinDiff(renderPatchDiff(json.RawMessage("{not json"), nil, false))
	if !strings.Contains(rendered, "不可解码") {
		t.Fatalf("expected decode failure note: %s", rendered)
	}
}

// TestRenderUnifiedDiffTruncationCap 锁定超限截断并标注总量。
func TestRenderUnifiedDiffTruncationCap(t *testing.T) {
	before := make([]string, diffMaxLines+50)
	after := make([]string, diffMaxLines+50)
	for index := range before {
		before[index] = "old"
		after[index] = "new"
	}
	rendered := renderUnifiedDiff(before, after)
	if len(rendered) > diffMaxLines+1 {
		t.Fatalf("rendered lines=%d exceed cap+%d", len(rendered), 1)
	}
	if !strings.Contains(joinDiff(rendered), "已截断") {
		t.Fatal("truncation note missing")
	}
}

// TestApplyReplacementsTextSkipsEmptyOld 锁定畸形参数防御：空 old 与工具层
// 拒绝语义对齐，展示层跳过而不渲染逐字符注入假差异。
func TestApplyReplacementsTextSkipsEmptyOld(t *testing.T) {
	replacements := []tools.Replacement{{Old: "", New: "x"}}
	if got := applyReplacementsText("ab\n", replacements); got != "ab\n" {
		t.Fatalf("empty old must be skipped: %q", got)
	}
}

// TestRenderUnifiedDiffEmptyRangeHeaderConvention 锁定空范围侧行号取前一位置
// 的统一差异惯例（@@ -0,0 +1,N @@）。
func TestRenderUnifiedDiffEmptyRangeHeaderConvention(t *testing.T) {
	rendered := joinDiff(renderUnifiedDiff(nil, []string{"first", "second"}))
	if !strings.Contains(rendered, "@@ -0,0 +1,2 @@") {
		t.Fatalf("empty-range header convention: %s", rendered)
	}
	rendered = joinDiff(renderUnifiedDiff([]string{"only"}, nil))
	if !strings.Contains(rendered, "@@ -1,1 +0,0 @@") {
		t.Fatalf("empty-after header convention: %s", rendered)
	}
}

// TestRenderPlannedPatchDiffUsesPlanAfter 锁定审批路径用计划器权威前后全文：
// 展示层不重建 after。
func TestRenderPlannedPatchDiffUsesPlanAfter(t *testing.T) {
	arguments := patchArguments(t, map[string]any{
		"path": "note.txt", "mode": "update",
		"replacements": []map[string]string{{"old": "original", "new": "restored"}},
	})
	plan := tools.PatchPlan{Path: "note.txt", Mode: "update", Before: []byte("header\noriginal body\nfooter\n"), After: []byte("header\nrestored body\nfooter\n"), Exists: true}
	rendered := joinDiff(renderPlannedPatchDiff(plan, arguments))
	if !strings.Contains(rendered, "- original body") || !strings.Contains(rendered, "+ restored body") {
		t.Fatalf("planned diff missing: %s", rendered)
	}
	if !strings.Contains(rendered, "  header") || !strings.Contains(rendered, "  footer") {
		t.Fatalf("planned diff context missing: %s", rendered)
	}
}
