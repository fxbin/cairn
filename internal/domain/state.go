package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RunState 表示一次 Agent 运行在回放状态机中的生命周期阶段。
type RunState string

const (
	RunCreated    RunState = "created"
	RunRunning    RunState = "running"
	RunCancelling RunState = "cancelling"
	RunCompleted  RunState = "completed"
	RunFailed     RunState = "failed"
	RunCancelled  RunState = "cancelled"
)

// RunProjection 是由事件序列重放得到的当前运行状态与恢复依据。
type RunProjection struct {
	ID                       RunID
	Task                     string
	Workspace                string
	WorkspaceHash            string
	ConfigFingerprint        string         `json:"config_fingerprint,omitempty"`
	Build                    *BuildIdentity `json:"build,omitempty"`
	SessionID                string
	PrevRunID                RunID
	ForkSource               *SessionForkSource
	CreatedAt                time.Time
	UpdatedAt                time.Time
	WorkspaceObservation     string
	State                    RunState
	LastSeq                  uint64
	CurrentTurn              *TurnID
	LastAssistantMessage     string
	VerificationSucceeded    bool
	LastVerificationSeq      uint64
	LastMutationSeq          uint64
	LastContextCompactSeq    uint64
	ContextCompactedThrough  uint64
	PendingApproval          *PendingApproval
	PendingOperation         *PendingOperation
	unfinishedVerification   *TurnID
	unfinishedVerificationID string
	operationFingerprints    map[OperationID]string
	reconciledFingerprints   map[string]struct{}
	operations               map[OperationID]*operationRecord
}

// ValidateDraft 在不改变当前投影的前提下校验一条待追加事件的 payload 与状态迁移。
// 它让调用方能够在写入不可变 Journal 之前拒绝非法事件，避免留下不可回放的日志。
func (projection RunProjection) ValidateDraft(draft EventDraft) error {
	event, err := NewEvent(draft, projection.LastSeq+1, GenesisHash)
	if err != nil {
		return err
	}
	if err := validatePayload(event.Type, event.Payload); err != nil {
		return fmt.Errorf("event payload: %w", err)
	}
	if event.Type == EventOperationStarted {
		var payload struct {
			SideEffectClass *string `json:"side_effect_class"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode operation_started draft: %w", err)
		}
		if payload.SideEffectClass == nil || !oneOf(*payload.SideEffectClass, "none", "workspace", "process") {
			return errors.New("operation_started draft requires a valid side_effect_class")
		}
	}
	if event.Type == EventRunCancelRequested {
		var payload struct {
			ExpectedTerminal *string `json:"expected_terminal"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode run_cancel_requested draft: %w", err)
		}
		if payload.ExpectedTerminal == nil || !oneOf(*payload.ExpectedTerminal, "failed", "cancelled") {
			return errors.New("run_cancel_requested draft requires a valid expected_terminal")
		}
	}
	candidate := projection.clone()
	return candidate.Apply(event)
}

