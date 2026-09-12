// diff.go 实现补丁差异视图：把 apply_patch 的已持久化事实投影为统一差异行。
// 审批时的差异由 ApplyPatch.Plan 零写入现算（快照发生在批准之后）；回看已
// 执行补丁时 before 来自 planned_before_ref 制品、after 由参数确定性重建。
// 两条路共用同一渲染器；before 不可用时如实降级，不猜测文件内容。
//
// Author: fxbin
package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
)

// diffContextLines 是统一差异块两侧的上下文行数。
const diffContextLines = 3

// diffMaxLines 是差异视图的总行数上限；超出截断并标注，完整内容以制品与
// 事件为准。
const diffMaxLines = 800

// diffMaxCompareLines 是精确行比较的行数上限；超出退化为整块替换渲染，
// 如实可读而不假装精确。
const diffMaxCompareLines = 2000

// diffWindowHeight 是差异视图同屏行数。
const diffWindowHeight = 20

// diffOpKind 标记一行在编辑投影中的角色。
type diffOpKind int

const (
	diffEqual diffOpKind = iota
	diffRemove
	diffAdd
)

// diffOp 是行级编辑投影的一个步骤。
type diffOp struct {
	kind diffOpKind
	text string
}

// renderPatchDiff 把一次 apply_patch 提议投影为差异视图行：create 渲染新增
// 全文，delete 渲染 before 内容全文删除，update 以 before 内容重建 after 后
// 行比较；before 不可用时 update 降级为无定位替换块、delete 如实标注。
func renderPatchDiff(arguments json.RawMessage, before []byte, beforeKnown bool) []string {
	var patch tools.PatchArguments
	if len(arguments) == 0 || json.Unmarshal(arguments, &patch) != nil {
		return []string{warnStyle.Render("（补丁参数不可解码，无法生成差异）")}
	}
	header := patchHeader(&patch)
	switch patch.Mode {
	case "create":
		if patch.Content == nil {
			return []string{header, warnStyle.Render("（create 参数缺少内容）")}
		}
		return append([]string{header}, renderUnifiedDiff(nil, splitLines(*patch.Content))...)
	case "delete":
		if !beforeKnown {
			return []string{header, warnStyle.Render("（before 快照不可用，无法显示被删内容）")}
		}
		return append([]string{header}, renderUnifiedDiff(splitLines(string(before)), nil)...)
	case "update":
		if !beforeKnown {
			fallback := []string{header, warnStyle.Render("（before 快照不可用，仅显示替换块，无文件定位）")}
			for index, replacement := range patch.Replacements {
				fallback = append(fallback, dimStyle.Render(fmt.Sprintf("替换 %d：", index+1)))
				for _, line := range splitLines(replacement.Old) {
					fallback = append(fallback, errStyle.Render("- "+line))
				}
				for _, line := range splitLines(replacement.New) {
					fallback = append(fallback, okStyle.Render("+ "+line))
				}
			}
			return fallback
		}
		after := applyReplacementsText(string(before), patch.Replacements)
		return append([]string{header}, renderUnifiedDiff(splitLines(string(before)), splitLines(after))...)
	default:
		return []string{header, warnStyle.Render("（未知补丁模式 " + patch.Mode + "）")}
	}
}

// renderPlannedPatchDiff 以补丁计划器的前后全文渲染差异：审批路径使用，after
// 来自工具层权威计算，不经展示层重建。
func renderPlannedPatchDiff(plan tools.PatchPlan, arguments json.RawMessage) []string {
	var patch tools.PatchArguments
	if len(arguments) != 0 && json.Unmarshal(arguments, &patch) == nil {
		header := patchHeader(&patch)
		switch patch.Mode {
		case "create":
			return append([]string{header}, renderUnifiedDiff(nil, splitLines(string(plan.After)))...)
		case "delete":
			return append([]string{header}, renderUnifiedDiff(splitLines(string(plan.Before)), nil)...)
		case "update":
			return append([]string{header}, renderUnifiedDiff(splitLines(string(plan.Before)), splitLines(string(plan.After)))...)
		}
	}
	return renderPatchDiff(arguments, plan.Before, plan.Exists)
}

// patchHeader 渲染差异视图的补丁身份头。
func patchHeader(patch *tools.PatchArguments) string {
	return itemStyle.Render(fmt.Sprintf("补丁 %s · %s", patch.Mode, patch.Path))
}

