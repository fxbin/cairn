// taskcontext.go 定义任务上下文的只读投影 TaskContextV1：从任务库、工作流链、Journal
// 与制品库的既有事实投影出带来源、版本锚点与可信状态的统一上下文，供新会话安全接手
// 持久任务。投影是纯函数：不读取存储，不缓存，不落库；它不授予权限，也不决定运行终态，
// 也不携带任何模型输出或工具结果正文。
//
// [INPUT]: 应用层以只读方式加载并各自完成完整性检查的事实（TaskContextInput），每个来源以稳定原因码报告失败。
// [OUTPUT]: TaskContextV1 及其规范化摘要；交接摘要文本。
// [POS]: 合同层纯数据与纯函数；不依赖存储、应用层或文件系统。
// [PROTOCOL]: Update this header on change, then check AGENTS.md
//
// Author: fxbin
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

const (
	// TaskContextSchemaVersion 是任务上下文投影的结构版本；字段增删或语义变化时递增。
	TaskContextSchemaVersion = 1
	// TaskContextDigestPrefix 是任务上下文摘要的域前缀。
	TaskContextDigestPrefix = "cairn-task-context-v1"
	// TaskHandoffMaxBytes 是交接摘要的体积上限；超限时省略列表条目并显式标出省略数量。
	TaskHandoffMaxBytes = 8192
	// TaskHandoffPreviewRunes 是目标在交接摘要里保留的字符数上限。
	TaskHandoffPreviewRunes = 512
	// TaskHandoffUnsettledPreview 是交接摘要里未了结操作标识保留的条数上限；这一行是必需
	// 内容，不设上限就可能把必需行整体顶过体积上限。超出部分只给数量。
	TaskHandoffUnsettledPreview = 20
	// TaskHandoffHashRunes 是摘要与哈希在交接摘要里保留的十六进制字符数。
	TaskHandoffHashRunes = 16
	// TaskHandoffDisclaimer 是交接摘要末尾的固定说明：记录的审批不授予任何权限。
	TaskHandoffDisclaimer = "NOTE: approvals recorded here grant no permissions in the receiving session."
	// TaskHandoffHeading 是交接摘要交给模型时的首行说明：它由任务上下文的只读投影生成，
	// 不是本会话的对话历史；unknown 与 blocked 是显式标记，不得当作已完成或已通过。
	TaskHandoffHeading = "【任务交接摘要】以下内容由任务上下文的只读投影生成，供接手该任务时参考；它不是本会话的对话历史。unknown 与 blocked 是显式标记，不得当作已完成或已通过。\n"
	// TaskDecisionAccept 是显式验收在决定列表里的种类。
	TaskDecisionAccept = "accept"
)

// TaskFactStatus 是任务上下文里单个字段的可信状态。
type TaskFactStatus string

const (
	// TaskFactKnown 表示事实已从通过完整性检查的来源读到，且锚点相互一致。
	TaskFactKnown TaskFactStatus = "known"
	// TaskFactUnknown 表示尚无事实，例如任务未绑定工作流或验证报告尚未生成。
	TaskFactUnknown TaskFactStatus = "unknown"
	// TaskFactBlocked 表示来源存在但不可信，例如哈希链断裂、摘要不符或版本漂移。
	TaskFactBlocked TaskFactStatus = "blocked"
)

// TaskAnchorKind 是版本锚点的类型；封闭集合，新增取值必须同时新增一致性规则与测试。
type TaskAnchorKind string

const (
	// TaskAnchorTaskID 锚定任务标识。
	TaskAnchorTaskID TaskAnchorKind = "task_id"
	// TaskAnchorTaskRevision 锚定任务修订号。
	TaskAnchorTaskRevision TaskAnchorKind = "task_revision"
	// TaskAnchorAcceptanceDigest 锚定冻结的验收合同摘要。
	TaskAnchorAcceptanceDigest TaskAnchorKind = "acceptance_digest"
	// TaskAnchorWorkflowSequence 锚定工作流链的事件序号。
	TaskAnchorWorkflowSequence TaskAnchorKind = "workflow_sequence"
	// TaskAnchorConfigFingerprint 锚定配置指纹。
	TaskAnchorConfigFingerprint TaskAnchorKind = "config_fingerprint"
	// TaskAnchorScopeDigest 锚定验证范围摘要。
	TaskAnchorScopeDigest TaskAnchorKind = "scope_digest"
	// TaskAnchorArtifactSHA256 锚定被引用制品的清单。
	TaskAnchorArtifactSHA256 TaskAnchorKind = "artifact_sha256"
)

// TaskFactSource 是事实的来源；封闭集合。
type TaskFactSource string

const (
	// TaskFactSourceTask 表示任务库。
	TaskFactSourceTask TaskFactSource = "task"
	// TaskFactSourceWorkflow 表示工作流事件链。
	TaskFactSourceWorkflow TaskFactSource = "workflow"
	// TaskFactSourceJournal 表示运行 Journal。
	TaskFactSourceJournal TaskFactSource = "journal"
	// TaskFactSourceArtifact 表示内容寻址制品库。
	TaskFactSourceArtifact TaskFactSource = "artifact"
	// TaskFactSourceReference 表示应用层的只读参考视图。
	TaskFactSourceReference TaskFactSource = "reference"
)

// TaskContextReason 是字段落 unknown 或 blocked 时的稳定原因码；封闭集合，只含类别，
// 不含错误原文与本地路径。
type TaskContextReason string

const (
	// TaskReasonNoStructuredAcceptance 表示任务创建时没有结构化验收合同。
	TaskReasonNoStructuredAcceptance TaskContextReason = "no_structured_acceptance"
	// TaskReasonNotBoundToWorkflow 表示任务没有绑定工作流。
	TaskReasonNotBoundToWorkflow TaskContextReason = "not_bound_to_workflow"
	// TaskReasonBindingNotAttached 表示任务已绑定工作流但实例尚未挂接。
	TaskReasonBindingNotAttached TaskContextReason = "binding_not_attached"
	// TaskReasonReportNotGenerated 表示验证报告尚未生成，或审查步骤尚未到达。
	TaskReasonReportNotGenerated TaskContextReason = "report_not_generated"

	// TaskReasonAcceptanceContractMismatch 表示验收合同摘要与标准正文不符。
	TaskReasonAcceptanceContractMismatch TaskContextReason = "acceptance_contract_mismatch"
	// TaskReasonWorkflowChainInvalid 表示工作流事件链校验失败。
	TaskReasonWorkflowChainInvalid TaskContextReason = "workflow_chain_invalid"
	// TaskReasonWorkflowMissing 表示已挂接的工作流实例找不到。
	TaskReasonWorkflowMissing TaskContextReason = "workflow_missing"
	// TaskReasonBindingMismatch 表示绑定不一致：任务绑定与工作流权威投影的身份或指纹对不上，
	// 或者被引用的运行与工作流尝试、手工分配记录的绑定对不上。
	TaskReasonBindingMismatch TaskContextReason = "binding_mismatch"
	// TaskReasonDefinitionMismatch 表示工作流定义与任务的验收合同或授权不一致。
	TaskReasonDefinitionMismatch TaskContextReason = "definition_mismatch"
	// TaskReasonJournalUnavailable 表示运行 Journal 无法以只读方式打开。
	TaskReasonJournalUnavailable TaskContextReason = "journal_unavailable"
	// TaskReasonJournalChainInvalid 表示某个运行的 Journal 哈希链校验失败。
	TaskReasonJournalChainInvalid TaskContextReason = "journal_chain_invalid"
	// TaskReasonRunMissing 表示被引用的运行在 Journal 里不存在。
	TaskReasonRunMissing TaskContextReason = "run_missing"
	// TaskReasonTerminalAnchorMismatch 表示工作流尝试声明的终态锚点与 Journal 链头不符。
	TaskReasonTerminalAnchorMismatch TaskContextReason = "terminal_anchor_mismatch"
	// TaskReasonArtifactMissing 表示被引用的制品缺失。
	TaskReasonArtifactMissing TaskContextReason = "artifact_missing"
	// TaskReasonArtifactTampered 表示被引用的制品内容与摘要不符。
	TaskReasonArtifactTampered TaskContextReason = "artifact_tampered"
	// TaskReasonCompletionAnchorMismatch 表示完成记录与验收摘要或运行链头不符。
	TaskReasonCompletionAnchorMismatch TaskContextReason = "completion_anchor_mismatch"
	// TaskReasonReferenceViewInvalid 表示应用层参考视图无法生成或核验失败。
	TaskReasonReferenceViewInvalid TaskContextReason = "reference_view_invalid"
	// TaskReasonReferenceViewStale 表示参考视图落后于权威投影，需要调用方重读。
	TaskReasonReferenceViewStale TaskContextReason = "reference_view_stale"
	// TaskReasonReportRunOutOfScope 表示报告引用的运行不在任务绑定的运行集合内，或尚未到终态。
	TaskReasonReportRunOutOfScope TaskContextReason = "report_run_out_of_scope"
)

