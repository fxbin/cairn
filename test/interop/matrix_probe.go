package interop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Resumer 是可凭外部会话句柄恢复一次运行的外壳。恢复语义是兼容性矩阵的
// 独立一行：Cairn 的恢复是带外命令，外部 CLI 的恢复是同一条命令的子命令。
type Resumer interface {
	// Resume 凭会话句柄恢复一次运行。
	Resume(ctx context.Context, run HarnessRun, handle string) (HarnessResult, error)
}

// probeFunc 是单项能力探测。探测只允许在临时目录内进行，且不得读取或
// 写入任何真实账号凭据。
type probeFunc func(ctx context.Context, harness Harness, stub *Stub) Observation

// capabilityProbe 把一项能力绑定到它的探测实现。
type capabilityProbe struct {
	Capability Capability
	Probe      probeFunc
}

// capabilityProbes 是能力矩阵的探测清单，顺序即矩阵行序。
var capabilityProbes = []capabilityProbe{
	{CapabilityStart, probeStart},
	{CapabilityStream, probeStream},
	{CapabilityCancel, probeCancel},
	{CapabilityResume, probeResume},
	{CapabilityApproval, probeApproval},
	{CapabilityArtifact, probeArtifact},
	{CapabilityUsage, probeUsage},
	{CapabilityError, probeError},
}

// edgeProbes 是边界场景的探测清单，顺序即矩阵行序。
var edgeProbes = []capabilityProbe{
	{Capability: "", Probe: probeSessionLost},
	{Capability: "", Probe: probeProcessInterrupted},
	{Capability: "", Probe: probeRuntimePermissionPrompt},
	{Capability: "", Probe: probeFileModificationEvent},
}

// ProbeMatrix 对一组外壳执行完整的能力与边界场景探测。
func ProbeMatrix(ctx context.Context, harnesses []Harness, stub *Stub) Matrix {
	matrix := Matrix{}
	for _, harness := range harnesses {
		for _, probe := range capabilityProbes {
			matrix.Add(probe.Probe(ctx, harness, stub))
		}
		for _, probe := range edgeProbes {
			matrix.Add(probe.Probe(ctx, harness, stub))
		}
	}
	return matrix
}

// probeWorkspace 为一次探测准备独立工作区，并返回工作区与快照根。
func probeWorkspace(harness Harness) (string, string, error) {
	root, err := os.MkdirTemp("", "cairn-interop-probe-")
	if err != nil {
		return "", "", fmt.Errorf("create probe root: %w", err)
	}
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return "", "", fmt.Errorf("create probe workspace: %w", err)
	}
	if preparer, ok := harness.(interface{ WriteConfig(string) error }); ok {
		if err := preparer.WriteConfig(workspace); err != nil {
			return "", "", fmt.Errorf("write probe config: %w", err)
		}
	}
	return workspace, root, nil
}

// probeStart 探测非交互启动能力：能否以进程方式启动一次运行并拿到退出码。
func probeStart(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Capability: CapabilityStart, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[0].Prompt(), Timeout: interopRunTimeout,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	observation.Status = StatusMeasured
	observation.Summary = fmt.Sprintf("非交互启动成功，退出码 %d", result.ExitCode)
	observation.Evidence = fmt.Sprintf("事件序列 %v", result.Events)
	if result.ExitCode != 0 {
		observation.Summary = fmt.Sprintf("非交互启动未成功，退出码 %d", result.ExitCode)
	}
	return observation
}

// probeStream 探测结构化增量事件流能力：输出是增量事件而非最终文本。
func probeStream(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Capability: CapabilityStream, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[5].Prompt(), Timeout: interopRunTimeout,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	structured := len(result.Events) > 1
	observation.Status = StatusMeasured
	observation.Evidence = fmt.Sprintf("事件序列 %v", result.Events)
	if structured {
		observation.Summary = fmt.Sprintf("输出为结构化增量事件，共 %d 条", len(result.Events))
	} else {
		observation.Summary = "输出只有单一结果，没有增量事件面"
	}
	return observation
}

// probeCancel 探测取消能力：在运行中打断进程后的退出码与可恢复性。
func probeCancel(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Capability: CapabilityCancel, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	task := cancellableTask()
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: task.Prompt(), Timeout: interopRunTimeout,
		InterruptAfter: 2 * time.Second,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	observation.Status = StatusMeasured
	observation.Summary = fmt.Sprintf("中断后退出码 %d，被判定为已中断=%v；退出码本身能否区分中断=%v",
		result.ExitCode, result.Interrupted, result.ExitCode != 0)
	observation.Evidence = fmt.Sprintf("会话句柄 %q；事件序列 %v", result.SessionHandle, result.Events)
	if result.SessionHandle == "" {
		observation.Summary += "；中断前未产出可恢复的会话句柄"
	}
	return observation
}

