// Package app 装配面向 CLI 的 Cairn 运行、恢复与诊断用例。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"runtime/debug"

	"github.com/fxbin/cairn/internal/buildinfo"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/telemetry"
	"github.com/fxbin/cairn/internal/tools"
)

// ProviderHealth 汇总 Provider 的非敏感运行前提：端点、模型与密钥环境变量的
// 存在性（只出现变量名，绝不出现值）。
type ProviderHealth struct {
	Protocol      string `json:"protocol"`
	Use           string `json:"use,omitempty"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	APIKeyEnv     string `json:"api_key_env"`
	APIKeyPresent bool   `json:"api_key_present"`
}

// SandboxHealth 汇总执行后端的只读探测结果；镜像可用性为 yes/no/unknown，
// 探测失败不改变 doctor 的完整性判定。HostConfinement 描述 OS 原生沙箱档位。
type SandboxHealth struct {
	Backend        string `json:"backend"`
	DockerImage    string `json:"docker_image,omitempty"`
	ImageAvailable string `json:"image_available,omitempty"`
	// HostConfined 表示 host 后端是否套 Seatbelt（macOS）。
	HostConfined bool `json:"host_confined"`
	// SeatbeltAvailable 表示平台 OS 沙箱（Seatbelt/bwrap）是否可用。
	SeatbeltAvailable bool `json:"seatbelt_available"` // alias of OS sandbox
}

// AdmissionHealth 汇总当前 Provider 绑定的准入模式和只读判定；Error 只使用
// 稳定、非敏感原因，不暴露本地路径、凭据或证据正文。
type AdmissionHealth struct {
	Mode     string `json:"mode"`
	Admitted bool   `json:"admitted"`
	Error    string `json:"error,omitempty"`
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
	BuildValid        bool                             `json:"build_valid"`
	BuildError        string                           `json:"build_error,omitempty"`
	WorkspaceHash     string                           `json:"workspace_hash"`
	Policy            PolicyHealth                     `json:"policy"`
	Resolution        []config.ResolutionEntry         `json:"resolution,omitempty"`
	Provider          ProviderHealth                   `json:"provider"`
	ProviderCard      *contract.ProviderCapabilityCard `json:"provider_card,omitempty"`
	ProviderCardError string                           `json:"provider_card_error,omitempty"`
	Admission         AdmissionHealth                  `json:"admission"`
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
	Mode  string `json:"mode,omitempty"`
	Write string `json:"write"`
	Exec  string `json:"exec"`
}

// TelemetryHealth 如实报告遥测的真实接线状态。Status 是记忆体系的只读
// 聚合快照（cairn memory telemetry）+ 召回聚合采集点状态：snapshot_available
// 表示只读聚合与召回采集可用；disabled 表示该面未接线。OTLP 是构建形态维度
// 的运行级导出状态：unavailable 表示 plain 无探针；disabled 表示 SDK 全局
// 关闭或 exporter 全部为 none；otlp 表示至少一个 OTLP exporter；enabled 表示
// 仅非 OTLP exporter 启用；config_error 表示配置不受注入 SDK 支持。端点与认证
// 只存在于环境，本投影永远不携带其值。
type TelemetryHealth struct {
	// Status 是记忆体系的只读聚合快照状态（既有语义，不变）。
	Status string `json:"status"`
	// OTLP 是 SDK 面（otelc 构建形态）的运行级导出状态（既有语义，不变）。
	OTLP string `json:"otlp,omitempty"`
	// Semantic 是语义面（Journal 事件投影）的接线状态：disabled 表示未启用；
	// sdk_disabled 表示被 OTel 全局关闭开关禁用；config_error 表示显式启用
	// 但连接事实不受支持；no_signal 表示接线就绪但两个信号都被关闭；
	// instrumented 表示接线就绪且至少一个信号会被发送——它只说明观测面
	// 就绪，绝不宣称已经成功采集。
	Semantic string `json:"semantic,omitempty"`
	// SemanticSignals 列出语义面实际会发送的信号（traces、metrics）。
	SemanticSignals string `json:"semantic_signals,omitempty"`
	// SemanticReason 是语义面状态的稳定原因取值，只描述原因类别与出错的
	// 变量归属，不含端点、请求头取值或本机路径。
	SemanticReason string `json:"semantic_reason,omitempty"`
	// SemanticExports 汇总最近一次运行记录的语义导出计数与记录时间；文件
	// 缺失时为空，表示还没有可对账的导出记录。
	SemanticExports *TelemetryExportRecord `json:"semantic_exports,omitempty"`
}

// Doctor 以只读模式验证工作区对应日志和制品引用的完整性，并返回环境可运行性
// 诊断报告；所有探测均为只读（环境变量只查存在性、Docker 只做本地镜像查询）。
// 新工作区没有 journal 时不报错，journal_state=absent 并继续给出 ready 判定。
func (service Service) Doctor(ctx context.Context, workspacePath, stateDir string, configuration config.Config) (DoctorReport, error) {
	validationErr := config.Validate(configuration)
	// 语义观测接线判定与运行路径同源：doctor 报告的状态与运行时是否导出
	// 永远一致，不会出现"doctor 说可用、运行却不导出"的分叉。
	semanticPlan := telemetrySemanticPlan(configuration)
	report := DoctorReport{
		ConfigValid:   validationErr == nil,
		Preset:        configuration.Preset,
		Profile:       configuration.Profile,
		Build:         buildIdentity(),
		WorkspaceHash: "",
		Policy:        PolicyHealth{Mode: configuration.Policy.Mode, Write: configuration.Policy.Write, Exec: configuration.Policy.Exec},
		Resolution:    configuration.Resolution,
		Provider:      ProviderHealth{Protocol: configuration.Provider.Protocol, Use: configuration.Provider.Use, BaseURL: configuration.Provider.BaseURL, Model: configuration.Provider.Model, APIKeyEnv: configuration.Provider.APIKeyEnv, APIKeyPresent: false},
		Admission:     AdmissionHealth{Mode: "disabled"},
		Sandbox: SandboxHealth{
			Backend:           configuration.Sandbox.Backend,
			DockerImage:       configuration.Sandbox.DockerImage,
			HostConfined:      tools.ExecBackend(configuration.Sandbox.Backend) != tools.ExecBackendDocker && tools.OSSandboxAvailable(),
			SeatbeltAvailable: tools.OSSandboxAvailable(),
		},
		Tools:             nil,
		VerificationCount: len(configuration.Verification.Commands),
		JournalState:      "absent",
		MemoryState:       "absent",
		Telemetry: TelemetryHealth{
			Status:          "disabled",
			OTLP:            buildinfo.TelemetryMode(),
			Semantic:        semanticPlan.Status,
			SemanticSignals: telemetrySemanticSignals(semanticPlan),
			SemanticReason:  semanticPlan.Reason,
		},
	}
	report.BuildRevision = report.Build.Revision
	if err := report.Build.Validate(); err != nil {
		report.BuildError = err.Error()
	} else {
		report.BuildValid = true
	}
	if validationErr != nil {
		return report, nil
	}
	report.ConfigFingerprint = configuration.Fingerprint()
	apiKey, credentialErr := providerCredential(workspacePath, configuration.Provider.APIKeyEnv)
	if credentialErr != nil {
		return report, credentialErr
	}
	report.Provider.APIKeyPresent = apiKey != ""
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
	// 导出计数只在语义面接线就绪时对账：被关闭或配置不合法时旧的计数
	// 不代表当前行为，宁可不报也不误报。
	if semanticPlan.Status == telemetry.ExportStatusInstrumented {
		report.Telemetry.SemanticExports = readTelemetryStats(stateDir, hash)
	}
	report.Admission, _ = checkAdmissionGate(stateDir, hash, configuration)
	if _, err := execToolFor(workspace, configuration); err != nil {
		return report, err
	}
	capabilities := toolCapabilities(workspace, configuration.Preset, configuration.Tools)
	if configuration.Skills != nil && configuration.Skills.Enabled {
		capabilities = append(capabilities, ToolCapability{Name: "skill", SideEffect: string(tools.SideEffectNone)})
	}
	if hasEnabledMCP(configuration.MCP) {
		capabilities = append(capabilities, ToolCapability{Name: "mcp_discover", SideEffect: string(tools.SideEffectProcess)}, ToolCapability{Name: "tool_search", SideEffect: string(tools.SideEffectNone)})
	}
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
			report.probeSandboxAndEvaluateReadiness(configuration)
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
	report.Telemetry = TelemetryHealth{
		Status:          telemetryStatusForMemory(memoryState),
		OTLP:            buildinfo.TelemetryMode(),
		Semantic:        semanticPlan.Status,
		SemanticSignals: telemetrySemanticSignals(semanticPlan),
		SemanticReason:  semanticPlan.Reason,
		SemanticExports: report.Telemetry.SemanticExports,
	}
	report.probeSandboxAndEvaluateReadiness(configuration)
	return report, nil
}

// probeSandboxAndEvaluateReadiness 在本地日志、制品与记忆完整性检查通过后执行
// Docker 只读探测，并据此汇总最终就绪状态。
func (report *DoctorReport) probeSandboxAndEvaluateReadiness(configuration config.Config) {
	if configuration.Sandbox.Backend == "docker" {
		report.Sandbox.ImageAvailable = probeDockerImage(configuration.Sandbox.DockerImage)
	}
	report.evaluateReadiness()
}

// evaluateReadiness 汇总总体就绪判定：配置与构建身份有效、能力卡可解析、
// Provider 前提在位、required 准入通过、Docker 后端镜像可用，且 journal
// 完整或首次运行前尚不存在。
func (report *DoctorReport) evaluateReadiness() {
	sandboxReady := report.Sandbox.Backend != "docker" || report.Sandbox.ImageAvailable == "yes"
	admissionReady := report.Admission.Mode != config.ProviderAdmissionRequired || report.Admission.Admitted
	report.Ready = report.ConfigValid && report.BuildValid && report.ProviderCardError == "" && report.Provider.BaseURL != "" && report.Provider.Model != "" && report.Provider.APIKeyPresent && admissionReady && sandboxReady && (report.JournalState == "ok" || report.JournalState == "absent")
}

// buildIdentity 返回实际二进制的非敏感身份。revision 缺失时保留空值、dirty
// 元数据缺失时标为 unknown；编译期构建形态及工具链事实原样保留，非法 ldflags
// 由 Validate 显式拒绝，禁止静默清洗后伪装成合法身份。
func buildIdentity() domain.BuildIdentity {
	if info, ok := debug.ReadBuildInfo(); ok {
		return buildIdentityFromSettings(info.Settings)
	}
	return buildIdentityFromSettings(nil)
}

// buildIdentityFromSettings 从 Go BuildInfo 或编译期注入中读取可公开的 VCS 设置
// 与构建形态并派生身份；otelc 使用 -buildvcs=false，因此 revision/dirty 由构建
// 脚本在模块改写前注入。Go BuildInfo 中存在合法值时优先采用，以保持 plain
// 构建行为；非法注入值不被修正，由 Validate 失败关闭。
func buildIdentityFromSettings(settings []debug.BuildSetting) domain.BuildIdentity {
	identity := domain.BuildIdentity{Revision: buildinfo.Revision, Dirty: buildinfo.Dirty, Flavor: buildinfo.Flavor, OtelcVersion: buildinfo.ToolchainVersion, InstrumentationDigest: buildinfo.InstrumentationDigest}
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

// currentBuildIdentity 返回已校验的当前构建身份；运行入口必须在创建 journal、
// 锁或制品前调用，确保非法编译期注入失败关闭且不留下持久化痕迹。
func currentBuildIdentity() (*domain.BuildIdentity, error) {
	identity := buildIdentity()
	if err := identity.Validate(); err != nil {
		return nil, fmt.Errorf("invalid current build identity: %w", err)
	}
	return &identity, nil
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
func toolCapabilities(workspace tools.Workspace, preset string, options config.ToolsConfig) []ToolCapability {
	registry, err := tools.NewRegistry(presetTools(workspace, tools.ExecCommand{Workspace: workspace}, preset, options)...)
	if err != nil {
		return nil
	}
	capabilities := make([]ToolCapability, 0, len(registry.Specs()))
	for _, spec := range registry.Specs() {
		capabilities = append(capabilities, ToolCapability{Name: spec.Name, SideEffect: string(registry.SideEffect(spec.Name))})
	}
	return capabilities
}

// presetTools 按 Preset 与 tools 开关返回注册表工具子集；read-only 只组合只读工具。
// 副作用工具必须传入"已完成配置装配"的实例（EnvAllow/Backend/Grace 等），
// 此处绝不自造裸工具——否则验证命令所需的环境变量会被拒、沙箱配置会被静默丢弃
// （2026-08-16 AB Gate 回归实证）。
func presetTools(workspace tools.Workspace, execTool tools.ExecCommand, preset string, options config.ToolsConfig) []tools.Tool {
	return presetToolsWithWeb(workspace, execTool, preset, options)
}

// presetToolsWithWeb 按 opt-in 决定是否暴露网络与会话工具。
func presetToolsWithWeb(workspace tools.Workspace, execTool tools.ExecCommand, preset string, options config.ToolsConfig) []tools.Tool {
	patchTool := tools.ApplyPatch{Workspace: workspace}
	available := []tools.Tool{
		tools.ListFiles{Workspace: workspace},
		tools.ReadFile{Workspace: workspace},
		tools.SearchText{Workspace: workspace},
		tools.Todo{Board: tools.NewTodoBoard()},
		tools.GitStatus{Workspace: workspace},
		tools.GitDiff{Workspace: workspace},
		tools.GitLog{Workspace: workspace},
		tools.GitCommit{Workspace: workspace},
		patchTool,
		execTool,
		tools.SendMessage{},
		tools.DelegateTask{},
	}
	// 网络工具 opt-in：与 dsh Web Search 设置一致，默认不暴露。
	if options.Web && config.PresetReadOnly != preset {
		available = append(available, tools.WebFetch{}, tools.WebSearch{})
	}
	// exec_session 默认禁用；显式启用且非 read-only 时注册 tmux 后端。
	if options.ExecSession && config.PresetReadOnly != preset {
		available = append(available, tools.ExecSession{Workspace: workspace, Backend: tools.NewTmuxSessionBackend()})
	}
	// run_tests / lsp_diagnostics 为开发辅助面，read-only 仍允许只读诊断。
	if options.RunTests && config.PresetReadOnly != preset {
		available = append(available, tools.RunTests{Workspace: workspace})
	}
	if options.Browser && config.PresetReadOnly != preset {
		available = append(available, tools.Browser{Driver: &tools.LazyBrowserDriver{}})
	}
	if preset == config.PresetReadOnly {
		readOnly := append([]tools.Tool(nil), available[:7]...)
		readOnly = append(readOnly, tools.MemoryRecall{})
		if options.LspDiagnostics {
			readOnly = append(readOnly, tools.LspDiagnostics{Workspace: workspace})
		}
		return readOnly
	}
	if options.LspDiagnostics {
		available = append(available, tools.LspDiagnostics{Workspace: workspace})
	}
	available = append(available, tools.MemoryRecall{})
	return available
}

// withoutDelegateTool 返回不含委派工具的工具切片；子运行不暴露委派面，
// 委派能力只属于根运行（与 Loop 侧 Delegator 绑定规则一致）。
func withoutDelegateTool(available []tools.Tool) []tools.Tool {
	filtered := make([]tools.Tool, 0, len(available))
	for _, tool := range available {
		if _, isDelegate := tool.(tools.DelegateTask); isDelegate {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
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

// SummarizeRun 从事件流与投影构建诊断摘要；completionTrusted 来自同一
// 冻结计划合同投影，事件 payload 在写入时已经过脱敏。
func SummarizeRun(events []domain.Event, projection domain.RunProjection, completionTrusted bool) RunSummary {
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
	summary.CompletionTrusted = completionTrusted
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
// 指引保持同一语义。对账提示复用 contract 的共用词表，避免各界面自造措辞。
func nextActionFor(projection domain.RunProjection) string {
	if projection.PendingApproval != nil {
		return "resume <run-id> --approve <fingerprint>"
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return contract.ReconcileActionHint
	}
	switch projection.State {
	case domain.RunRunning:
		return "resume <run-id>"
	case domain.RunCreated, domain.RunCancelling:
		return "resume <run-id> --abort-run --reason <text>"
	}
	return ""
}

// telemetryStatusForMemory 由记忆控制面状态推导遥测投影：库存在时只读聚
// 合快照与召回采集可用；库缺失或损坏时如实报告不可用。
func telemetryStatusForMemory(memoryState string) string {
	if memoryState == "ok" {
		return "snapshot_available"
	}
	return "disabled"
}
