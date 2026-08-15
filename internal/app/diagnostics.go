// Package app 装配面向 CLI 的 Cairn 运行、恢复与诊断用例。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

// ProviderHealth 汇总 Provider 的非敏感运行前提：端点、模型与密钥环境变量的
// 存在性（只出现变量名，绝不出现值）。
type ProviderHealth struct {
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	APIKeyEnv     string `json:"api_key_env"`
	APIKeyPresent bool   `json:"api_key_present"`
}

// SandboxHealth 汇总执行后端的只读探测结果；镜像可用性为 yes/no/unknown，
// 探测失败不改变 doctor 的完整性判定。
type SandboxHealth struct {
	Backend        string `json:"backend"`
	DockerImage    string `json:"docker_image,omitempty"`
	ImageAvailable string `json:"image_available,omitempty"`
}

// DoctorReport 回答"环境和配置是否可运行"：非敏感 resolved 配置、Profile、
// 预算来源、Provider/Sandbox 可用性与 capability manifest；journal_ok 与
// artifacts_ok 对应既有只读完整性检查。
type DoctorReport struct {
	ConfigValid       bool                     `json:"config_valid"`
	Preset            string                   `json:"preset"`
	Profile           string                   `json:"profile"`
	Resolution        []config.ResolutionEntry `json:"resolution,omitempty"`
	Provider          ProviderHealth           `json:"provider"`
	Sandbox           SandboxHealth            `json:"sandbox"`
	Tools             []string                 `json:"tools"`
	VerificationCount int                      `json:"verification_commands"`
	JournalOK         bool                     `json:"journal_ok"`
	ArtifactsOK       bool                     `json:"artifacts_ok"`
}

// Doctor 以只读模式验证工作区对应日志和制品引用的完整性，并返回环境可运行性
// 诊断报告；所有探测均为只读（环境变量只查存在性、Docker 只做本地镜像查询）。
func (service Service) Doctor(ctx context.Context, workspacePath, stateDir string, configuration config.Config) (DoctorReport, error) {
	report := DoctorReport{
		ConfigValid:       config.Validate(configuration) == nil,
		Preset:            configuration.Preset,
		Profile:           configuration.Profile,
		Resolution:        configuration.Resolution,
		Provider:          ProviderHealth{BaseURL: configuration.Provider.BaseURL, Model: configuration.Provider.Model, APIKeyEnv: configuration.Provider.APIKeyEnv, APIKeyPresent: false},
		Sandbox:           SandboxHealth{Backend: configuration.Sandbox.Backend, DockerImage: configuration.Sandbox.DockerImage},
		VerificationCount: len(configuration.Verification.Commands),
	}
	_, report.Provider.APIKeyPresent = os.LookupEnv(configuration.Provider.APIKeyEnv)
	workspace, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return report, err
	}
	if _, err := execToolFor(workspace, configuration); err != nil {
		return report, err
	}
	if configuration.Sandbox.Backend == "docker" {
		report.Sandbox.ImageAvailable = probeDockerImage(configuration.Sandbox.DockerImage)
	}
	report.Tools = fixedToolNames(workspace, configuration.Preset)
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		return report, err
	}
	defer journal.Close()
	if err := journal.Check(ctx); err != nil {
		return report, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	report.JournalOK = true
	if err := checkArtifactReferences(ctx, journal, stateDir, hash); err != nil {
		return report, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	report.ArtifactsOK = true
	return report, nil
}

// probeDockerImage 以无 shell 的只读 argv 查询本地镜像存在性；docker 不可用
// 或镜像缺失分别返回 unknown 与 no，探测本身不产生副作用。
func probeDockerImage(image string) string {
	if image == "" {
		return "unknown"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return "unknown"
	}
	probe := exec.Command("docker", "image", "inspect", image)
	probe.Env = []string{}
	if err := probe.Run(); err != nil {
		return "no"
	}
	return "yes"
}

// fixedToolNames 返回固定注册表的能力清单顺序；注册表构造是纯装配，不产生
// 任何副作用，符合 doctor 的只读合同。read-only Preset 只列出三件只读工具。
func fixedToolNames(workspace tools.Workspace, preset string) []string {
	registry, err := tools.NewRegistry(readOnlySafeTools(workspace, preset)...)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(registry.Specs()))
	for _, spec := range registry.Specs() {
		names = append(names, spec.Name)
	}
	return names
}

// readOnlySafeTools 按 Preset 返回注册表工具子集；read-only 只组合三件只读
// 工具，coding 与 benchmark 使用完整五件。是 Boot 装配与 doctor 清单的唯一
// 共同来源，防止能力清单双轨漂移。
func readOnlySafeTools(workspace tools.Workspace, preset string) []tools.Tool {
	execTool := tools.ExecCommand{Workspace: workspace}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	available := []tools.Tool{tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace}, patchTool, execTool}
	if preset == config.PresetReadOnly {
		return available[:3]
	}
	return available
}

