package interop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExperimentOutcome 是一次"任务 × 外壳"对照实验的观测结果。
type ExperimentOutcome struct {
	// TaskID 是任务标识。
	TaskID string
	// Harness 是外壳标识。
	Harness string
	// Success 表示任务是否走完脚本并给出结论。
	Success bool
	// ExitCode 是外壳退出码。
	ExitCode int
	// ChangedFiles 是快照之间发生变化的文件路径（相对快照根）。
	ChangedFiles []string
	// EscapedWorkspace 标记副作用是否越出工作区。
	EscapedWorkspace bool
	// Usage 是外壳报告的 token 用量。
	Usage Usage
	// Duration 是墙钟耗时。
	Duration time.Duration
	// ApprovalRoundTrips 是为完成该任务发生的审批往返次数。
	ApprovalRoundTrips int
	// SessionHandle 是外壳给出的会话或线程句柄。
	SessionHandle string
	// Interrupted 标记运行是否被外部信号中断。
	Interrupted bool
	// Notes 是补充说明（终态原因、错误摘要等）。
	Notes []string
}

// ExperimentOptions 是一次对照实验的运行参数。
type ExperimentOptions struct {
	// Root 是实验的临时根目录；每个任务与外壳各自开子目录。
	Root string
	// Timeout 是单次运行的整体时限。
	Timeout time.Duration
	// InterruptAfter 非零时在该时长后中断运行，用于取消语义观测。
	InterruptAfter time.Duration
}

// RunExperiment 在同一组任务上依次驱动多个外壳，产出可对照的观测结果。
// 每个任务与外壳组合使用独立的工作区，互不污染。
func RunExperiment(ctx context.Context, harnesses []Harness, tasks []Task, options ExperimentOptions) ([]ExperimentOutcome, error) {
	var outcomes []ExperimentOutcome
	for _, task := range tasks {
		for _, harness := range harnesses {
			outcome, err := runOne(ctx, harness, task, options)
			if err != nil {
				return outcomes, fmt.Errorf("task %s harness %s: %w", task.ID, harness.Name(), err)
			}
			outcomes = append(outcomes, outcome)
		}
	}
	return outcomes, nil
}

// runOne 跑一次"任务 × 外壳"组合并采集观测。
func runOne(ctx context.Context, harness Harness, task Task, options ExperimentOptions) (ExperimentOutcome, error) {
	outcome := ExperimentOutcome{TaskID: task.ID, Harness: harness.Name()}
	root := filepath.Join(options.Root, task.ID, harness.Name())
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return outcome, fmt.Errorf("create workspace: %w", err)
	}
	for name, content := range task.Fixtures {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return outcome, fmt.Errorf("create fixture dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return outcome, fmt.Errorf("write fixture: %w", err)
		}
	}
	if preparer, ok := harness.(interface{ WriteConfig(string) error }); ok {
		if err := preparer.WriteConfig(workspace); err != nil {
			return outcome, err
		}
	}
	before, err := SnapshotWorkspace(root)
	if err != nil {
		return outcome, err
	}
	run := HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: task.Prompt(), Timeout: options.Timeout,
		InterruptAfter: options.InterruptAfter,
	}
	result, err := harness.Run(ctx, run)
	if err != nil {
		return outcome, err
	}
	after, err := SnapshotWorkspace(root)
	if err != nil {
		return outcome, err
	}
	outcome.ExitCode = result.ExitCode
	outcome.ChangedFiles = ChangedFiles(before, after)
	outcome.Usage = result.Usage
	outcome.Duration = result.Duration
	outcome.SessionHandle = result.SessionHandle
	outcome.Interrupted = result.Interrupted
	outcome.ApprovalRoundTrips = parseApprovalCount(result.Extra)
	for _, changed := range outcome.ChangedFiles {
		if !strings.HasPrefix(changed, "ws/") {
			outcome.EscapedWorkspace = true
			break
		}
	}
	outcome.Success = judgeSuccess(task, result, outcome)
	outcome.Notes = collectNotes(task, result, outcome)
	return outcome, nil
}

