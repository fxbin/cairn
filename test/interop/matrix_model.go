package interop

import (
	"fmt"
	"strings"
)

// Capability 是兼容性矩阵的一行：把外部 Agent CLI 当作外层 Harness 时，
// Cairn 需要与它对齐的能力面。取值与验收口径一一对应。
type Capability string

const (
	// CapabilityStart 覆盖"能否以非交互方式启动一次运行并拿到退出码"。
	CapabilityStart Capability = "start"
	// CapabilityStream 覆盖"能否拿到结构化的增量事件流而非最终文本"。
	CapabilityStream Capability = "stream"
	// CapabilityCancel 覆盖"能否在运行中中断并得到确定的终止语义"。
	CapabilityCancel Capability = "cancel"
	// CapabilityResume 覆盖"能否凭外部会话句柄恢复一次中断的运行"。
	CapabilityResume Capability = "resume"
	// CapabilityApproval 覆盖"运行期权限提示能否回到宿主决策"。
	CapabilityApproval Capability = "approval"
	// CapabilityArtifact 覆盖"能否把产物或文件修改事件交给外部记录"。
	CapabilityArtifact Capability = "artifact"
	// CapabilityUsage 覆盖"能否拿到 token 用量与费用口径"。
	CapabilityUsage Capability = "usage"
	// CapabilityError 覆盖"失败是否有结构化、可分类的错误面"。
	CapabilityError Capability = "error"
)

// AllCapabilities 返回全部能力行，顺序即矩阵行序。
func AllCapabilities() []Capability {
	return []Capability{
		CapabilityStart,
		CapabilityStream,
		CapabilityCancel,
		CapabilityResume,
		CapabilityApproval,
		CapabilityArtifact,
		CapabilityUsage,
		CapabilityError,
	}
}

// EdgeScenario 是矩阵必须额外覆盖的边界场景：它们决定外层 Harness 的失败
// 形态是否可被 Cairn 的 Event Journal 完整表达。
type EdgeScenario string

const (
	// EdgeSessionLost 覆盖"外部会话记录丢失或不可读"时的降级形态。
	EdgeSessionLost EdgeScenario = "session-lost"
	// EdgeProcessInterrupted 覆盖"宿主进程被外部信号打断"时的残留状态。
	EdgeProcessInterrupted EdgeScenario = "process-interrupted"
	// EdgeRuntimePermissionPrompt 覆盖"运行期弹出权限提示"时的可决策性。
	EdgeRuntimePermissionPrompt EdgeScenario = "runtime-permission-prompt"
	// EdgeFileModificationEvent 覆盖"文件被修改"是否有独立事件面。
	EdgeFileModificationEvent EdgeScenario = "file-modification-event"
)

// AllEdgeScenarios 返回全部边界场景，顺序即矩阵行序。
func AllEdgeScenarios() []EdgeScenario {
	return []EdgeScenario{
		EdgeSessionLost,
		EdgeProcessInterrupted,
		EdgeRuntimePermissionPrompt,
		EdgeFileModificationEvent,
	}
}

// ProbeStatus 标注一条观测的证据等级。矩阵里每格都必须落到某个取值上，
// 不允许用空白或猜测填充。
type ProbeStatus string

const (
	// StatusMeasured 表示已真实驱动过模型往返，事件流或结果结构被实际解析。
	StatusMeasured ProbeStatus = "measured"
	// StatusSurface 表示只做了 CLI 表面探测：帮助文本、退出码、参数契约，
	// 没有产生模型往返。
	StatusSurface ProbeStatus = "surface"
	// StatusUntested 表示本轮未覆盖，原因是范围之外而非不可得。
	StatusUntested ProbeStatus = "untested"
	// StatusUnavailable 表示客观不可得，Reason 必须写明原因。
	StatusUnavailable ProbeStatus = "unavailable"
)

// Observation 是矩阵的一格：某个 CLI 在某项能力（或某个边界场景）上的
// 观测结论。Evidence 只允许放行为摘要，不得放本机绝对路径、账号或凭据。
type Observation struct {
	Capability Capability
	Scenario   EdgeScenario
	CLI        string
	Status     ProbeStatus
	Summary    string
	Evidence   string
	Reason     string
}

// Row 返回该观测所属的矩阵行标识：能力行优先，边界场景行用场景标识。
func (observation Observation) Row() string {
	if observation.Capability != "" {
		return string(observation.Capability)
	}
	return string(observation.Scenario)
}

// Describe 把观测渲染为一行可读文本，供报告直接引用。
func (observation Observation) Describe() string {
	builder := &strings.Builder{}
	fmt.Fprintf(builder, "%-26s %-9s %s", observation.Row(), observation.CLI, observation.Status)
	if observation.Summary != "" {
		fmt.Fprintf(builder, " | %s", observation.Summary)
	}
	if observation.Evidence != "" {
		fmt.Fprintf(builder, " | 证据: %s", observation.Evidence)
	}
	if observation.Reason != "" {
		fmt.Fprintf(builder, " | 原因: %s", observation.Reason)
	}
	return builder.String()
}

// Matrix 是一次兼容性矩阵探测的完整结果：能力行与边界场景行分别按
// CLI 归拢，便于逐格核对。
type Matrix struct {
	Observations []Observation
}

// Add 追加一条观测。
func (matrix *Matrix) Add(observation Observation) {
	matrix.Observations = append(matrix.Observations, observation)
}

// AddAll 批量追加观测。
func (matrix *Matrix) AddAll(observations ...Observation) {
	matrix.Observations = append(matrix.Observations, observations...)
}

// Lookup 返回指定 CLI 与行的观测；不存在时返回零值与 false。
func (matrix Matrix) Lookup(cli string, row string) (Observation, bool) {
	for _, observation := range matrix.Observations {
		if observation.CLI == cli && observation.Row() == row {
			return observation, true
		}
	}
	return Observation{}, false
}

// Render 把矩阵渲染为按行分组的可读文本；每行先列能力与场景名，再按 CLI
// 顺序列出该行的观测。
func (matrix Matrix) Render(clis []string) string {
	builder := &strings.Builder{}
	rows := make([]string, 0, len(AllCapabilities())+len(AllEdgeScenarios()))
	for _, capability := range AllCapabilities() {
		rows = append(rows, string(capability))
	}
	for _, scenario := range AllEdgeScenarios() {
		rows = append(rows, string(scenario))
	}
	for _, row := range rows {
		fmt.Fprintf(builder, "\n== %s ==\n", row)
		for _, cli := range clis {
			observation, ok := matrix.Lookup(cli, row)
			if !ok {
				fmt.Fprintf(builder, "  %-9s %-11s (未探测)\n", cli, StatusUntested)
				continue
			}
			fmt.Fprintf(builder, "  %s\n", observation.Describe())
		}
	}
	return builder.String()
}