// VerificationFact 摘要最近一次验证的序号与结果。
type VerificationFact struct {
	Seq uint64 `json:"seq"`
	OK  bool   `json:"ok"`
}

// RunSummary 回答"这个 Run 发生了什么"：当前状态、停止原因、预算使用、
// 最新验证进展、待决审批/对账、下一步恢复动作与完成证据绑定；全部从已持久化
// 事件与投影派生，不依赖隐藏日志或模型思维链。
type RunSummary struct {
	State             string            `json:"state"`
	Phase             string            `json:"phase"`
	StopReason        string            `json:"stop_reason,omitempty"`
	TurnsUsed         int               `json:"turns_used"`
	ToolCallsStarted  int               `json:"tool_calls_started"`
	InputTokens       int64             `json:"input_tokens"`
	OutputTokens      int64             `json:"output_tokens"`
	UsageEstimated    bool              `json:"usage_estimated"`
	LastMutationSeq   uint64            `json:"last_mutation_seq"`
	LastVerification  *VerificationFact `json:"last_verification,omitempty"`
	PendingApproval   bool              `json:"pending_approval"`
	PendingOperation  bool              `json:"pending_operation"`
	NextAction        string            `json:"next_action,omitempty"`
	CompletionTrusted bool              `json:"completion_trusted"`
}

// SummarizeRun 从事件流与投影构建诊断摘要；事件 payload 在写入时已经过脱敏，
// 此处只读取既有事实。
func SummarizeRun(events []domain.Event, projection domain.RunProjection) RunSummary {
	summary := RunSummary{
		State:            string(projection.State),
		Phase:            projection.Phase(),
		PendingApproval:  projection.PendingApproval != nil,
		PendingOperation: projection.PendingOperation != nil,
	}
	for _, event := range events {
		switch event.Type {
		case domain.EventTurnOpened:
			summary.TurnsUsed++
		case domain.EventOperationStarted:
			summary.ToolCallsStarted++
		case domain.EventModelResponseFinished:
			var payload struct {
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
				UsageEstimated bool `json:"usage_estimated"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Usage != nil {
				summary.InputTokens += int64(payload.Usage.PromptTokens)
				summary.OutputTokens += int64(payload.Usage.CompletionTokens)
				summary.UsageEstimated = summary.UsageEstimated || payload.UsageEstimated
			}
		case domain.EventVerificationFinished:
			var payload domain.VerificationFinishedPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				summary.LastVerification = &VerificationFact{Seq: event.Seq, OK: payload.OK}
			}
		}
	}
	summary.LastMutationSeq = projection.LastMutationSeq
	summary.StopReason = terminalStopReason(events)
	summary.NextAction = nextActionFor(projection)
	summary.CompletionTrusted = projection.State == domain.RunCompleted && projection.VerificationSucceeded && projection.LastVerificationSeq != 0 && (projection.LastMutationSeq == 0 || projection.LastVerificationSeq > projection.LastMutationSeq)
	return summary
}

// terminalStopReason 从终态事件提取人可读的停止原因；仅读取已脱敏的持久化文本。
func terminalStopReason(events []domain.Event) string {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		switch event.Type {
		case domain.EventRunFailed:
			var payload struct {
				Error      string `json:"error"`
				ErrorClass string `json:"error_class"`
				Budget     string `json:"budget_dimension"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				return "run failed"
			}
			reason := payload.ErrorClass
			if reason == "" {
				reason = payload.Budget
			}
			if reason == "" {
				reason = payload.Error
			}
			if reason == "" {
				reason = "run failed"
			}
			return reason
		case domain.EventRunCancelled:
			var payload struct {
				Cause string `json:"cause"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Cause != "" {
				return "cancelled: " + payload.Cause
			}
			return "cancelled"
		case domain.EventRunCompleted:
			return ""
		}
	}
	return ""
}

// nextActionFor 依据投影状态给出可发现的恢复动作提示；与 Run 的 ErrActiveRun
// 指引保持同一语义。
func nextActionFor(projection domain.RunProjection) string {
	if projection.PendingApproval != nil {
		return "resume <run-id> --approve <fingerprint>"
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return "resume <run-id> --resolve <operation-id>=<verdict> --note <text>"
	}
	switch projection.State {
	case domain.RunRunning:
		return "resume <run-id>"
	case domain.RunCreated, domain.RunCancelling:
		return "resume <run-id> --abort-run --reason <text>"
	}
	return ""
}