// judgeSuccess 判定一次运行是否达成了任务脚本的终点：外壳退出码为成功、
// 脚本已走完、且结论文本已出现。
func judgeSuccess(task Task, result HarnessResult, outcome ExperimentOutcome) bool {
	if outcome.Interrupted {
		return false
	}
	if result.ExitCode != 0 {
		return false
	}
	if !scriptCompleted(task, result) {
		return false
	}
	return true
}

// scriptCompleted 判断脚本是否已走完：要么任务没有脚本步骤，要么外壳
// 报告了脚本结束标记或执行了全部步骤。
func scriptCompleted(task Task, result HarnessResult) bool {
	if len(task.Script.Steps) == 0 {
		return true
	}
	if strings.Contains(result.Stdout, "INTEROP_DONE") {
		return true
	}
	return false
}

// parseApprovalCount 从外壳补充观测中取出审批往返次数。
func parseApprovalCount(extra map[string]string) int {
	value, ok := extra["approval_round_trips"]
	if !ok {
		return 0
	}
	count := 0
	for _, character := range value {
		if character < '0' || character > '9' {
			break
		}
		count = count*10 + int(character-'0')
	}
	return count
}

// collectNotes 汇总一次运行的补充说明。
func collectNotes(task Task, result HarnessResult, outcome ExperimentOutcome) []string {
	var notes []string
	if reason, ok := result.Extra["terminal_reason"]; ok && reason != "" {
		notes = append(notes, "终态原因: "+reason)
	}
	if denials, ok := result.Extra["permission_denials"]; ok && denials != "0" {
		notes = append(notes, "权限拒绝数: "+denials)
	}
	if outcome.EscapedWorkspace {
		notes = append(notes, "副作用越出工作区")
	}
	if outcome.Interrupted {
		notes = append(notes, "运行被外部信号中断")
	}
	if !outcome.Success {
		notes = append(notes, fmt.Sprintf("退出码 %d", outcome.ExitCode))
	}
	if task.ExpectEscape && !outcome.EscapedWorkspace {
		notes = append(notes, "越界写入被拦截")
	}
	return notes
}

// RenderExperiments 把实验结果渲染为可读表格，供报告直接引用。
func RenderExperiments(outcomes []ExperimentOutcome) string {
	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("%-20s %-14s %-6s %-8s %-8s %-9s %-8s %s\n",
		"任务", "外壳", "成功", "退出码", "越界", "改动文件", "审批往返", "说明"))
	for _, outcome := range outcomes {
		builder.WriteString(fmt.Sprintf("%-20s %-14s %-6s %-8d %-8s %-9d %-8d %s\n",
			outcome.TaskID, outcome.Harness, yesNo(outcome.Success), outcome.ExitCode,
			yesNo(outcome.EscapedWorkspace), len(outcome.ChangedFiles), outcome.ApprovalRoundTrips,
			strings.Join(outcome.Notes, "；")))
	}
	return builder.String()
}

// SummarizeByHarness 按外壳汇总成功率、平均耗时与审批往返总数。
func SummarizeByHarness(outcomes []ExperimentOutcome) []string {
	grouped := map[string][]ExperimentOutcome{}
	for _, outcome := range outcomes {
		grouped[outcome.Harness] = append(grouped[outcome.Harness], outcome)
	}
	names := make([]string, 0, len(grouped))
	for name := range grouped {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		items := grouped[name]
		successes := 0
		escapes := 0
		approvals := 0
		var total time.Duration
		var reportedUsage int
		var inputTokens, outputTokens int
		for _, item := range items {
			if item.Success {
				successes++
			}
			if item.EscapedWorkspace {
				escapes++
			}
			approvals += item.ApprovalRoundTrips
			total += item.Duration
			if item.Usage.Reported {
				reportedUsage++
				inputTokens += item.Usage.InputTokens
				outputTokens += item.Usage.OutputTokens
			}
		}
		lines = append(lines, fmt.Sprintf(
			"%s: 成功 %d/%d，越界 %d，审批往返合计 %d，平均耗时 %s，报告用量的运行 %d 个（输入 %d / 输出 %d token）",
			name, successes, len(items), escapes, approvals, (total/time.Duration(len(items))).Round(time.Millisecond),
			reportedUsage, inputTokens, outputTokens))
	}
	return lines
}

// yesNo 把布尔值渲染为中文短标签。
func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}