// renderUnifiedDiff 把行级编辑投影分组为带上下文的统一差异行（@@ 头 + 上
// 下文/删除/新增行），总行数超限截断并标注。
func renderUnifiedDiff(before, after []string) []string {
	ops := lineDiff(before, after)
	changed := make([]bool, len(ops))
	hasChange := false
	for index, op := range ops {
		changed[index] = op.kind != diffEqual
		hasChange = hasChange || changed[index]
	}
	if !hasChange {
		return []string{dimStyle.Render("（内容无差异）")}
	}
	// 每段连续变更按上下文扩展为块区间，间隔小于两倍上下文的相邻块合并。
	type hunkRange struct{ start, end int }
	var hunks []hunkRange
	for index := 0; index < len(ops); {
		if !changed[index] {
			index++
			continue
		}
		end := index + 1
		for end < len(ops) && changed[end] {
			end++
		}
		startContext := index - diffContextLines
		if startContext < 0 {
			startContext = 0
		}
		endContext := end + diffContextLines
		if endContext > len(ops) {
			endContext = len(ops)
		}
		if last := len(hunks) - 1; last >= 0 && startContext <= hunks[last].end {
			if endContext > hunks[last].end {
				hunks[last].end = endContext
			}
		} else {
			hunks = append(hunks, hunkRange{startContext, endContext})
		}
		index = end
	}
	var lines []string
	truncated := false
	emit := func(line string) bool {
		if len(lines) >= diffMaxLines {
			truncated = true
			return false
		}
		lines = append(lines, line)
		return true
	}
	beforeLine, afterLine, position := 1, 1, 0
	advance := func(op diffOp) {
		if op.kind != diffAdd {
			beforeLine++
		}
		if op.kind != diffRemove {
			afterLine++
		}
	}
	for _, hunk := range hunks {
		for position < hunk.start {
			advance(ops[position])
			position++
		}
		countBefore, countAfter := 0, 0
		for _, op := range ops[hunk.start:hunk.end] {
			if op.kind != diffAdd {
				countBefore++
			}
			if op.kind != diffRemove {
				countAfter++
			}
		}
		// 空范围侧的起始行号按统一差异惯例取前一位置（如 @@ -0,0 +1,N @@）。
		headerBefore, headerAfter := beforeLine, afterLine
		if countBefore == 0 && headerBefore > 0 {
			headerBefore--
		}
		if countAfter == 0 && headerAfter > 0 {
			headerAfter--
		}
		if !emit(dimStyle.Render(fmt.Sprintf("@@ -%d,%d +%d,%d @@", headerBefore, countBefore, headerAfter, countAfter))) {
			break
		}
		aborted := false
		for position < hunk.end {
			op := ops[position]
			var line string
			switch op.kind {
			case diffEqual:
				line = "  " + op.text
			case diffRemove:
				line = errStyle.Render("- " + op.text)
			case diffAdd:
				line = okStyle.Render("+ " + op.text)
			}
			if !emit(line) {
				aborted = true
				break
			}
			advance(op)
			position++
		}
		if aborted {
			break
		}
	}
	if truncated {
		lines = append(lines, warnStyle.Render(fmt.Sprintf("…（差异超过 %d 行已截断，完整内容以制品与事件为准）", diffMaxLines)))
	}
	return lines
}

// lineDiff 计算两个行序列的行级编辑投影：先裁剪公共前后缀，剩余部分用最长
// 公共子序列动态规划；任一侧超过精确比较上限时退化为整块替换。
func lineDiff(before, after []string) []diffOp {
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix && before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	ops := make([]diffOp, 0, len(before)+len(after))
	for index := 0; index < prefix; index++ {
		ops = append(ops, diffOp{diffEqual, before[index]})
	}
	core := before[prefix : len(before)-suffix]
	rest := after[prefix : len(after)-suffix]
	if len(core) > diffMaxCompareLines || len(rest) > diffMaxCompareLines {
		for _, line := range core {
			ops = append(ops, diffOp{diffRemove, line})
		}
		for _, line := range rest {
			ops = append(ops, diffOp{diffAdd, line})
		}
	} else {
		ops = append(ops, lcsOps(core, rest)...)
	}
	for index := 0; index < suffix; index++ {
		ops = append(ops, diffOp{diffEqual, before[len(before)-suffix+index]})
	}
	return ops
}

// lcsOps 以最长公共子序列动态规划生成保持/删除/新增投影；两个输入均已裁剪
// 公共前后缀且行数受上限约束。
func lcsOps(before, after []string) []diffOp {
	rows, columns := len(before), len(after)
	table := make([][]int32, rows+1)
	for row := range table {
		table[row] = make([]int32, columns+1)
	}
	for row := rows - 1; row >= 0; row-- {
		for column := columns - 1; column >= 0; column-- {
			if before[row] == after[column] {
				table[row][column] = table[row+1][column+1] + 1
			} else if table[row+1][column] >= table[row][column+1] {
				table[row][column] = table[row+1][column]
			} else {
				table[row][column] = table[row][column+1]
			}
		}
	}
	ops := make([]diffOp, 0, rows+columns)
	row, column := 0, 0
	for row < rows && column < columns {
		switch {
		case before[row] == after[column]:
			ops = append(ops, diffOp{diffEqual, before[row]})
			row++
			column++
		case table[row+1][column] >= table[row][column+1]:
			ops = append(ops, diffOp{diffRemove, before[row]})
			row++
		default:
			ops = append(ops, diffOp{diffAdd, after[column]})
			column++
		}
	}
	for ; row < rows; row++ {
		ops = append(ops, diffOp{diffRemove, before[row]})
	}
	for ; column < columns; column++ {
		ops = append(ops, diffOp{diffAdd, after[column]})
	}
	return ops
}

// applyReplacementsText 以与工具层一致的顺序替换语义重建改写后文本；仅用于
// 展示推导，出现次数校验仍属工具层职责。空 old 文本会被工具层拒绝，此处同
// 样跳过以防畸形参数把逐字符注入渲染成假差异。
func applyReplacementsText(content string, replacements []tools.Replacement) string {
	for _, replacement := range replacements {
		if replacement.Old == "" {
			continue
		}
		content = strings.ReplaceAll(content, replacement.Old, replacement.New)
	}
	return content
}

// splitLines 按行切分文本；结尾换行不产生幻影空行，空文本返回空切片。
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	return lines
}