// clone 复制投影及其内部可变映射，供写前校验模拟状态迁移使用。
func (projection RunProjection) clone() RunProjection {
	copyTurn := func(source *TurnID) *TurnID {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	copyApproval := func(source *PendingApproval) *PendingApproval {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	copyOperation := func(source *PendingOperation) *PendingOperation {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	copyBuild := func(source *BuildIdentity) *BuildIdentity {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	copyForkSource := func(source *SessionForkSource) *SessionForkSource {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	candidate := projection
	candidate.CurrentTurn = copyTurn(projection.CurrentTurn)
	candidate.PendingApproval = copyApproval(projection.PendingApproval)
	candidate.PendingOperation = copyOperation(projection.PendingOperation)
	candidate.unfinishedVerification = copyTurn(projection.unfinishedVerification)
	candidate.Build = copyBuild(projection.Build)
	candidate.ForkSource = copyForkSource(projection.ForkSource)
	if projection.operationFingerprints != nil {
		candidate.operationFingerprints = make(map[OperationID]string, len(projection.operationFingerprints))
		for id, fingerprint := range projection.operationFingerprints {
			candidate.operationFingerprints[id] = fingerprint
		}
	}
	if projection.reconciledFingerprints != nil {
		candidate.reconciledFingerprints = make(map[string]struct{}, len(projection.reconciledFingerprints))
		for fingerprint := range projection.reconciledFingerprints {
			candidate.reconciledFingerprints[fingerprint] = struct{}{}
		}
	}
	if projection.operations != nil {
		candidate.operations = make(map[OperationID]*operationRecord, len(projection.operations))
		for id, record := range projection.operations {
			copyRecord := *record
			candidate.operations[id] = &copyRecord
		}
	}
	return candidate
}

// operationState 记录单个工具操作在投议、授权与终态之间的内部迁移。
type operationState string

const (
	operationProposed        operationState = "proposed"
	operationAwaitingApprove operationState = "awaiting_approval"
	operationApprovalGranted operationState = "approval_granted"
	operationAuthorized      operationState = "authorized"
	operationDenied          operationState = "denied"
	operationStarted         operationState = "started"
	operationTerminal        operationState = "terminal"
)

// operationRecord 记录回放时验证操作合法性所需的最小内部事实。
type operationRecord struct {
	state              operationState
	toolResultAppended bool
	turnID             TurnID
	requiresToolResult bool
}

// PendingApproval 描述等待人工确认的副作用操作。
type PendingApproval struct {
	OperationID OperationID
	Fingerprint string
	Summary     string
}

// PendingOperation 描述已启动但尚未持久化终态的外部操作。
type PendingOperation struct {
	ID              OperationID
	ToolName        string
	SideEffectClass string
	MutationBarrier bool
	PlannedPath     string
	PlannedBefore   string
	PlannedAfter    string
}

// BuildIdentity 描述实际启动 Run 的二进制身份；它仅保存修订、三态 dirty 状态
// （unknown/true/false）、明确的 plain/otelc flavor、可选观测工具链事实与这些
// 事实的确定性摘要，不承诺可选探针或构建标签存在。OtelcVersion 与
// InstrumentationDigest 仅在 otelc flavor 下非空，旧事件缺省合法。
type BuildIdentity struct {
	Revision    string `json:"revision"`
	Dirty       string `json:"dirty"`
	Flavor      string `json:"flavor"`
	Fingerprint string `json:"fingerprint"`
	// 可选观测工具链事实：otelc 构建注入的工具链版本与插桩集合摘要；
	// plain 构建恒为空且必须为空，防止构建身份证据自相矛盾。
	OtelcVersion          string `json:"otelc_version,omitempty"`
	InstrumentationDigest string `json:"instrumentation_digest,omitempty"`
}

// buildIdentityFingerprintPrefix 是构建身份摘要的版本化域前缀。
const buildIdentityFingerprintPrefix = "cairn-build-identity-v1"

// ComputeFingerprint 为身份事实计算稳定的版本化摘要：覆盖可公开的修订、
// dirty、flavor 与可选工具链事实，不读取环境变量、配置或任何凭据；可选字段
// 缺省不进入编码，plain 身份摘要与历史二进制逐字节一致。派生方在装配时把
// 结果写回 Fingerprint 字段。方法名刻意区别于同名字段。
func (identity BuildIdentity) ComputeFingerprint() string {
	view := struct {
		Revision              string `json:"revision"`
		Dirty                 string `json:"dirty"`
		Flavor                string `json:"flavor"`
		OtelcVersion          string `json:"otelc_version,omitempty"`
		InstrumentationDigest string `json:"instrumentation_digest,omitempty"`
	}{Revision: identity.Revision, Dirty: identity.Dirty, Flavor: identity.Flavor,
		OtelcVersion: identity.OtelcVersion, InstrumentationDigest: identity.InstrumentationDigest}
	encoded, err := json.Marshal(view)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append([]byte(buildIdentityFingerprintPrefix+"\x00"), encoded...))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Validate 拒绝无法解释为当前二进制形态或摘要不自洽的新增 Run 身份；nil 是
// 旧 run 的合法兼容形式。Revision 可以为空、dirty 必须明确为
// unknown/true/false；工具链事实只在 otelc flavor 下允许出现，且 otelc 必须
// 同时携带版本与插桩摘要。Fingerprint 必须由其余身份事实确定性复算得到。
func (identity *BuildIdentity) Validate() error {
	if identity == nil {
		return nil
	}
	if identity.Flavor != "plain" && identity.Flavor != "otelc" {
		return fmt.Errorf("run build flavor %q is unsupported", identity.Flavor)
	}
	if identity.Dirty != "unknown" && identity.Dirty != "true" && identity.Dirty != "false" {
		return fmt.Errorf("run build dirty status %q is invalid", identity.Dirty)
	}
	if identity.Fingerprint == "" {
		return errors.New("run build fingerprint is required")
	}
	if identity.Flavor == "plain" && (identity.OtelcVersion != "" || identity.InstrumentationDigest != "") {
		return errors.New("plain build identity must not carry optional toolchain facts")
	}
	if identity.Flavor == "otelc" && (identity.OtelcVersion == "" || identity.InstrumentationDigest == "") {
		return errors.New("otelc build identity requires toolchain version and instrumentation digest")
	}
	if identity.Flavor == "otelc" {
		const digestPrefix = "sha256:"
		digest := identity.InstrumentationDigest
		if len(digest) != len(digestPrefix)+sha256.Size*2 || digest[:len(digestPrefix)] != digestPrefix {
			return errors.New("otelc instrumentation digest must be a sha256 digest")
		}
		if _, err := hex.DecodeString(digest[len(digestPrefix):]); err != nil {
			return errors.New("otelc instrumentation digest must be a sha256 digest")
		}
	}
	if expected := identity.ComputeFingerprint(); identity.Fingerprint != expected {
		return fmt.Errorf("run build fingerprint does not match identity facts: got %q want %q", identity.Fingerprint, expected)
	}
	return nil
}

// SessionForkSource 是跨会话分支根对历史终态 run 的来源锚点。它只允许出现在
// 新会话根运行的 run_created 上，与同 session 的 prev_run_id 互斥；锚点绑定
// 来源 run 标识、事件边界与工作区身份，创建前必须重新校验一致。
type SessionForkSource struct {
	// RunID 是被分支的历史终态 run。
	RunID RunID `json:"run_id"`
	// WorkspaceHash 是来源 run 的工作区身份摘要。
	WorkspaceHash string `json:"workspace_hash"`
	// LastSeq 是来源 run 末事件序号，界定事件边界。
	LastSeq uint64 `json:"last_seq"`
	// LastEventHash 是来源 run 末事件哈希链摘要，使用 sha256: 前缀规范形式。
	LastEventHash string `json:"last_event_hash"`
}

// RunCreatedPayload 是 run_created 事件的可回放业务字段。ConfigFingerprint 与
// Build 均为可选字段，使旧 Run 在升级后仍可完整回放。
type RunCreatedPayload struct {
	Task              string         `json:"task"`
	Workspace         string         `json:"workspace"`
	ConfigFingerprint string         `json:"config_fingerprint,omitempty"`
	Build             *BuildIdentity `json:"build,omitempty"`
	// 会话链关系为可选字段（ADR-0018）：session 是 run 链的投影，旧事件
	// 无字段合法，replay 不引入新规则；prior_context_ref 指向前情卡制品，
	// resume 按其回放注入会话前情。
	SessionID       string          `json:"session_id,omitempty"`
	PrevRunID       RunID           `json:"prev_run_id,omitempty"`
	PriorContextRef json.RawMessage `json:"prior_context_ref,omitempty"`
	// ProjectInstructionsRef 绑定本次运行的 AGENTS.md 快照；缺省表示旧运行不加载文件规约。
	ProjectInstructionsRef json.RawMessage `json:"project_instructions_ref,omitempty"`
	// ForkSource 绑定新会话分支根指向的历史终态 run；与 PrevRunID 互斥，
	// 只允许出现在新 session 的根运行上。
	ForkSource *SessionForkSource `json:"fork_source,omitempty"`
}

// TurnClosedPayload 表示一次推理轮次的关闭结果。
type TurnClosedPayload struct {
	Outcome             string `json:"outcome"`
	OperationSignature  string `json:"operation_signature,omitempty"`
	ProgressFingerprint string `json:"progress_fingerprint,omitempty"`
}

// AssistantMessagePayload 保存模型输出给用户的文本。
type AssistantMessagePayload struct {
	Text string `json:"text"`
}

// VerificationFinishedPayload 记录一次验证是否通过。
type VerificationFinishedPayload struct {
	VerificationID    string `json:"verification_id,omitempty"`
	OK                bool   `json:"ok"`
	ResultFingerprint string `json:"result_fingerprint,omitempty"`
}

// ContextCompactedPayload 保存一次可重放的对话检查点及其压缩范围。V1.3 起
// 追加可选证据字段（ADR-0007）：摘要格式版本、触发原因、估算器身份、最近
// 实际 Provider 输入计数、被替代与被完整保留的 turn 清单；旧事件缺省合法。
// retained_turn_ids 不带 omitempty——v1 摘要格式不保留任何完整轮次，该空
// 清单是显式设计事实，必须随新事件写入。
type ContextCompactedPayload struct {
	ThroughSeq               uint64   `json:"through_seq"`
	Summary                  string   `json:"summary"`
	EstimatedTokensBefore    int      `json:"estimated_units_before"`
	EstimatedTokensAfter     int      `json:"estimated_units_after"`
	Strategy                 string   `json:"strategy"`
	SummaryVersion           int      `json:"summary_version,omitempty"`
	Trigger                  string   `json:"trigger,omitempty"`
	UnitsSource              string   `json:"units_source,omitempty"`
	LastProviderPromptTokens int64    `json:"last_provider_prompt_tokens,omitempty"`
	CompactedTurnIDs         []string `json:"compacted_turn_ids,omitempty"`
	RetainedTurnIDs          []string `json:"retained_turn_ids"`
}

// RunCompletedPayload 保存已完成运行的最终回复。
type RunCompletedPayload struct {
	FinalResponse string `json:"final_response"`
}

// Replay 先校验哈希链，再按顺序应用事件得到运行投影。
func Replay(events []Event) (RunProjection, error) {
	if err := VerifyChain(events); err != nil {
		return RunProjection{}, err
	}
	var projection RunProjection
	for _, event := range events {
		if err := projection.Apply(event); err != nil {
			return RunProjection{}, err
		}
	}
	return projection, nil
}

// Apply 将单个合法事件应用到投影，并拒绝非法状态迁移。
func (projection *RunProjection) Apply(event Event) error {
	if event.Seq != projection.LastSeq+1 {
		return fmt.Errorf("apply event sequence %d after %d", event.Seq, projection.LastSeq)
	}
	if projection.ID != "" && event.RunID != projection.ID {
		return fmt.Errorf("event run id %q does not match projection run id %q", event.RunID, projection.ID)
	}
	if projection.ID != "" && event.WorkspaceHash != projection.WorkspaceHash {
		return fmt.Errorf("event workspace hash %q does not match projection workspace hash %q", event.WorkspaceHash, projection.WorkspaceHash)
	}
	switch event.Type {
	case EventRunCreated:
		if projection.ID != "" || event.Seq != 1 {
			return fmt.Errorf("run_created must be the first event")
		}
		var payload RunCreatedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode run_created payload: %w", err)
		}
		if err := payload.Build.Validate(); err != nil {
			return err
		}
		projection.ID = event.RunID
		projection.Task = payload.Task
		projection.Workspace = payload.Workspace
		projection.WorkspaceHash = event.WorkspaceHash
		projection.ConfigFingerprint = payload.ConfigFingerprint
		projection.SessionID = payload.SessionID
		projection.PrevRunID = payload.PrevRunID
		if payload.ForkSource != nil {
			source := *payload.ForkSource
			projection.ForkSource = &source
		}
		projection.CreatedAt = event.OccurredAt
		if payload.Build != nil {
			build := *payload.Build
			projection.Build = &build
		}
		projection.State = RunCreated
	case EventRunStarted:
		if projection.State != RunCreated {
			return fmt.Errorf("run_started from %s", projection.State)
		}
		projection.State = RunRunning
	case EventRunResumed:
		if projection.State != RunRunning || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("run_resumed from %s", projection.State)
		}
		var payload struct {
			Invocation           string `json:"invocation"`
			FromSeq              uint64 `json:"from_seq"`
			WorkspaceObservation string `json:"workspace_observation"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode run_resumed payload: %w", err)
		}
		if payload.Invocation == "" || payload.WorkspaceObservation == "" || payload.FromSeq >= event.Seq {
			return errors.New("run_resumed payload is incomplete")
		}
		projection.WorkspaceObservation = payload.WorkspaceObservation
	case EventTurnOpened:
		if projection.State != RunRunning || event.TurnID == nil || projection.CurrentTurn != nil {
			return fmt.Errorf("turn_opened is not legal for run state %s", projection.State)
		}
		turnID := *event.TurnID
		projection.CurrentTurn = &turnID
	case EventTurnClosed:
		if (projection.State != RunRunning && projection.State != RunCancelling) || event.TurnID == nil || projection.CurrentTurn == nil || *event.TurnID != *projection.CurrentTurn || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("turn_closed does not match current turn")
		}
		var payload TurnClosedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode turn_closed payload: %w", err)
		}
		if payload.Outcome != "continue" && payload.Outcome != "final" && payload.Outcome != "failed" && payload.Outcome != "cancelled" {
			return fmt.Errorf("turn_closed has invalid outcome %q", payload.Outcome)
		}
		if payload.Outcome == "continue" || payload.Outcome == "final" {
			for operationID, record := range projection.operations {
				if record.turnID == *event.TurnID && record.requiresToolResult && record.state == operationTerminal && !record.toolResultAppended {
					return fmt.Errorf("turn_closed leaves tool result for operation %s unappended", operationID)
				}
			}
		}
		projection.CurrentTurn = nil
	case EventToolCallProposed, EventVerificationCommandProposed:
		if projection.State != RunRunning || event.TurnID == nil || projection.CurrentTurn == nil || *event.TurnID != *projection.CurrentTurn || event.OperationID == nil {
			return fmt.Errorf("%s requires the current running turn and operation id", event.Type)
		}
		if projection.operations == nil {
			projection.operations = make(map[OperationID]*operationRecord)
		}
		if _, exists := projection.operations[*event.OperationID]; exists {
			return fmt.Errorf("operation %s was proposed more than once", *event.OperationID)
		}
		projection.operations[*event.OperationID] = &operationRecord{state: operationProposed, turnID: *event.TurnID, requiresToolResult: event.Type == EventToolCallProposed}
	case EventAssistantMessageAppended:
		if projection.State != RunRunning || event.TurnID == nil || projection.CurrentTurn == nil || *event.TurnID != *projection.CurrentTurn {
			return fmt.Errorf("assistant_message_appended requires the current running turn")
		}
		var payload AssistantMessagePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode assistant message: %w", err)
		}
		projection.LastAssistantMessage = payload.Text
	case EventVerificationFinished:
		var payload VerificationFinishedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode verification result: %w", err)
		}
		matchesVerification := projection.unfinishedVerification == nil ||
			(projection.unfinishedVerificationID == "" || payload.VerificationID == "" || payload.VerificationID == projection.unfinishedVerificationID)
		activeTurn := event.TurnID != nil && projection.CurrentTurn != nil &&
			*event.TurnID == *projection.CurrentTurn && matchesVerification
		closedUnfinishedTurn := event.TurnID != nil && projection.CurrentTurn == nil &&
			projection.unfinishedVerification != nil && *event.TurnID == *projection.unfinishedVerification &&
			matchesVerification
		if (projection.State != RunRunning && projection.State != RunCancelling) || (!activeTurn && !closedUnfinishedTurn) {
			return fmt.Errorf("verification_finished requires the current running turn or its unfinished closed verification")
		}
		projection.VerificationSucceeded = payload.OK
		projection.LastVerificationSeq = event.Seq
		projection.unfinishedVerification = nil
		projection.unfinishedVerificationID = ""
	case EventContextCompacted:
		if projection.State != RunRunning || projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("context_compacted is not legal for run state %s", projection.State)
		}
		var payload ContextCompactedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode context compacted payload: %w", err)
		}
		if payload.ThroughSeq == 0 || payload.ThroughSeq >= event.Seq || payload.Summary == "" || payload.EstimatedTokensBefore < 1 || payload.EstimatedTokensAfter < 1 || payload.EstimatedTokensAfter > payload.EstimatedTokensBefore || payload.Strategy == "" {
			return errors.New("context_compacted payload is incomplete")
		}
		// 证据字段"出现即必须合法"：版本不超过当前摘要格式版本，枚举取值
		// 受限，计数非负；缺省（旧事件）不参与校验。
		if payload.SummaryVersion < 0 || payload.SummaryVersion > ContextSummaryVersion {
			return fmt.Errorf("context_compacted summary_version %d is unsupported", payload.SummaryVersion)
		}
		if payload.Trigger != "" && payload.Trigger != ContextCompactionTriggerPromptEstimate {
			return fmt.Errorf("context_compacted trigger %q is unsupported", payload.Trigger)
		}
		if payload.UnitsSource != "" && payload.UnitsSource != ContextUnitsSourceEstimateBytesDiv2 {
			return fmt.Errorf("context_compacted units_source %q is unsupported", payload.UnitsSource)
		}
		if payload.LastProviderPromptTokens < 0 {
			return errors.New("context_compacted last_provider_prompt_tokens must not be negative")
		}
		// v1 摘要格式不保留任何完整轮次；声明非空 retained 清单即是矛盾证据。
		if payload.SummaryVersion == ContextSummaryVersion && len(payload.RetainedTurnIDs) != 0 {
			return errors.New("context_compacted summary v1 must not retain full turns")
		}
		projection.LastContextCompactSeq = event.Seq
		projection.ContextCompactedThrough = payload.ThroughSeq
	case EventOperationStarted:
		if projection.State != RunRunning || projection.PendingOperation != nil {
			return fmt.Errorf("operation_started is not legal for run state %s", projection.State)
		}
		if event.OperationID == nil {
			return fmt.Errorf("operation_started requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationAuthorized {
			return fmt.Errorf("operation_started requires an authorized proposal")
		}
		var payload struct {
			ToolName        string `json:"tool_name"`
			SideEffectClass string `json:"side_effect_class"`
			MutationBarrier bool   `json:"mutation_barrier"`
			PlannedPath     string `json:"planned_path"`
			PlannedBefore   string `json:"planned_before"`
			PlannedAfter    string `json:"planned_after"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode operation start: %w", err)
		}
		if payload.SideEffectClass == "workspace" || (payload.SideEffectClass == "process" && payload.MutationBarrier) {
			projection.LastMutationSeq = event.Seq
		}
		projection.PendingOperation = &PendingOperation{ID: *event.OperationID, ToolName: payload.ToolName, SideEffectClass: payload.SideEffectClass, MutationBarrier: payload.MutationBarrier, PlannedPath: payload.PlannedPath, PlannedBefore: payload.PlannedBefore, PlannedAfter: payload.PlannedAfter}
		record.state = operationStarted
	case EventPolicyDecided:
		if event.OperationID == nil {
			return fmt.Errorf("policy_decided requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists {
			return fmt.Errorf("policy_decided requires a proposed operation")
		}
		var payload struct {
			Effect      string `json:"effect"`
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode policy decision: %w", err)
		}
		if payload.Fingerprint != "" {
			if projection.operationFingerprints == nil {
				projection.operationFingerprints = make(map[OperationID]string)
			}
			projection.operationFingerprints[*event.OperationID] = payload.Fingerprint
		}
		switch payload.Effect {
		case "allow":
			if record.state != operationProposed && record.state != operationApprovalGranted {
				return fmt.Errorf("policy allow requires a proposed or approved operation")
			}
			record.state = operationAuthorized
		case "ask":
			if record.state != operationProposed {
				return fmt.Errorf("policy ask requires a proposed operation")
			}
			record.state = operationAwaitingApprove
		case "deny":
			if record.state != operationProposed {
				return fmt.Errorf("policy deny requires a proposed operation")
			}
			record.state = operationDenied
		default:
			return fmt.Errorf("policy_decided has invalid effect %q", payload.Effect)
		}
	case EventOperationSucceeded, EventOperationFailed, EventOperationCancelled:
		if event.OperationID == nil {
			return fmt.Errorf("operation terminal event requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record == nil {
			return fmt.Errorf("%s requires a proposed operation", event.Type)
		}
		failedBeforeStart := event.Type == EventOperationFailed &&
			(record.state == operationDenied || record.state == operationApprovalGranted || record.state == operationAuthorized)
		if record.state != operationStarted && !failedBeforeStart {
			return fmt.Errorf("%s requires a started operation or a pre-start failure decision", event.Type)
		}
		record.state = operationTerminal
		if projection.PendingOperation != nil && projection.PendingOperation.ID == *event.OperationID {
			projection.PendingOperation = nil
		}
	case EventOperationReconciled:
		if event.OperationID == nil || projection.PendingOperation == nil || projection.PendingOperation.ID != *event.OperationID {
			return fmt.Errorf("operation_reconciled requires the pending operation")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationStarted {
			return fmt.Errorf("operation_reconciled requires a started operation")
		}
		if fingerprint := projection.operationFingerprints[*event.OperationID]; fingerprint != "" {
			if projection.reconciledFingerprints == nil {
				projection.reconciledFingerprints = make(map[string]struct{})
			}
			projection.reconciledFingerprints[fingerprint] = struct{}{}
		}
		projection.PendingOperation = nil
		record.state = operationTerminal
	case EventApprovalRequested:
		if projection.State != RunRunning || projection.PendingApproval != nil {
			return fmt.Errorf("approval_requested is not legal for run state %s", projection.State)
		}
		if event.OperationID == nil {
			return fmt.Errorf("approval_requested requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationAwaitingApprove {
			return fmt.Errorf("approval_requested requires an ask decision")
		}
		var payload struct {
			Fingerprint string `json:"fingerprint"`
			Summary     string `json:"summary"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode approval request: %w", err)
		}
		projection.PendingApproval = &PendingApproval{OperationID: *event.OperationID, Fingerprint: payload.Fingerprint, Summary: payload.Summary}
	case EventApprovalGranted, EventApprovalDenied:
		if event.OperationID == nil || projection.PendingApproval == nil || projection.PendingApproval.OperationID != *event.OperationID {
			return fmt.Errorf("%s does not match a pending approval", event.Type)
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationAwaitingApprove {
			return fmt.Errorf("%s requires an operation waiting for approval", event.Type)
		}
		var payload struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode approval result: %w", err)
		}
		if payload.Fingerprint == "" || payload.Fingerprint != projection.PendingApproval.Fingerprint {
			return fmt.Errorf("approval fingerprint does not match pending operation")
		}
		projection.PendingApproval = nil
		if event.Type == EventApprovalGranted {
			record.state = operationApprovalGranted
		} else {
			record.state = operationDenied
		}
	case EventToolResultAppended:
		if event.OperationID == nil {
			return fmt.Errorf("tool_result_appended requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationTerminal || record.toolResultAppended {
			return fmt.Errorf("tool_result_appended requires one terminal operation result")
		}
		record.toolResultAppended = true
	case EventRunCancelRequested:
		if projection.State != RunCreated && projection.State != RunRunning {
			return fmt.Errorf("run_cancel_requested from %s", projection.State)
		}
		projection.State = RunCancelling
	case EventRunCompleted:
		// ADR-0017：完成的结构前置是"验证已收束"——验证事件存在且覆盖最后
		// 变更；验证是否成功不影响完成合法性，完成可信等级由
		// contract.CompletionTrusted 依据 VerificationSucceeded 分离判定。
		if projection.State != RunRunning || projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil || projection.LastVerificationSeq == 0 || (projection.LastMutationSeq != 0 && projection.LastVerificationSeq <= projection.LastMutationSeq) {
			return fmt.Errorf("run_completed without a closed verification")
		}
		projection.State = RunCompleted
	case EventRunFailed:
		// 终态事件只允许一次：completed/cancelled 为不相容终态，failed 自身
		// 也不得重复进入；created/running/cancelling 是合法失败起点
		//（Abort 可中止未启动的 run，预算耗尽先经 cancel_requested 进入 cancelling）。
		if projection.State == RunCompleted || projection.State == RunCancelled || projection.State == RunFailed || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("run_failed from terminal state %s", projection.State)
		}
		projection.State = RunFailed
	case EventRunCancelled:
		if projection.State != RunCancelling || projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("run_cancelled from terminal state %s", projection.State)
		}
		projection.State = RunCancelled
	case EventModelRequestStarted, EventModelResponseFinished, EventModelRequestFailed:
		// 模型遥测事件只补充审计证据，不改变投影状态；负载合法性由写入路径的
		// 封闭 payload schema 与哈希链保证，恢复逻辑直接按事件流读取。
	case EventVerificationStarted:
		// verification_started 的观察摘要是最近一次 verifier 视图；它不参与终态
		// 判定，却让 inspect 能解释验证时看到的工作区而无需重新扫描或写入状态。
		var payload struct {
			VerificationID       string `json:"verification_id"`
			WorkspaceObservation string `json:"workspace_observation"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode verification start: %w", err)
		}
		if event.TurnID != nil {
			turnID := *event.TurnID
			projection.unfinishedVerification = &turnID
			projection.unfinishedVerificationID = payload.VerificationID
		}
		if payload.WorkspaceObservation != "" {
			projection.WorkspaceObservation = payload.WorkspaceObservation
		}
	case EventVerificationFeedbackAppended:
		// 验证反馈仅补充模型上下文；终态判定只依赖 verification_finished。
	case EventSteeringInputSubmitted, EventSteeringInputConsumed:
		// 安全点 steering 输入/消费只补充审计证据与模型上下文，不改变运行终态；
		// 幂等标识与内容引用由 payload schema 保证，恢复按事件流重放。
		if projection.State != RunRunning {
			return fmt.Errorf("%s is not legal for run state %s", event.Type, projection.State)
		}
	case EventQueueInputSubmitted, EventQueueInputConsumed, EventQueueInputCancelled:
		// 队列事件属于 workspace 级 queue 命名空间，不得混入 run 链。
		return fmt.Errorf("%s must not appear in a run event chain", event.Type)
	default:
		return fmt.Errorf("unhandled event type %q", event.Type)
	}
	projection.LastSeq = event.Seq
	projection.UpdatedAt = event.OccurredAt
	return nil
}

// Terminal 报告运行是否已经进入不可继续的终态。
func (projection RunProjection) Terminal() bool {
	return projection.State == RunCompleted || projection.State == RunFailed || projection.State == RunCancelled
}

// Cancelling 报告运行是否处于收尾取消中：已请求终止但仍有未关闭轮次或待决操作。
func (projection RunProjection) Cancelling() bool {
	return projection.State == RunCancelling
}

// RequiresFreshApproval 在同一指纹曾因不确定副作用被对账后返回 true；
// 回放历史绝不能再次授予执行权限。
func (projection RunProjection) RequiresFreshApproval(fingerprint string) bool {
	_, exists := projection.reconciledFingerprints[fingerprint]
	return fingerprint != "" && exists
}

// Phase 返回对外可见的恢复阶段；悬而未决的副作用操作不会在回放后被静默重试。
func (projection RunProjection) Phase() string {
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return "waiting_reconcile"
	}
	if projection.PendingApproval != nil {
		return "waiting_approval"
	}
	return string(projection.State)
}