// taskReasonStatus 返回原因码对应的状态；未知取值返回空串。
func taskReasonStatus(reason TaskContextReason) TaskFactStatus {
	switch reason {
	case TaskReasonNoStructuredAcceptance, TaskReasonNotBoundToWorkflow, TaskReasonBindingNotAttached, TaskReasonReportNotGenerated:
		return TaskFactUnknown
	case TaskReasonAcceptanceContractMismatch, TaskReasonWorkflowChainInvalid, TaskReasonWorkflowMissing,
		TaskReasonBindingMismatch, TaskReasonDefinitionMismatch, TaskReasonJournalUnavailable, TaskReasonJournalChainInvalid,
		TaskReasonRunMissing, TaskReasonTerminalAnchorMismatch, TaskReasonArtifactMissing, TaskReasonArtifactTampered,
		TaskReasonCompletionAnchorMismatch, TaskReasonReferenceViewInvalid, TaskReasonReferenceViewStale, TaskReasonReportRunOutOfScope:
		return TaskFactBlocked
	}
	return ""
}

// TaskAnchor 记录一条事实的版本锚点：它锚定了哪个不可变或单调递进的标识。
type TaskAnchor struct {
	Kind  TaskAnchorKind `json:"kind"`
	Value string         `json:"value"`
}

// TaskFact 是带状态、来源与锚点的事实包装。Value 仅在 known 时出现，Reason 仅在
// unknown 或 blocked 时出现。
type TaskFact[T any] struct {
	Status TaskFactStatus    `json:"status"`
	Value  *T                `json:"value,omitempty"`
	Source TaskFactSource    `json:"source,omitempty"`
	Anchor *TaskAnchor       `json:"anchor,omitempty"`
	Reason TaskContextReason `json:"reason,omitempty"`
}

// knownTaskFact 用已知事实构造包装。
func knownTaskFact[T any](value T, source TaskFactSource, anchor TaskAnchor) TaskFact[T] {
	copied := value
	return TaskFact[T]{Status: TaskFactKnown, Value: &copied, Source: source, Anchor: &anchor}
}

// unknownTaskFact 构造“尚无事实”的包装并写明原因码。
func unknownTaskFact[T any](reason TaskContextReason) TaskFact[T] {
	return TaskFact[T]{Status: TaskFactUnknown, Reason: reason}
}

// blockedTaskFact 构造“来源存在但不可信”的包装并写明来源与原因码。
func blockedTaskFact[T any](source TaskFactSource, reason TaskContextReason) TaskFact[T] {
	return TaskFact[T]{Status: TaskFactBlocked, Source: source, Reason: reason}
}

// TaskFrozenAcceptance 是创建任务时冻结的验收内容。
type TaskFrozenAcceptance struct {
	AcceptanceDigest string                `json:"acceptance_digest"`
	Criteria         []AcceptanceCriterion `json:"criteria"`
}

// TaskProgress 是任务的当前进度；WorkflowPhase 与 Sequence 只在绑定的工作流可信时出现。
type TaskProgress struct {
	State         string `json:"state"`
	Revision      uint64 `json:"revision"`
	WorkflowPhase string `json:"workflow_phase,omitempty"`
	Sequence      uint64 `json:"sequence,omitempty"`
}

