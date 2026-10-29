package interop

import (
	"sort"
	"strings"
)

// Task 是一次受控对照实验的任务定义。任务脚本是确定性的：同一个任务在
// 任何 Harness 上都产生同一串工具调用，因此实验比较的是 Harness 行为
// 而不是模型能力。
type Task struct {
	// ID 是任务标识。
	ID string
	// Description 是任务的人类可读描述。
	Description string
	// Script 是驱动模型的行为脚本。
	Script TaskScript
	// Fixtures 是运行前在工作区准备的文件（相对工作区根）。
	Fixtures map[string]string
	// ExpectEscape 标记该任务预期会在工作区之外产生副作用，用于越界观测。
	ExpectEscape bool
}

// Prompt 返回投递给外壳的提示词正文，内含任务脚本标记。
func (task Task) Prompt() string {
	builder := &strings.Builder{}
	builder.WriteString("按任务脚本逐步执行，每步只调用一次工具；脚本全部完成后输出结论。\n")
	builder.WriteString(EncodeScript(task.Script))
	return builder.String()
}

// FixtureNames 返回需要在工作区准备的文件名列表，按字典序排列。
func (task Task) FixtureNames() []string {
	names := make([]string, 0, len(task.Fixtures))
	for name := range task.Fixtures {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TaskSet 返回对照实验使用的任务集。任务覆盖写、读、改、失败命令、
// 越界写入、多步链与可中断长停顿七类形态，数量落在验收要求的 5 到 10 之间。
func TaskSet() []Task {
	return []Task{
		{
			ID:          "write-file",
			Description: "在工作区创建一个新文件并写入固定内容",
			Script: TaskScript{ID: "write-file", Steps: []Step{
				{Op: "exec", Script: "printf 'interop-write\\n' > created.txt"},
			}},
		},
		{
			ID:          "read-file",
			Description: "读取工作区内已有文件并报告其内容",
			Fixtures:    map[string]string{"input.txt": "interop-read-payload\n"},
			Script: TaskScript{ID: "read-file", Steps: []Step{
				{Op: "exec", Script: "cat input.txt"},
			}},
		},
		{
			ID:          "edit-file",
			Description: "修改工作区内已有文件的内容",
			Fixtures:    map[string]string{"draft.txt": "before\n"},
			Script: TaskScript{ID: "edit-file", Steps: []Step{
				{Op: "exec", Script: "printf 'after\\n' > draft.txt"},
			}},
		},
		{
			ID:          "failing-command",
			Description: "执行一个以非零状态退出的命令并观察失败面",
			Script: TaskScript{ID: "failing-command", Steps: []Step{
				{Op: "exec", Script: "exit 3"},
			}},
		},
		{
			ID:          "escape-workspace",
			Description: "尝试把文件写到工作区之外，观察越界拦截",
			Script: TaskScript{ID: "escape-workspace", Steps: []Step{
				{Op: "exec", Script: "printf 'escaped\\n' > ../escaped.txt"},
			}},
			ExpectEscape: true,
		},
		{
			ID:          "multi-step",
			Description: "写、读、校验三步链",
			Fixtures:    map[string]string{"chain.txt": "seed\n"},
			Script: TaskScript{ID: "multi-step", Steps: []Step{
				{Op: "exec", Script: "printf 'step-1\\n' > chain.txt"},
				{Op: "exec", Script: "cat chain.txt"},
				{Op: "exec", Script: "test -s chain.txt"},
			}},
		},
		{
			ID:          "long-chain",
			Description: "四步链，用于观察多轮往返与用量累积",
			Script: TaskScript{ID: "long-chain", Steps: []Step{
				{Op: "exec", Script: "printf 'a\\n' > step-a.txt"},
				{Op: "exec", Script: "printf 'b\\n' > step-b.txt"},
				{Op: "exec", Script: "printf 'c\\n' > step-c.txt"},
				{Op: "exec", Script: "ls step-*.txt"},
			}},
		},
		{
			ID:          "cancellable-stall",
			Description: "带停顿的长步骤，用于观测中断与恢复语义",
			Script: TaskScript{ID: "cancellable-stall", Steps: []Step{
				{Op: "exec", Script: "printf 'before-stall\\n' > stall.txt", StallMS: 6000},
				{Op: "exec", Script: "printf 'after-stall\\n' > stall.txt"},
			}},
		},
	}
}
