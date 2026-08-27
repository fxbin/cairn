// Package app 装配面向 CLI 的 Cairn 运行、恢复与诊断用例。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime/debug"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
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
// 预算来源、Provider/Sandbox 可用性、Provider 能力卡与 capability manifest；
// journal_ok 与 artifacts_ok 对应既有只读完整性检查。ready 是总体就绪判定：
// 配置有效、能力卡可解析、Provider 前提（基地址、模型、密钥环境变量）在位、
// Docker 后端镜像存在、journal 完整或尚不存在（支持首次运行前 preflight）。
// MemoryState/MemoryOK 单独报告二级记忆控制面的状态；它不参与新工作区 ready
// 判定，诊断永远只读。
type DoctorReport struct {
	ConfigValid       bool                             `json:"config_valid"`
	Ready             bool                             `json:"ready"`
	Preset            string                           `json:"preset"`
	Profile           string                           `json:"profile"`
	ConfigFingerprint string                           `json:"config_fingerprint"`
	BuildRevision     string                           `json:"build_revision,omitempty"`
	Build             domain.BuildIdentity             `json:"build"`
	WorkspaceHash     string                           `json:"workspace_hash"`
	Policy            PolicyHealth                     `json:"policy"`
	Resolution        []config.ResolutionEntry         `json:"resolution,omitempty"`
	Provider          ProviderHealth                   `json:"provider"`
	ProviderCard      *contract.ProviderCapabilityCard `json:"provider_card,omitempty"`
	ProviderCardError string                           `json:"provider_card_error,omitempty"`
	Sandbox           SandboxHealth                    `json:"sandbox"`
	Tools             []string                         `json:"tools"`
	VerificationCount int                              `json:"verification_commands"`
	Capabilities      CapabilityManifest               `json:"capability_manifest"`
	Telemetry         TelemetryHealth                  `json:"telemetry"`
	JournalState      string                           `json:"journal_state"`
	JournalOK         bool                             `json:"journal_ok"`
	ArtifactsOK       bool                             `json:"artifacts_ok"`
	MemoryState       string                           `json:"memory_state"`
	MemoryOK          bool                             `json:"memory_ok"`
}

// PolicyHealth 暴露写入与命令执行策略的非敏感结论；doctor 只报告，不决策。
type PolicyHealth struct {
	Write string `json:"write"`
	Exec  string `json:"exec"`
}

// TelemetryHealth 如实报告遥测的真实接线状态：当前版本没有接入任何
// TelemetrySink 或基础设施探针，状态恒为 disabled。测试锁定该诚实合同——
// otelc 构建形态落地时必须同步更新本投影，不得提前宣称任何观测能力。
type TelemetryHealth struct {
	Status string `json:"status"`
}