// probeResume 探测恢复能力：凭会话句柄能否把中断的运行接着跑完。
func probeResume(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Capability: CapabilityResume, CLI: harness.Name()}
	resumer, ok := harness.(Resumer)
	if !ok {
		observation.Status = StatusUnavailable
		observation.Reason = "该外壳没有暴露会话句柄恢复入口"
		return observation
	}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	task := cancellableTask()
	first, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: task.Prompt(), Timeout: interopRunTimeout,
		InterruptAfter: 2 * time.Second,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	if first.SessionHandle == "" {
		observation.Status = StatusMeasured
		observation.Summary = "中断后没有产出可恢复的会话句柄，恢复入口不可用"
		return observation
	}
	second, err := resumer.Resume(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: task.Prompt(), Timeout: interopRunTimeout,
	}, first.SessionHandle)
	if err != nil {
		observation.Status = StatusMeasured
		observation.Summary = "凭会话句柄恢复失败"
		observation.Evidence = err.Error()
		return observation
	}
	observation.Status = StatusMeasured
	observation.Summary = fmt.Sprintf("凭会话句柄恢复后退出码 %d，任务是否走完=%v",
		second.ExitCode, strings.Contains(second.Stdout, "INTEROP_DONE"))
	observation.Evidence = fmt.Sprintf("恢复后事件序列 %v；stderr %s", second.Events, truncateText(second.Stderr, 200))
	return observation
}

// probeApproval 探测运行期权限提示：宿主能否拿到可决策的授权面。
func probeApproval(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Capability: CapabilityApproval, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[0].Prompt(), Timeout: interopRunTimeout,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	observation.Status = StatusMeasured
	observation.Evidence = fmt.Sprintf("补充观测 %v；事件序列 %v", result.Extra, result.Events)
	observation.Summary = describeApprovalSurface(harness.Name(), result)
	return observation
}

// describeApprovalSurface 把不同外壳的授权面归纳成一句可比结论。
func describeApprovalSurface(name string, result HarnessResult) string {
	switch name {
	case "cairn-native":
		roundTrips := result.Extra["approval_round_trips"]
		return fmt.Sprintf("审批为带外流程：运行以退出码 6 停在待审批态，由独立命令按指纹放行，本次往返 %s 次", roundTrips)
	case "codex":
		return "审批在进程内自动判定，非交互模式下没有可供宿主应答的提示面"
	case "claude":
		if denials := result.Extra["permission_denials"]; denials != "" && denials != "0" {
			return "无人应答时权限提示被自动拒绝，拒绝项作为结构化字段回报"
		}
		return "权限提示在非交互模式下由权限模式直接判定，无独立提示面"
	}
	return "授权面未能归类"
}

// probeArtifact 探测产物面：文件修改是否有独立的事件表达。
func probeArtifact(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Capability: CapabilityArtifact, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	before, err := SnapshotWorkspace(root)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[0].Prompt(), Timeout: interopRunTimeout,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	after, err := SnapshotWorkspace(root)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	changed := ChangedFiles(before, after)
	observation.Status = StatusMeasured
	observation.Summary = fmt.Sprintf("工作区改动 %d 个文件", len(changed))
	observation.Evidence = fmt.Sprintf("改动文件 %v；事件序列 %v", changed, result.Events)
	return observation
}

// probeUsage 探测用量面：是否报告 token 用量与费用。
func probeUsage(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Capability: CapabilityUsage, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[0].Prompt(), Timeout: interopRunTimeout,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	observation.Status = StatusMeasured
	if !result.Usage.Reported {
		observation.Summary = "没有报告任何 token 用量"
		observation.Evidence = fmt.Sprintf("事件序列 %v", result.Events)
		return observation
	}
	observation.Summary = fmt.Sprintf("报告 token 用量：输入 %d、输出 %d", result.Usage.InputTokens, result.Usage.OutputTokens)
	observation.Evidence = fmt.Sprintf("费用口径 %v", result.Usage.CostUSD)
	return observation
}

// probeError 探测错误面：失败是否有结构化、可分类的表达。
func probeError(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Capability: CapabilityError, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[3].Prompt(), Timeout: interopRunTimeout,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	observation.Status = StatusMeasured
	failures := countFailureEvents(harness.Name(), result.Events)
	observation.Summary = fmt.Sprintf("工具以非零状态失败后，宿主进程退出码 %d，失败事件 %d 条",
		result.ExitCode, failures)
	observation.Evidence = fmt.Sprintf("事件序列 %v", result.Events)
	return observation
}