// TaskDecision 是一次人工决定：审批、人工门决定或显式验收。只含身份与锚点，不含操作
// 内容；它说明当时批准了什么，不授予任何权限。
type TaskDecision struct {
	Kind        string `json:"kind"`
	Step        string `json:"step,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	EventSeq    uint64 `json:"event_seq,omitempty"`
	EventHash   string `json:"event_hash,omitempty"`
}

// TaskRunFact 是一条运行事实：身份、来源会话、终态与链头，均来自 Journal。
type TaskRunFact struct {
	RunID     string `json:"run_id"`
	Role      string `json:"role"`
	SessionID string `json:"session_id,omitempty"`
	PrevRunID string `json:"prev_run_id,omitempty"`
	State     string `json:"state"`
	Phase     string `json:"phase,omitempty"`
	LastSeq   uint64 `json:"last_seq"`
	HeadHash  string `json:"head_hash,omitempty"`
	Terminal  bool   `json:"terminal"`
}

// TaskArtifactFact 是一条内容寻址制品的引用。
type TaskArtifactFact struct {
	SHA256  string `json:"sha256"`
	Role    string `json:"role"`
	Present bool   `json:"present"`
}

// TaskEvidenceSummary 是验证报告的摘要：范围与覆盖清单的摘要，以及逐标准的结论。
// 评估与证据没有自有摘要，只能用范围摘要与覆盖摘要锚定。
type TaskEvidenceSummary struct {
	WorkerRunID    string             `json:"worker_run_id"`
	ProgramRunID   string             `json:"program_run_id"`
	ReviewRunID    string             `json:"review_run_id,omitempty"`
	Verdict        string             `json:"verdict"`
	ScopeDigest    string             `json:"scope_digest"`
	CoverageDigest string             `json:"coverage_digest"`
	Criteria       []CriterionVerdict `json:"criteria"`
}

// TaskFindingFact 是一条发现的结构化摘要。程序验证失败的发现有证据校验，模型发现一律
// 是未定论：两类可信度不同，接收方必须按 Status 区分。
type TaskFindingFact struct {
	ID              string   `json:"id"`
	TargetID        string   `json:"target_id,omitempty"`
	CriterionID     string   `json:"criterion_id,omitempty"`
	Category        string   `json:"category"`
	Severity        string   `json:"severity"`
	Status          string   `json:"status"`
	Blocking        bool     `json:"blocking"`
	EvidenceSHA256s []string `json:"evidence_sha256,omitempty"`
}

// TaskPermissionSummary 是授权与预算的非敏感摘要，以配置指纹锚定；不携带配置正文。
type TaskPermissionSummary struct {
	WorkflowID          string `json:"workflow_id"`
	ConfigFingerprint   string `json:"config_fingerprint"`
	AuthorizationSource string `json:"authorization_source"`
	AuthorizedRevision  uint64 `json:"authorized_revision"`
	PlanSHA256          string `json:"plan_sha256,omitempty"`
	Attached            bool   `json:"attached"`
}

// TaskSideEffectSummary 汇总未了结的副作用：已启动但没有终态结果的操作，以及待对账
// 与待审批的标记。
type TaskSideEffectSummary struct {
	StartedOperations int      `json:"started_operations"`
	Settled           int      `json:"settled"`
	Unsettled         []string `json:"unsettled"`
	PendingReconcile  bool     `json:"pending_reconcile"`
	PendingApproval   bool     `json:"pending_approval"`
}

// TaskContextV1 是任务上下文的只读投影。Digest 是自身规范化内容的摘要；输出里没有
// 时间戳，同一存储快照的两次读取逐字节相同。
type TaskContextV1 struct {
	SchemaVersion         int                             `json:"schema_version"`
	TaskID                string                          `json:"task_id"`
	WorkspaceHash         string                          `json:"workspace_hash"`
	Goal                  TaskFact[string]                `json:"goal"`
	FrozenAcceptance      TaskFact[TaskFrozenAcceptance]  `json:"frozen_acceptance"`
	Progress              TaskFact[TaskProgress]          `json:"progress"`
	Decisions             TaskFact[[]TaskDecision]        `json:"decisions"`
	Runs                  TaskFact[[]TaskRunFact]         `json:"runs"`
	Artifacts             TaskFact[[]TaskArtifactFact]    `json:"artifacts"`
	Evidence              TaskFact[TaskEvidenceSummary]   `json:"evidence"`
	Findings              TaskFact[[]TaskFindingFact]     `json:"findings"`
	Remaining             TaskFact[[]string]              `json:"remaining"`
	Permissions           TaskFact[TaskPermissionSummary] `json:"permissions"`
	UnresolvedSideEffects TaskFact[TaskSideEffectSummary] `json:"unresolved_side_effects"`
	Digest                string                          `json:"digest"`
}

// taskContextContent 是摘要输入；Digest 字段自身不参与计算，避免自引用。字段必须与
// TaskContextV1 逐项一致，由测试守住。
type taskContextContent struct {
	SchemaVersion         int                             `json:"schema_version"`
	TaskID                string                          `json:"task_id"`
	WorkspaceHash         string                          `json:"workspace_hash"`
	Goal                  TaskFact[string]                `json:"goal"`
	FrozenAcceptance      TaskFact[TaskFrozenAcceptance]  `json:"frozen_acceptance"`
	Progress              TaskFact[TaskProgress]          `json:"progress"`
	Decisions             TaskFact[[]TaskDecision]        `json:"decisions"`
	Runs                  TaskFact[[]TaskRunFact]         `json:"runs"`
	Artifacts             TaskFact[[]TaskArtifactFact]    `json:"artifacts"`
	Evidence              TaskFact[TaskEvidenceSummary]   `json:"evidence"`
	Findings              TaskFact[[]TaskFindingFact]     `json:"findings"`
	Remaining             TaskFact[[]string]              `json:"remaining"`
	Permissions           TaskFact[TaskPermissionSummary] `json:"permissions"`
	UnresolvedSideEffects TaskFact[TaskSideEffectSummary] `json:"unresolved_side_effects"`
}

// TaskSourceFailure 描述某个事实来源在读取时的失败：状态必须是 unknown 或 blocked，
// 原因码必须与之对应。
type TaskSourceFailure struct {
	Status TaskFactStatus    `json:"status"`
	Reason TaskContextReason `json:"reason"`
}

// TaskRunAnchor 是工作流尝试声明的终态锚点：该运行 Journal 的最后一个事件。
type TaskRunAnchor struct {
	Sequence  uint64 `json:"sequence"`
	EventHash string `json:"event_hash"`
}

// TaskAttemptInput 是工作流一次尝试中投影所需的部分。
type TaskAttemptInput struct {
	Step         string
	Number       int
	RunID        string
	State        string
	Terminal     *TaskRunAnchor
	OutputSHA256 string
	Decision     string
}

// TaskWorkflowInput 是任务绑定的工作流的权威投影中投影所需的部分。Phase 取应用层的
// 判定；参考视图失败时留空。
type TaskWorkflowInput struct {
	WorkflowID          string
	ConfigFingerprint   string
	AuthorizationSource string
	PlanSHA256          string
	AuthorizedRevision  uint64
	Attached            bool
	LastSeq             uint64
	Phase               string
	RequiresReconcile   bool
	Attempts            []TaskAttemptInput
}

// TaskRunInput 是单个运行的事实摘要，由应用层从通过哈希链校验的事件里提取。
type TaskRunInput struct {
	RunID                 string
	Role                  string
	SessionID             string
	PrevRunID             string
	State                 string
	Phase                 string
	LastSeq               uint64
	HeadHash              string
	Terminal              bool
	PendingOperationID    string
	PendingApproval       bool
	StartedOperations     int
	SettledOperations     int
	ApprovalDecisions     []TaskDecision
	VerificationSeq       uint64
	VerificationEventHash string
}

// TaskArtifactInput 是一条被引用的制品及其在位状态。
type TaskArtifactInput struct {
	SHA256   string
	Role     string
	Missing  bool
	Tampered bool
}

// TaskReportInput 是验证报告中投影所需的部分。
type TaskReportInput struct {
	WorkerRunID    string
	ProgramRunID   string
	ReviewRunID    string
	Verdict        string
	ScopeDigest    string
	CoverageDigest string
	Criteria       []CriterionVerdict
	Findings       []TaskFindingFact
}

// TaskReferenceInput 是应用层只读参考视图：它来源的工作流序号，以及验证报告。
type TaskReferenceInput struct {
	Sequence uint64
	Report   *TaskReportInput
}

// TaskCompletionInput 是任务的完成记录：显式验收后才有。
type TaskCompletionInput struct {
	RunID            string
	Sequence         uint64
	EventHash        string
	AcceptanceDigest string
}

// TaskContextInput 是 ComposeTaskContext 的全部输入。任务来源的字段总是存在；其余
// 每个来源要么给出已通过完整性检查的数据，要么用失败说明原因码，两者都不给视为
// 输入不一致。它没有任何模型文本或工具结果正文的位置，投影因此在结构上带不出它们。
type TaskContextInput struct {
	SchemaVersion int
	TaskID        string
	WorkspaceHash string

	Goal              string
	State             string
	Revision          uint64
	Remaining         []string
	Acceptance        *AcceptanceContract
	AcceptanceFailure *TaskSourceFailure
	Completion        *TaskCompletionInput

	WorkflowBound   bool
	Workflow        *TaskWorkflowInput
	WorkflowFailure *TaskSourceFailure

	Runs        []TaskRunInput
	RunsFailure *TaskSourceFailure

	Artifacts []TaskArtifactInput

	Reference        *TaskReferenceInput
	ReferenceFailure *TaskSourceFailure
}

// TaskRunRoles 是运行事实与制品事实里角色的封闭取值。
const (
	// TaskRoleWorker 是执行步骤的运行。
	TaskRoleWorker = "worker"
	// TaskRoleAssignment 是手工分配的运行。
	TaskRoleAssignment = "assignment"
	// TaskRoleDefinition 是工作流定义制品。
	TaskRoleDefinition = "definition"
	// TaskRoleAttemptOutput 是工作流尝试的输出制品。
	TaskRoleAttemptOutput = "attempt_output"
	// TaskRoleCompletionEvidence 是完成记录引用的证据制品。
	TaskRoleCompletionEvidence = "completion_evidence"
)

// ComposeTaskContext 从已加载的事实投影任务上下文。每个字段独立落状态；完整性失败、
// 摘要不符或版本漂移都保守地落 blocked，尚无事实落 unknown，不做任何猜测。输入自相
// 矛盾（例如绑定了工作流却既无数据又无失败说明）时返回错误。
func ComposeTaskContext(input TaskContextInput) (TaskContextV1, error) {
	if input.SchemaVersion != TaskContextSchemaVersion {
		return TaskContextV1{}, fmt.Errorf("task context input schema version %d is unsupported", input.SchemaVersion)
	}
	if input.TaskID == "" {
		return TaskContextV1{}, errors.New("task context input requires a task id")
	}
	if err := input.validate(); err != nil {
		return TaskContextV1{}, err
	}
	built := TaskContextV1{SchemaVersion: TaskContextSchemaVersion, TaskID: input.TaskID, WorkspaceHash: input.WorkspaceHash}
	taskAnchor := TaskAnchor{Kind: TaskAnchorTaskID, Value: input.TaskID}
	revisionAnchor := TaskAnchor{Kind: TaskAnchorTaskRevision, Value: strconv.FormatUint(input.Revision, 10)}
	built.Goal = knownTaskFact(input.Goal, TaskFactSourceTask, taskAnchor)
	built.FrozenAcceptance = taskFrozenAcceptance(input)
	progress := TaskProgress{State: input.State, Revision: input.Revision}
	if input.Workflow != nil {
		progress.WorkflowPhase = input.Workflow.Phase
		progress.Sequence = input.Workflow.LastSeq
	}
	built.Progress = knownTaskFact(progress, TaskFactSourceTask, revisionAnchor)
	built.Remaining = knownTaskFact(cloneStrings(input.Remaining), TaskFactSourceTask, revisionAnchor)

	if input.WorkflowBound && input.WorkflowFailure != nil {
		// 工作流链不可信或尚未挂接时，任务的完整运行集合无从得知：由它派生的字段
		// 跟随工作流来源的结论，不拿个别运行冒充全集。
		built.Permissions = failedTaskFact[TaskPermissionSummary](input.WorkflowFailure, TaskFactSourceWorkflow)
		built.Decisions = failedTaskFact[[]TaskDecision](input.WorkflowFailure, TaskFactSourceWorkflow)
		built.Runs = failedTaskFact[[]TaskRunFact](input.WorkflowFailure, TaskFactSourceWorkflow)
		built.UnresolvedSideEffects = failedTaskFact[TaskSideEffectSummary](input.WorkflowFailure, TaskFactSourceWorkflow)
		built.Artifacts = failedTaskFact[[]TaskArtifactFact](input.WorkflowFailure, TaskFactSourceWorkflow)
		built.Evidence = failedTaskFact[TaskEvidenceSummary](input.WorkflowFailure, TaskFactSourceWorkflow)
		built.Findings = failedTaskFact[[]TaskFindingFact](input.WorkflowFailure, TaskFactSourceWorkflow)
		return finishTaskContext(built)
	}

	// 锚点说明事实锚定在哪里，来源说明它从哪里读到，两者独立。绑定了工作流时运行与
	// 参考视图锚定工作流序号，否则锚定任务修订号。
	anchor := revisionAnchor
	if input.WorkflowBound {
		workflow := input.Workflow
		built.Permissions = knownTaskFact(TaskPermissionSummary{
			WorkflowID: workflow.WorkflowID, ConfigFingerprint: workflow.ConfigFingerprint,
			AuthorizationSource: workflow.AuthorizationSource, AuthorizedRevision: workflow.AuthorizedRevision,
			PlanSHA256: workflow.PlanSHA256, Attached: workflow.Attached,
		}, TaskFactSourceWorkflow, TaskAnchor{Kind: TaskAnchorConfigFingerprint, Value: workflow.ConfigFingerprint})
		anchor = TaskAnchor{Kind: TaskAnchorWorkflowSequence, Value: strconv.FormatUint(workflow.LastSeq, 10)}
	} else {
		built.Permissions = unknownTaskFact[TaskPermissionSummary](TaskReasonNotBoundToWorkflow)
	}
	built.Runs, built.Decisions, built.UnresolvedSideEffects = taskJournalFacts(input, built.FrozenAcceptance, anchor)
	built.Artifacts = taskArtifactFact(input, anchor)
	built.Evidence, built.Findings = taskReferenceFacts(input)
	return finishTaskContext(built)
}

// failedTaskFact 按来源的失败说明给由它派生的字段落状态：尚无事实时不带来源，不可信时
// 带指明的来源。
func failedTaskFact[T any](failure *TaskSourceFailure, source TaskFactSource) TaskFact[T] {
	if failure.Status == TaskFactUnknown {
		return unknownTaskFact[T](failure.Reason)
	}
	return blockedTaskFact[T](source, failure.Reason)
}

// validate 核对输入自洽：失败说明的状态与原因码对应，每个来源要么有数据要么有失败。
// 逐项按固定顺序检查，错误信息因此确定。
func (input TaskContextInput) validate() error {
	failures := []struct {
		name    string
		failure *TaskSourceFailure
	}{
		{name: "acceptance", failure: input.AcceptanceFailure},
		{name: "workflow", failure: input.WorkflowFailure},
		{name: "runs", failure: input.RunsFailure},
		{name: "reference", failure: input.ReferenceFailure},
	}
	for _, item := range failures {
		if item.failure != nil && item.failure.Status != taskReasonStatus(item.failure.Reason) {
			return fmt.Errorf("task context input %s failure has reason %q inconsistent with status %q", item.name, item.failure.Reason, item.failure.Status)
		}
	}
	if (input.Acceptance == nil) == (input.AcceptanceFailure == nil) {
		return errors.New("task context input needs either acceptance or an acceptance failure")
	}
	if input.WorkflowBound {
		if (input.Workflow == nil) == (input.WorkflowFailure == nil) {
			return errors.New("bound task context input needs either workflow data or a workflow failure")
		}
	} else if input.Workflow != nil || input.WorkflowFailure != nil {
		return errors.New("unbound task context input must not carry workflow data or a workflow failure")
	}
	// 运行集合可以为空：任务尚未开始时没有任何运行。只有“既有运行又有失败说明”才矛盾。
	if input.RunsFailure != nil && len(input.Runs) > 0 {
		return errors.New("task context input cannot carry both runs and a runs failure")
	}
	if input.Reference != nil && input.ReferenceFailure != nil {
		return errors.New("task context input cannot carry both a reference view and a reference failure")
	}
	return nil
}

// taskFrozenAcceptance 投影冻结验收：没有结构化合同落 unknown，摘要与标准正文不符落 blocked。
func taskFrozenAcceptance(input TaskContextInput) TaskFact[TaskFrozenAcceptance] {
	if input.AcceptanceFailure != nil {
		return failedTaskFact[TaskFrozenAcceptance](input.AcceptanceFailure, TaskFactSourceTask)
	}
	acceptance := input.Acceptance
	return knownTaskFact(TaskFrozenAcceptance{AcceptanceDigest: acceptance.Digest, Criteria: CloneAcceptanceCriteria(acceptance.Criteria)},
		TaskFactSourceTask, TaskAnchor{Kind: TaskAnchorAcceptanceDigest, Value: acceptance.Digest})
}

// taskJournalFacts 产出运行、副作用与决定三组事实。终态锚点或完成记录与 Journal 链头
// 不符时三组一起失败关闭：它们共享同一批事件。
func taskJournalFacts(input TaskContextInput, frozen TaskFact[TaskFrozenAcceptance], anchor TaskAnchor) (TaskFact[[]TaskRunFact], TaskFact[[]TaskDecision], TaskFact[TaskSideEffectSummary]) {
	if input.RunsFailure != nil {
		return blockedTaskFact[[]TaskRunFact](TaskFactSourceJournal, input.RunsFailure.Reason),
			blockedTaskFact[[]TaskDecision](TaskFactSourceJournal, input.RunsFailure.Reason),
			blockedTaskFact[TaskSideEffectSummary](TaskFactSourceJournal, input.RunsFailure.Reason)
	}
	runs := make([]TaskRunFact, 0, len(input.Runs))
	decisions := make([]TaskDecision, 0)
	summary := TaskSideEffectSummary{Unsettled: []string{}}
	heads := make(map[string]TaskRunInput, len(input.Runs))
	for _, run := range input.Runs {
		heads[run.RunID] = run
		runs = append(runs, TaskRunFact{RunID: run.RunID, Role: run.Role, SessionID: run.SessionID, PrevRunID: run.PrevRunID,
			State: run.State, Phase: run.Phase, LastSeq: run.LastSeq, HeadHash: run.HeadHash, Terminal: run.Terminal})
		summary.StartedOperations += run.StartedOperations
		summary.Settled += run.SettledOperations
		if run.PendingOperationID != "" {
			summary.Unsettled = append(summary.Unsettled, run.PendingOperationID)
		}
		summary.PendingApproval = summary.PendingApproval || run.PendingApproval
		decisions = append(decisions, run.ApprovalDecisions...)
	}
	sort.Slice(runs, func(left, right int) bool { return runs[left].RunID < runs[right].RunID })
	sort.Strings(summary.Unsettled)
	if input.WorkflowBound {
		for _, attempt := range input.Workflow.Attempts {
			if attempt.Decision == "" {
				continue
			}
			decision := TaskDecision{Kind: attempt.Decision, Step: attempt.Step}
			if attempt.Terminal != nil {
				decision.EventSeq, decision.EventHash = attempt.Terminal.Sequence, attempt.Terminal.EventHash
			}
			decisions = append(decisions, decision)
		}
		summary.PendingReconcile = input.Workflow.RequiresReconcile
	}
	if taskTerminalAnchorMismatch(heads, input.WorkflowBound, input.Workflow) {
		return blockedTaskFact[[]TaskRunFact](TaskFactSourceJournal, TaskReasonTerminalAnchorMismatch),
			blockedTaskFact[[]TaskDecision](TaskFactSourceJournal, TaskReasonTerminalAnchorMismatch),
			blockedTaskFact[TaskSideEffectSummary](TaskFactSourceJournal, TaskReasonTerminalAnchorMismatch)
	}
	if !taskCompletionConsistent(input, frozen, heads) {
		return blockedTaskFact[[]TaskRunFact](TaskFactSourceJournal, TaskReasonCompletionAnchorMismatch),
			blockedTaskFact[[]TaskDecision](TaskFactSourceJournal, TaskReasonCompletionAnchorMismatch),
			blockedTaskFact[TaskSideEffectSummary](TaskFactSourceJournal, TaskReasonCompletionAnchorMismatch)
	}
	if input.Completion != nil {
		decisions = append(decisions, TaskDecision{Kind: TaskDecisionAccept, RunID: input.Completion.RunID,
			EventSeq: input.Completion.Sequence, EventHash: input.Completion.EventHash})
	}
	sortTaskDecisions(decisions)
	return knownTaskFact(runs, TaskFactSourceJournal, anchor), knownTaskFact(decisions, TaskFactSourceJournal, anchor),
		knownTaskFact(summary, TaskFactSourceJournal, anchor)
}

// taskTerminalAnchorMismatch 判断工作流尝试声明的终态锚点是否正是对应运行 Journal 的
// 最后一个事件；这是跨存储的链头见证，单条哈希链发现不了链尾被截断。
func taskTerminalAnchorMismatch(heads map[string]TaskRunInput, bound bool, workflow *TaskWorkflowInput) bool {
	if !bound {
		return false
	}
	for _, attempt := range workflow.Attempts {
		if attempt.Terminal == nil || attempt.RunID == "" {
			continue
		}
		run, found := heads[attempt.RunID]
		if !found || run.LastSeq != attempt.Terminal.Sequence || run.HeadHash != attempt.Terminal.EventHash {
			return true
		}
	}
	return false
}

// taskCompletionConsistent 核对完成记录：验收摘要等于冻结摘要；事件锚点按路径核对，
// 自主工作流路径锚定审查运行的末事件，手工分配路径锚定分配运行的最后一次验证事件。
// 任务已标记完成却没有完成记录，同样视为不一致。
func taskCompletionConsistent(input TaskContextInput, frozen TaskFact[TaskFrozenAcceptance], heads map[string]TaskRunInput) bool {
	completion := input.Completion
	if completion == nil {
		return input.State != "completed"
	}
	if completion.AcceptanceDigest != "" && (frozen.Status != TaskFactKnown || completion.AcceptanceDigest != frozen.Value.AcceptanceDigest) {
		return false
	}
	run, found := heads[completion.RunID]
	if !found {
		return false
	}
	if input.WorkflowBound {
		return run.LastSeq == completion.Sequence && run.HeadHash == completion.EventHash
	}
	return run.VerificationSeq == completion.Sequence && run.VerificationEventHash == completion.EventHash
}

// taskArtifactFact 投影被引用的制品；任一缺失或内容不符即整体失败关闭。
func taskArtifactFact(input TaskContextInput, anchor TaskAnchor) TaskFact[[]TaskArtifactFact] {
	artifacts := make([]TaskArtifactFact, 0, len(input.Artifacts))
	missing, tampered := false, false
	for _, item := range input.Artifacts {
		artifacts = append(artifacts, TaskArtifactFact{SHA256: item.SHA256, Role: item.Role, Present: !item.Missing && !item.Tampered})
		missing = missing || item.Missing
		tampered = tampered || item.Tampered
	}
	sort.Slice(artifacts, func(left, right int) bool {
		if artifacts[left].SHA256 != artifacts[right].SHA256 {
			return artifacts[left].SHA256 < artifacts[right].SHA256
		}
		return artifacts[left].Role < artifacts[right].Role
	})
	if tampered {
		return blockedTaskFact[[]TaskArtifactFact](TaskFactSourceArtifact, TaskReasonArtifactTampered)
	}
	if missing {
		return blockedTaskFact[[]TaskArtifactFact](TaskFactSourceArtifact, TaskReasonArtifactMissing)
	}
	// 锚点取排序后摘要清单的哈希，同一批制品因此总得到同一个锚点。
	digests := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		digests = append(digests, artifact.SHA256)
	}
	sum := sha256.Sum256([]byte(strings.Join(digests, "\n")))
	return knownTaskFact(artifacts, TaskFactSourceArtifact, TaskAnchor{Kind: TaskAnchorArtifactSHA256, Value: "sha256:" + hex.EncodeToString(sum[:])})
}

// taskReferenceFacts 投影验证报告的摘要与发现。报告是持久化的制品，读取路径只做
// 核对：参考视图必须与权威投影同一序号，报告引用的运行必须落在绑定的运行集合内且
// 已到终态。
func taskReferenceFacts(input TaskContextInput) (TaskFact[TaskEvidenceSummary], TaskFact[[]TaskFindingFact]) {
	if !input.WorkflowBound {
		return unknownTaskFact[TaskEvidenceSummary](TaskReasonNotBoundToWorkflow), unknownTaskFact[[]TaskFindingFact](TaskReasonNotBoundToWorkflow)
	}
	if input.RunsFailure != nil {
		return blockedTaskFact[TaskEvidenceSummary](TaskFactSourceJournal, input.RunsFailure.Reason),
			blockedTaskFact[[]TaskFindingFact](TaskFactSourceJournal, input.RunsFailure.Reason)
	}
	if input.ReferenceFailure != nil {
		return blockedTaskFact[TaskEvidenceSummary](TaskFactSourceReference, input.ReferenceFailure.Reason),
			blockedTaskFact[[]TaskFindingFact](TaskFactSourceReference, input.ReferenceFailure.Reason)
	}
	reference := input.Reference
	if reference == nil || reference.Report == nil {
		return unknownTaskFact[TaskEvidenceSummary](TaskReasonReportNotGenerated), unknownTaskFact[[]TaskFindingFact](TaskReasonReportNotGenerated)
	}
	if reference.Sequence != input.Workflow.LastSeq {
		return blockedTaskFact[TaskEvidenceSummary](TaskFactSourceReference, TaskReasonReferenceViewStale),
			blockedTaskFact[[]TaskFindingFact](TaskFactSourceReference, TaskReasonReferenceViewStale)
	}
	heads := make(map[string]TaskRunInput, len(input.Runs))
	for _, run := range input.Runs {
		heads[run.RunID] = run
	}
	if taskReportRunOutOfScope(reference.Report, heads) {
		return blockedTaskFact[TaskEvidenceSummary](TaskFactSourceReference, TaskReasonReportRunOutOfScope),
			blockedTaskFact[[]TaskFindingFact](TaskFactSourceReference, TaskReasonReportRunOutOfScope)
	}
	report := reference.Report
	anchor := TaskAnchor{Kind: TaskAnchorScopeDigest, Value: report.ScopeDigest}
	findings := make([]TaskFindingFact, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findings = append(findings, TaskFindingFact{ID: finding.ID, TargetID: finding.TargetID, CriterionID: finding.CriterionID,
			Category: finding.Category, Severity: finding.Severity, Status: finding.Status, Blocking: finding.Blocking,
			EvidenceSHA256s: cloneStrings(finding.EvidenceSHA256s)})
	}
	sort.Slice(findings, func(left, right int) bool { return findings[left].ID < findings[right].ID })
	return knownTaskFact(TaskEvidenceSummary{WorkerRunID: report.WorkerRunID, ProgramRunID: report.ProgramRunID, ReviewRunID: report.ReviewRunID,
		Verdict: report.Verdict, ScopeDigest: report.ScopeDigest, CoverageDigest: report.CoverageDigest,
		Criteria: cloneCriterionVerdicts(report.Criteria)}, TaskFactSourceReference, anchor), knownTaskFact(findings, TaskFactSourceReference, anchor)
}

// taskReportRunOutOfScope 判断报告引用的运行是否都落在运行集合内且已到终态。审查
// 步骤尚未到达时审查运行标识为空，按未到达处理。
func taskReportRunOutOfScope(report *TaskReportInput, heads map[string]TaskRunInput) bool {
	if report.WorkerRunID == "" || report.ProgramRunID == "" {
		return true
	}
	for _, id := range []string{report.WorkerRunID, report.ProgramRunID, report.ReviewRunID} {
		if id == "" {
			continue
		}
		run, found := heads[id]
		if !found || !run.Terminal {
			return true
		}
	}
	return false
}

// finishTaskContext 补全投影的规范化摘要。
func finishTaskContext(context TaskContextV1) (TaskContextV1, error) {
	digest, err := taskContextDigest(context)
	if err != nil {
		return TaskContextV1{}, fmt.Errorf("compute task context digest: %w", err)
	}
	context.Digest = digest
	return context, nil
}

// taskContextDigest 计算不含摘要字段自身的规范化摘要。
func taskContextDigest(context TaskContextV1) (string, error) {
	return canonicaljson.Fingerprint(TaskContextDigestPrefix, taskContextContentOf(context))
}

// taskContextContentOf 取出参与摘要的内容；与 TaskContextV1 的字段逐一对应，由测试守住。
func taskContextContentOf(context TaskContextV1) taskContextContent {
	return taskContextContent{
		SchemaVersion: context.SchemaVersion, TaskID: context.TaskID, WorkspaceHash: context.WorkspaceHash,
		Goal: context.Goal, FrozenAcceptance: context.FrozenAcceptance, Progress: context.Progress,
		Decisions: context.Decisions, Runs: context.Runs, Artifacts: context.Artifacts, Evidence: context.Evidence,
		Findings: context.Findings, Remaining: context.Remaining, Permissions: context.Permissions,
		UnresolvedSideEffects: context.UnresolvedSideEffects,
	}
}

// Validate 核对投影自洽：结构版本、摘要可重算，且每个字段的状态、来源、锚点与原因码
// 都在封闭集合内并相互对应。unknown 与 blocked 的字段不得携带取值。
func (context TaskContextV1) Validate() error {
	if context.SchemaVersion != TaskContextSchemaVersion {
		return fmt.Errorf("task context schema version %d is unsupported", context.SchemaVersion)
	}
	if context.TaskID == "" {
		return errors.New("task context requires a task id")
	}
	digest, err := taskContextDigest(context)
	if err != nil {
		return err
	}
	if digest != context.Digest {
		return errors.New("task context digest does not match its content")
	}
	facts := []taskFactView{
		taskFactViewOf(context.Goal), taskFactViewOf(context.FrozenAcceptance), taskFactViewOf(context.Progress),
		taskFactViewOf(context.Decisions), taskFactViewOf(context.Runs), taskFactViewOf(context.Artifacts),
		taskFactViewOf(context.Evidence), taskFactViewOf(context.Findings), taskFactViewOf(context.Remaining),
		taskFactViewOf(context.Permissions), taskFactViewOf(context.UnresolvedSideEffects),
	}
	for _, fact := range facts {
		if err := fact.validate(); err != nil {
			return err
		}
	}
	return nil
}

// BlockedReason 返回按字段顺序第一个落 blocked 的字段的原因码；没有 blocked 字段时
// 第二个返回值为 false。只给原因码，不带路径或错误原文，供消费方决定是否失败关闭。
func (context TaskContextV1) BlockedReason() (TaskContextReason, bool) {
	facts := []struct {
		status TaskFactStatus
		reason TaskContextReason
	}{
		{context.Goal.Status, context.Goal.Reason},
		{context.FrozenAcceptance.Status, context.FrozenAcceptance.Reason},
		{context.Progress.Status, context.Progress.Reason},
		{context.Decisions.Status, context.Decisions.Reason},
		{context.Runs.Status, context.Runs.Reason},
		{context.Artifacts.Status, context.Artifacts.Reason},
		{context.Evidence.Status, context.Evidence.Reason},
		{context.Findings.Status, context.Findings.Reason},
		{context.Remaining.Status, context.Remaining.Reason},
		{context.Permissions.Status, context.Permissions.Reason},
		{context.UnresolvedSideEffects.Status, context.UnresolvedSideEffects.Reason},
	}
	for _, fact := range facts {
		if fact.status == TaskFactBlocked {
			return fact.reason, true
		}
	}
	return "", false
}

// taskFactView 是校验时对单个事实的统一视角。
type taskFactView struct {
	status   TaskFactStatus
	hasValue bool
	source   TaskFactSource
	anchor   *TaskAnchor
	reason   TaskContextReason
}

// taskFactViewOf 把任意类型的事实收敛成校验视角。
func taskFactViewOf[T any](fact TaskFact[T]) taskFactView {
	return taskFactView{status: fact.Status, hasValue: fact.Value != nil, source: fact.Source, anchor: fact.Anchor, reason: fact.Reason}
}

// validate 核对单个字段的状态、来源、锚点与原因码相互对应。
func (fact taskFactView) validate() error {
	switch fact.status {
	case TaskFactKnown:
		if !fact.hasValue || fact.reason != "" || !validTaskFactSource(fact.source) || fact.anchor == nil || !validTaskAnchorKind(fact.anchor.Kind) || fact.anchor.Value == "" {
			return errors.New("known fact needs a value, a source and an anchor")
		}
	case TaskFactUnknown, TaskFactBlocked:
		if fact.hasValue || taskReasonStatus(fact.reason) != fact.status {
			return fmt.Errorf("fact without a value needs a reason matching status %q", fact.status)
		}
		if fact.status == TaskFactBlocked && !validTaskFactSource(fact.source) {
			return errors.New("blocked fact needs a source")
		}
	default:
		return fmt.Errorf("task fact status %q is unsupported", fact.status)
	}
	return nil
}

// validTaskFactSource 判断来源是否在封闭集合内。
func validTaskFactSource(source TaskFactSource) bool {
	switch source {
	case TaskFactSourceTask, TaskFactSourceWorkflow, TaskFactSourceJournal, TaskFactSourceArtifact, TaskFactSourceReference:
		return true
	}
	return false
}

// validTaskAnchorKind 判断锚点类型是否在封闭集合内。
func validTaskAnchorKind(kind TaskAnchorKind) bool {
	switch kind {
	case TaskAnchorTaskID, TaskAnchorTaskRevision, TaskAnchorAcceptanceDigest, TaskAnchorWorkflowSequence,
		TaskAnchorConfigFingerprint, TaskAnchorScopeDigest, TaskAnchorArtifactSHA256:
		return true
	}
	return false
}

// sortTaskDecisions 按运行标识、事件序号与种类排序，保证摘要确定。
func sortTaskDecisions(decisions []TaskDecision) {
	sort.SliceStable(decisions, func(left, right int) bool {
		if decisions[left].RunID != decisions[right].RunID {
			return decisions[left].RunID < decisions[right].RunID
		}
		if decisions[left].EventSeq != decisions[right].EventSeq {
			return decisions[left].EventSeq < decisions[right].EventSeq
		}
		return decisions[left].Kind < decisions[right].Kind
	})
}

// cloneStrings 复制字符串切片；nil 变为空切片，序列化因此稳定。
func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string{}, values...)
}

// cloneCriterionVerdicts 复制逐标准结论，切片本身与内部切片都不与调用方共享。
func cloneCriterionVerdicts(verdicts []CriterionVerdict) []CriterionVerdict {
	if len(verdicts) == 0 {
		return []CriterionVerdict{}
	}
	return append([]CriterionVerdict{}, verdicts...)
}

// RenderTaskHandoff 把任务上下文渲染成给新会话的交接摘要：只渲染 known 的事实，每条
// 带来源与锚点；unknown 与 blocked 作为显式标记出现并带原因码，接收方不得把它们当作
// 已完成或已通过。体积不超过 TaskHandoffMaxBytes，超限时省略列表条目并标出省略数量。
func RenderTaskHandoff(context TaskContextV1) string {
	writer := taskHandoffWriter{lines: make([]string, 0, 32)}
	writer.required("task %s", context.TaskID)
	writer.required("workspace %s", context.WorkspaceHash)
	writer.required("digest %s", context.Digest)
	writer.required("goal %s", taskHandoffPreview(context.Goal, TaskHandoffPreviewRunes))
	writer.required("acceptance %s", taskHandoffAcceptanceLine(context.FrozenAcceptance))
	writer.required("progress %s", taskHandoffProgressLine(context.Progress))
	writer.required("permissions %s", taskHandoffPermissionsLine(context.Permissions))
	writer.required("evidence %s", taskHandoffEvidenceLine(context.Evidence))
	writer.required("unresolved side effects %s", taskHandoffSideEffectsLine(context.UnresolvedSideEffects))
	if context.Remaining.Status == TaskFactKnown {
		for _, item := range *context.Remaining.Value {
			writer.optional("  remaining %s", taskHandoffPreviewText(item, TaskHandoffPreviewRunes))
		}
	} else {
		writer.optional("  remaining %s", taskHandoffStatusLine(context.Remaining.Status, context.Remaining.Reason))
	}
	if context.Decisions.Status == TaskFactKnown {
		for _, decision := range *context.Decisions.Value {
			writer.optional("  decision %s", taskHandoffDecisionLine(decision))
		}
	} else {
		writer.optional("  decisions %s", taskHandoffStatusLine(context.Decisions.Status, context.Decisions.Reason))
	}
	if context.Runs.Status == TaskFactKnown {
		for _, run := range *context.Runs.Value {
			writer.optional("  run %s role=%s state=%s terminal=%t last_seq=%d head=%s", run.RunID, run.Role, run.State, run.Terminal, run.LastSeq, taskHandoffHash(run.HeadHash))
		}
	} else {
		writer.optional("  runs %s", taskHandoffStatusLine(context.Runs.Status, context.Runs.Reason))
	}
	if context.Findings.Status == TaskFactKnown {
		for _, finding := range *context.Findings.Value {
			writer.optional("  finding %s status=%s severity=%s blocking=%t", finding.ID, finding.Status, finding.Severity, finding.Blocking)
		}
	} else {
		writer.optional("  findings %s", taskHandoffStatusLine(context.Findings.Status, context.Findings.Reason))
	}
	if context.Artifacts.Status == TaskFactKnown {
		for _, artifact := range *context.Artifacts.Value {
			writer.optional("  artifact %s role=%s present=%t", taskHandoffHash(artifact.SHA256), artifact.Role, artifact.Present)
		}
	} else {
		writer.optional("  artifacts %s", taskHandoffStatusLine(context.Artifacts.Status, context.Artifacts.Reason))
	}
	return writer.render()
}

// taskHandoffWriter 按体积预算拼接交接摘要：必需行总是保留，列表行超限时省略。
type taskHandoffWriter struct {
	lines   []string
	omitted int
}

// taskHandoffOmissionFormat 是省略说明的格式；预算为它保留最坏情况的字节数。
const taskHandoffOmissionFormat = "... %d list line(s) omitted: handoff truncated\n"

// required 加入一行必需内容。
func (writer *taskHandoffWriter) required(format string, arguments ...any) {
	writer.lines = append(writer.lines, fmt.Sprintf(format, arguments...))
}

// optional 加入一行列表内容；超出体积预算时省略并计数。
func (writer *taskHandoffWriter) optional(format string, arguments ...any) {
	line := fmt.Sprintf(format, arguments...)
	reserved := len(TaskHandoffDisclaimer) + 1 + len(fmt.Sprintf(taskHandoffOmissionFormat, int64(1<<62)))
	if writer.size()+len(line)+1 > TaskHandoffMaxBytes-reserved {
		writer.omitted++
		return
	}
	writer.lines = append(writer.lines, line)
}

// size 返回当前各行占用的字节数，含换行。
func (writer *taskHandoffWriter) size() int {
	total := 0
	for _, line := range writer.lines {
		total += len(line) + 1
	}
	return total
}

// render 输出最终文本；有省略时在免责说明之前写明省略数量。免责说明是合同的一部分：
// 必需行本身超限时截断正文给它让位，任何情况下它都完整地留在末尾。
func (writer *taskHandoffWriter) render() string {
	var builder strings.Builder
	for _, line := range writer.lines {
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	if writer.omitted > 0 {
		fmt.Fprintf(&builder, taskHandoffOmissionFormat, writer.omitted)
	}
	disclaimer := TaskHandoffDisclaimer + "\n"
	body := builder.String()
	if len(body)+len(disclaimer) <= TaskHandoffMaxBytes {
		return body + disclaimer
	}
	// 必需行本身超限是异常输入：按字符边界截断正文并标明，不输出残缺字符。
	marker := "... handoff truncated\n"
	limit := TaskHandoffMaxBytes - len(disclaimer) - len(marker)
	for limit > 0 && !utf8.RuneStart(body[limit]) {
		limit--
	}
	return body[:limit] + marker + disclaimer
}

// taskHandoffStatusLine 把 unknown 与 blocked 渲染成显式标记。
func taskHandoffStatusLine(status TaskFactStatus, reason TaskContextReason) string {
	return fmt.Sprintf("%s - %s", status, reason)
}

// taskHandoffAcceptanceLine 渲染冻结验收。
func taskHandoffAcceptanceLine(fact TaskFact[TaskFrozenAcceptance]) string {
	if fact.Status != TaskFactKnown {
		return taskHandoffStatusLine(fact.Status, fact.Reason)
	}
	return fmt.Sprintf("digest=%s criteria=%d anchor=%s:%s", taskHandoffHash(fact.Value.AcceptanceDigest), len(fact.Value.Criteria), fact.Anchor.Kind, fact.Anchor.Value)
}

// taskHandoffProgressLine 渲染进度。
func taskHandoffProgressLine(fact TaskFact[TaskProgress]) string {
	if fact.Status != TaskFactKnown {
		return taskHandoffStatusLine(fact.Status, fact.Reason)
	}
	progress := fact.Value
	if progress.WorkflowPhase != "" {
		return fmt.Sprintf("state=%s revision=%d phase=%s sequence=%d anchor=%s:%s", progress.State, progress.Revision, progress.WorkflowPhase, progress.Sequence, fact.Anchor.Kind, fact.Anchor.Value)
	}
	return fmt.Sprintf("state=%s revision=%d anchor=%s:%s", progress.State, progress.Revision, fact.Anchor.Kind, fact.Anchor.Value)
}

// taskHandoffPermissionsLine 渲染授权摘要。
func taskHandoffPermissionsLine(fact TaskFact[TaskPermissionSummary]) string {
	if fact.Status != TaskFactKnown {
		return taskHandoffStatusLine(fact.Status, fact.Reason)
	}
	permissions := fact.Value
	return fmt.Sprintf("workflow=%s config=%s source=%s authorized_revision=%d attached=%t plan=%s",
		permissions.WorkflowID, taskHandoffHash(permissions.ConfigFingerprint), permissions.AuthorizationSource,
		permissions.AuthorizedRevision, permissions.Attached, taskHandoffHash(permissions.PlanSHA256))
}

// taskHandoffEvidenceLine 渲染验证证据摘要。
func taskHandoffEvidenceLine(fact TaskFact[TaskEvidenceSummary]) string {
	if fact.Status != TaskFactKnown {
		return taskHandoffStatusLine(fact.Status, fact.Reason)
	}
	evidence := fact.Value
	return fmt.Sprintf("verdict=%s scope=%s coverage=%s worker=%s program=%s review=%s",
		evidence.Verdict, taskHandoffHash(evidence.ScopeDigest), taskHandoffHash(evidence.CoverageDigest),
		evidence.WorkerRunID, evidence.ProgramRunID, evidence.ReviewRunID)
}

// taskHandoffSideEffectsLine 渲染未了结副作用。未了结操作只列出前若干个并标出省略数量：
// 这一列来自任务数据，不设上限时单凭它就能让必需内容超过体积上限。
func taskHandoffSideEffectsLine(fact TaskFact[TaskSideEffectSummary]) string {
	if fact.Status != TaskFactKnown {
		return taskHandoffStatusLine(fact.Status, fact.Reason)
	}
	summary := fact.Value
	unsettled := "none"
	if len(summary.Unsettled) > 0 {
		shown := summary.Unsettled
		if len(shown) > TaskHandoffUnsettledPreview {
			shown = append(append([]string{}, shown[:TaskHandoffUnsettledPreview]...),
				fmt.Sprintf("...(+%d more)", len(shown)-TaskHandoffUnsettledPreview))
		}
		unsettled = strings.Join(shown, ",")
	}
	return fmt.Sprintf("started=%d settled=%d unsettled=%s pending_reconcile=%t pending_approval=%t",
		summary.StartedOperations, summary.Settled, unsettled, summary.PendingReconcile, summary.PendingApproval)
}

// taskHandoffDecisionLine 渲染一条决定。
func taskHandoffDecisionLine(decision TaskDecision) string {
	parts := []string{decision.Kind}
	if decision.RunID != "" {
		parts = append(parts, "run="+decision.RunID)
	}
	if decision.Step != "" {
		parts = append(parts, "step="+decision.Step)
	}
	if decision.OperationID != "" {
		parts = append(parts, "operation="+decision.OperationID)
	}
	if decision.Fingerprint != "" {
		parts = append(parts, "fingerprint="+taskHandoffHash(decision.Fingerprint))
	}
	if decision.EventSeq != 0 || decision.EventHash != "" {
		parts = append(parts, fmt.Sprintf("seq=%d", decision.EventSeq))
	}
	if decision.EventHash != "" {
		parts = append(parts, "hash="+taskHandoffHash(decision.EventHash))
	}
	return strings.Join(parts, " ")
}

// taskHandoffPreview 渲染目标：取值保留有限字符，非 known 时是显式标记。
func taskHandoffPreview(fact TaskFact[string], limit int) string {
	if fact.Status != TaskFactKnown {
		return taskHandoffStatusLine(fact.Status, fact.Reason)
	}
	return taskHandoffPreviewText(*fact.Value, limit)
}

// taskHandoffPreviewText 把文本截到给定字符数并标明截断。
func taskHandoffPreviewText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + " ...(truncated)"
}

// taskHandoffHash 保留摘要与哈希的前若干位，空串原样返回。
func taskHandoffHash(value string) string {
	trimmed := strings.TrimPrefix(value, "sha256:")
	if len(trimmed) > TaskHandoffHashRunes {
		return trimmed[:TaskHandoffHashRunes]
	}
	return trimmed
}