// Doctor 以只读模式验证工作区对应日志和制品引用的完整性，并返回环境可运行性
// 诊断报告；所有探测均为只读（环境变量只查存在性、Docker 只做本地镜像查询）。
// 新工作区没有 journal 时不报错，journal_state=absent 并继续给出 ready 判定。
func (service Service) Doctor(ctx context.Context, workspacePath, stateDir string, configuration config.Config) (DoctorReport, error) {
	report := DoctorReport{
		ConfigValid:       config.Validate(configuration) == nil,
		Preset:            configuration.Preset,
		Profile:           configuration.Profile,
		ConfigFingerprint: configuration.Fingerprint(),
		Build:             buildIdentity(),
		WorkspaceHash:     "",
		Policy:            PolicyHealth{Write: configuration.Policy.Write, Exec: configuration.Policy.Exec},
		Resolution:        configuration.Resolution,
		Provider:          ProviderHealth{BaseURL: configuration.Provider.BaseURL, Model: configuration.Provider.Model, APIKeyEnv: configuration.Provider.APIKeyEnv, APIKeyPresent: false},
		Sandbox:           SandboxHealth{Backend: configuration.Sandbox.Backend, DockerImage: configuration.Sandbox.DockerImage},
		Tools:             nil,
		VerificationCount: len(configuration.Verification.Commands),
		JournalState:      "absent",
		MemoryState:       "absent",
		Telemetry:         TelemetryHealth{Status: "disabled"},
	}
	report.BuildRevision = report.Build.Revision
	_, report.Provider.APIKeyPresent = os.LookupEnv(configuration.Provider.APIKeyEnv)
	// 能力卡解析在 doctor 中保持只读：失败只记录错误并压低 ready，不中断
	// 其余诊断；成功则进入 manifest 与报告供运行前解释。
	card, cardErr := resolveProviderCard(configuration)
	var cardPtr *contract.ProviderCapabilityCard
	if cardErr == nil {
		cardPtr = &card
		report.ProviderCard = &card
	} else {
		report.ProviderCardError = cardErr.Error()
	}
	workspace, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return report, err
	}
	report.WorkspaceHash = hash
	if _, err := execToolFor(workspace, configuration); err != nil {
		return report, err
	}
	if configuration.Sandbox.Backend == "docker" {
		report.Sandbox.ImageAvailable = probeDockerImage(configuration.Sandbox.DockerImage)
	}
	capabilities := toolCapabilities(workspace, configuration.Preset)
	report.Tools = make([]string, 0, len(capabilities))
	for _, tool := range capabilities {
		report.Tools = append(report.Tools, tool.Name)
	}
	verification, err := verificationCommands(configuration.Verification)
	if err != nil {
		return report, err
	}
	report.Capabilities = buildCapabilityManifest(configuration, capabilities, verification, cardPtr)
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			memoryStore, memoryErr := openReadOnlyMemory(stateDir, hash)
			if memoryErr == nil {
				_ = memoryStore.Close()
				return report, fmt.Errorf("%w: memory store exists but source journal is absent", ErrStateCorrupt)
			}
			if !errors.Is(memoryErr, fs.ErrNotExist) {
				return report, fmt.Errorf("%w: %v", ErrStateCorrupt, memoryErr)
			}
			report.evaluateReadiness()
			return report, nil
		}
		return report, err
	}
	defer journal.Close()
	if err := journal.Check(ctx); err != nil {
		return report, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	report.JournalState = "ok"
	report.JournalOK = true
	if err := checkArtifactReferences(ctx, journal, stateDir, hash); err != nil {
		return report, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	report.ArtifactsOK = true
	memoryState, memoryOK, err := checkMemoryReferences(ctx, journal, stateDir, hash)
	if err != nil {
		return report, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	report.MemoryState, report.MemoryOK = memoryState, memoryOK
	report.evaluateReadiness()
	return report, nil
}

// evaluateReadiness 汇总总体就绪判定：配置有效、能力卡可解析、Provider 前提
// 在位、Docker 后端镜像可用、journal 完整或首次运行前尚不存在。
func (report *DoctorReport) evaluateReadiness() {
	sandboxReady := report.Sandbox.Backend != "docker" || report.Sandbox.ImageAvailable == "yes"
	report.Ready = report.ConfigValid && report.ProviderCardError == "" && report.Provider.BaseURL != "" && report.Provider.Model != "" && report.Provider.APIKeyPresent && sandboxReady && (report.JournalState == "ok" || report.JournalState == "absent")
}

// buildIdentity 返回实际二进制的非敏感身份。当前版本没有构建标签或 otelc
// flavor，因此固定为 plain；revision 缺失时保留空值、dirty 元数据缺失时标为
// unknown，以如实表示构建信息不可得。
func buildIdentity() domain.BuildIdentity {
	if info, ok := debug.ReadBuildInfo(); ok {
		return buildIdentityFromSettings(info.Settings)
	}
	return buildIdentityFromSettings(nil)
}

// buildIdentityFromSettings 从 Go BuildInfo 中可公开的 VCS 设置派生构建身份；缺失
// dirty 设置必须保留 unknown，避免把不可知状态误报为干净工作树。
func buildIdentityFromSettings(settings []debug.BuildSetting) domain.BuildIdentity {
	identity := domain.BuildIdentity{Dirty: "unknown", Flavor: "plain"}
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			identity.Revision = setting.Value
		case "vcs.modified":
			if setting.Value == "true" || setting.Value == "false" {
				identity.Dirty = setting.Value
			}
		}
	}
	identity.Fingerprint = identity.ComputeFingerprint()
	return identity
}

// buildIdentityPtr 为 run_created payload 返回独立的构建身份副本；每个 Run 都冻结
// 创建时实际二进制的事实，不依赖 inspect 时所在的二进制。
func buildIdentityPtr() *domain.BuildIdentity {
	identity := buildIdentity()
	return &identity
}