// countFailureEvents 统计事件流里表达工具失败或运行失败的条目数。
func countFailureEvents(name string, events []string) int {
	count := 0
	for _, event := range events {
		switch name {
		case "cairn-native":
			if event == "operation_failed" || event == "run_failed" {
				count++
			}
		case "codex":
			if event == "turn.failed" || event == "item.completed:error" || event == "item.completed:command_execution" {
				count++
			}
		case "claude":
			if event == "user" {
				count++
			}
		}
	}
	return count
}

// probeSessionLost 探测会话丢失：外部会话记录不可用时的降级形态。
func probeSessionLost(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Scenario: EdgeSessionLost, CLI: harness.Name()}
	resumer, ok := harness.(Resumer)
	if !ok {
		observation.Status = StatusUnavailable
		observation.Reason = "该外壳没有暴露会话句柄恢复入口"
		return observation
	}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := resumer.Resume(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[0].Prompt(), Timeout: interopRunTimeout,
	}, "interop-missing-session-handle")
	if err != nil {
		observation.Status = StatusMeasured
		observation.Summary = "用不存在的会话句柄恢复时进程本身报错"
		observation.Evidence = truncateText(err.Error(), 200)
		return observation
	}
	observation.Status = StatusMeasured
	if result.ExitCode != 0 {
		observation.Summary = fmt.Sprintf("用不存在的会话句柄恢复以退出码 %d 失败关闭，错误面：%s",
			result.ExitCode, truncateText(firstNonEmpty(result.Stderr, result.Stdout), 200))
		observation.Evidence = fmt.Sprintf("事件序列 %v", result.Events)
		return observation
	}
	observation.Summary = "用不存在的会话句柄恢复没有失败关闭：进程以退出码 0 结束"
	if result.SessionHandle != "" && result.SessionHandle != "interop-missing-session-handle" {
		observation.Summary += fmt.Sprintf("，并产出了新的会话句柄 %q（静默新建会话降级）", result.SessionHandle)
	}
	observation.Evidence = fmt.Sprintf("事件序列 %v", result.Events)
	return observation
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "(无输出)"
}

// probeProcessInterrupted 探测进程中断：外部信号打断后的残留状态。
func probeProcessInterrupted(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Scenario: EdgeProcessInterrupted, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	before, err := SnapshotWorkspace(root)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: cancellableTask().Prompt(), Timeout: interopRunTimeout,
		InterruptAfter: 2 * time.Second,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	after, err := SnapshotWorkspace(root)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	changed := ChangedFiles(before, after)
	observation.Status = StatusMeasured
	observation.Summary = fmt.Sprintf("中断后退出码 %d，残留改动 %d 个文件，会话句柄 %q",
		result.ExitCode, len(changed), result.SessionHandle)
	observation.Evidence = fmt.Sprintf("残留文件 %v", changed)
	return observation
}

// probeRuntimePermissionPrompt 探测运行期权限提示的可决策性。
func probeRuntimePermissionPrompt(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Scenario: EdgeRuntimePermissionPrompt, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[0].Prompt(), Timeout: interopRunTimeout,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	observation.Status = StatusMeasured
	observation.Summary = describeApprovalSurface(harness.Name(), result)
	observation.Evidence = fmt.Sprintf("补充观测 %v", result.Extra)
	return observation
}

// probeFileModificationEvent 探测文件修改事件：是否有独立于工具结果的
// 修改事件面。
func probeFileModificationEvent(ctx context.Context, harness Harness, stub *Stub) Observation {
	observation := Observation{Scenario: EdgeFileModificationEvent, CLI: harness.Name()}
	workspace, root, err := probeWorkspace(harness)
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	result, err := harness.Run(ctx, HarnessRun{
		Workspace: workspace, SnapshotRoot: root,
		Task: TaskSet()[0].Prompt(), Timeout: interopRunTimeout,
	})
	if err != nil {
		observation.Status = StatusUnavailable
		observation.Reason = err.Error()
		return observation
	}
	observation.Status = StatusMeasured
	observation.Summary = describeFileEventSurface(harness.Name(), result)
	observation.Evidence = fmt.Sprintf("事件序列 %v", result.Events)
	return observation
}

// describeFileEventSurface 归纳文件修改的事件表达。
func describeFileEventSurface(name string, result HarnessResult) string {
	switch name {
	case "cairn-native":
		return "有独立的 operation_started/operation_succeeded 副作用事件，带操作标识"
	case "codex":
		return "文件修改混在命令执行事件里，没有独立的文件变更事件"
	case "claude":
		return "文件修改混在工具调用事件里，没有独立的文件变更事件"
	}
	return "未能归类"
}

// cancellableTask 返回带停顿的可中断任务。
func cancellableTask() Task {
	for _, task := range TaskSet() {
		if task.ID == "cancellable-stall" {
			return task
		}
	}
	return TaskSet()[0]
}

// truncateText 截断长文本。
func truncateText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