// buildIdentityFingerprint 是构建身份摘要的既有调用面：公式已收敛到领域方法，
// 本函数仅作兼容委托保持单一事实源。
func buildIdentityFingerprint(identity domain.BuildIdentity) string {
	return identity.ComputeFingerprint()
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

// toolCapabilities 返回固定注册表的能力清单顺序与副作用分类；注册表构造是
// 纯装配，不产生任何副作用，符合 doctor 的只读合同。read-only Preset 只列出
// 三件只读工具。doctor 只需非敏感能力事实，副作用工具实例的运行时配置在此
// 无关紧要。
func toolCapabilities(workspace tools.Workspace, preset string) []ToolCapability {
	registry, err := tools.NewRegistry(presetTools(workspace, tools.ExecCommand{Workspace: workspace}, preset)...)
	if err != nil {
		return nil
	}
	capabilities := make([]ToolCapability, 0, len(registry.Specs()))
	for _, spec := range registry.Specs() {
		capabilities = append(capabilities, ToolCapability{Name: spec.Name, SideEffect: string(registry.SideEffect(spec.Name))})
	}
	return capabilities
}

// presetTools 按 Preset 返回注册表工具子集；read-only 只组合三件只读工具，
// coding 与 benchmark 使用完整五件。副作用工具必须传入"已完成配置装配"的
// 实例（EnvAllow/Backend/Grace 等），此处绝不自造裸工具——否则验证命令所需
// 的环境变量会被拒、沙箱配置会被静默丢弃（2026-08-16 AB Gate 回归实证）。
func presetTools(workspace tools.Workspace, execTool tools.ExecCommand, preset string) []tools.Tool {
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
	State                string                `json:"state"`
	Phase                string                `json:"phase"`
	ConfigFingerprint    string                `json:"config_fingerprint,omitempty"`
	Build                *domain.BuildIdentity `json:"build,omitempty"`
	WorkspaceHash        string                `json:"workspace_hash"`
	WorkspaceObservation string                `json:"workspace_observation,omitempty"`
	StopReason           string                `json:"stop_reason,omitempty"`
	TurnsUsed            int                   `json:"turns_used"`
	ToolCallsStarted     int                   `json:"tool_calls_started"`
	InputTokens          int64                 `json:"input_tokens"`
	OutputTokens         int64                 `json:"output_tokens"`
	UsageEstimated       bool                  `json:"usage_estimated"`
	LastMutationSeq      uint64                `json:"last_mutation_seq"`
	LastVerification     *VerificationFact     `json:"last_verification,omitempty"`
	PendingApproval      bool                  `json:"pending_approval"`
	PendingOperation     bool                  `json:"pending_operation"`
	NextAction           string                `json:"next_action,omitempty"`
	CompletionTrusted    bool                  `json:"completion_trusted"`
}

// SummarizeRun 从事件流与投影构建诊断摘要；事件 payload 在写入时已经过脱敏，
// 此处只读取既有事实。
func SummarizeRun(events []domain.Event, projection domain.RunProjection) RunSummary {
	summary := RunSummary{
		State:                string(projection.State),
		Phase:                projection.Phase(),
		ConfigFingerprint:    projection.ConfigFingerprint,
		Build:                cloneBuildIdentity(projection.Build),
		WorkspaceHash:        projection.WorkspaceHash,
		WorkspaceObservation: projection.WorkspaceObservation,
		PendingApproval:      projection.PendingApproval != nil,
		PendingOperation:     projection.PendingOperation != nil,
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
	summary.StopReason = contract.TerminalStopReason(events)
	summary.NextAction = nextActionFor(projection)
	summary.CompletionTrusted = contract.CompletionTrusted(projection.State, projection.VerificationSucceeded, projection.LastVerificationSeq, projection.LastMutationSeq)
	return summary
}

// cloneBuildIdentity 避免诊断摘要与可变投影共享指针，保证两种呈现视图都只读。
func cloneBuildIdentity(identity *domain.BuildIdentity) *domain.BuildIdentity {
	if identity == nil {
		return nil
	}
	copy := *identity
	return &copy
}

// nextActionFor 依据投影状态给出可发现的恢复动作提示；与 Run 的 ErrActiveRun
// 指引保持同一语义。
func nextActionFor(projection domain.RunProjection) string {
	if projection.PendingApproval != nil {
		return "resume <run-id> --approve <fingerprint>"
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return "resume <run-id> --resolve <operation-id>=<verdict> --note <text> --evidence <ref>"
	}
	switch projection.State {
	case domain.RunRunning:
		return "resume <run-id>"
	case domain.RunCreated, domain.RunCancelling:
		return "resume <run-id> --abort-run --reason <text>"
	}
	return ""
}
