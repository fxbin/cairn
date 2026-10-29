// events.go 定义工作流执行链的事件、哈希链与重放投影：与运行事件 Journal
// 分离的追加式事实源（ADR-0021），内存状态只是缓存，权威投影始终来自重放。
// Author: fxbin
package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

// SchemaVersion 是工作流事件链的当前模式版本；读取端只拒绝更新版本。
const SchemaVersion = 1

// GenesisHash 是工作流哈希链起点之前的空摘要占位，与运行事件链一致。
const GenesisHash = domain.GenesisHash

// ID 标识一条工作流执行链。
type ID string

// NewID 生成新的工作流标识符。
func NewID() ID { return ID(domain.NewID("wf")) }

// EventType 是工作流事件类型；未知类型在负载校验时失败关闭。
type EventType string

const (
	// EventRunCreated 声明工作流：冻结步骤清单、计划摘要、指纹与授权来源。
	EventRunCreated EventType = "workflow_run_created"
	// EventStepStarted 标记步骤开始执行并绑定输入引用。
	EventStepStarted EventType = "workflow_step_started"
	// EventStepFinished 收束步骤终态并绑定输出引用。
	EventStepFinished EventType = "workflow_step_finished"
	// EventRunFinished 收束整个工作流的终态。
	EventRunFinished EventType = "workflow_run_finished"
)

// ErrConflict 表示追加时观察到的链头序号与调用方预期不一致。
var ErrConflict = errors.New("workflow store conflict")

// Event 是工作流链上的持久化事实。
type Event struct {
	SchemaVersion uint32
	EventID       string
	WorkflowID    ID
	WorkspaceHash string
	Seq           uint64
	OccurredAt    time.Time
	Type          EventType
	Payload       json.RawMessage
	PrevHash      string
	EventHash     string
}

// EventDraft 是尚未赋序号与哈希的待追加事件。
type EventDraft struct {
	WorkflowID    ID
	WorkspaceHash string
	Type          EventType
	Payload       json.RawMessage
	OccurredAt    time.Time
}

// StepSpec 是创建事件中声明的单个步骤身份与种类。
type StepSpec struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
	Kind  string `json:"kind"`
}

// 步骤种类约束：执行、验证与人工审批门；其他值失败关闭。
const (
	StepKindExecute = "execute"
	StepKindVerify  = "verify"
	StepKindAccept  = "accept"
)

// RunCreatedPayload 是 workflow_run_created 的负载。Task 为可选的任务制品
// 绑定：执行链写入方必须提供，缺失时恢复入口失败关闭而不是猜测任务正文。
type RunCreatedPayload struct {
	Steps               []StepSpec `json:"steps"`
	PlanSHA256          string     `json:"plan_sha256"`
	ConfigFingerprint   string     `json:"config_fingerprint"`
	AuthorizationSource string     `json:"authorization_source"`
	Task                StepIO     `json:"task,omitempty"`
}

// StepStartedPayload 是 workflow_step_started 的负载。RunID 可选绑定驱动该
// 步骤的子运行；AuthorizationSource 可选记录显式授权来源（人工门步骤必填）。
type StepStartedPayload struct {
	StepID              string `json:"step_id"`
	Input               StepIO `json:"input,omitempty"`
	RunID               string `json:"run_id,omitempty"`
	AuthorizationSource string `json:"authorization_source,omitempty"`
}

// StepFinishedPayload 是 workflow_step_finished 的负载。
type StepFinishedPayload struct {
	StepID     string    `json:"step_id"`
	State      StepState `json:"state"`
	Output     StepIO    `json:"output,omitempty"`
	ErrorClass string    `json:"error_class,omitempty"`
}

// RunFinishedPayload 是 workflow_run_finished 的负载。
type RunFinishedPayload struct {
	State       string `json:"state"`
	ResumeIndex int    `json:"resume_index"`
}

// 工作流终态与步骤合法收束集合。
const (
	RunStateCompleted = "completed"
	RunStateFailed    = "failed"
	RunStateCancelled = "cancelled"
)

// NewEvent 为草稿赋序号、前哈希与内容哈希；负载不合法或身份缺失时失败关闭。
func NewEvent(draft EventDraft, seq uint64, prevHash string) (Event, error) {
	if draft.WorkflowID == "" {
		return Event{}, errors.New("workflow event id is required")
	}
	if draft.WorkspaceHash == "" {
		return Event{}, errors.New("workflow event workspace hash is required")
	}
	if prevHash == "" {
		prevHash = GenesisHash
	}
	if err := validatePayload(draft.Type, draft.Payload); err != nil {
		return Event{}, err
	}
	event := Event{
		SchemaVersion: SchemaVersion,
		EventID:       domain.NewID("wfe"),
		WorkflowID:    draft.WorkflowID,
		WorkspaceHash: draft.WorkspaceHash,
		Seq:           seq,
		OccurredAt:    draft.OccurredAt.UTC(),
		Type:          draft.Type,
		Payload:       append(json.RawMessage(nil), draft.Payload...),
		PrevHash:      prevHash,
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	hash, err := ComputeEventHash(event)
	if err != nil {
		return Event{}, err
	}
	event.EventHash = hash
	return event, nil
}

// ComputeEventHash 按冻结的二进制字段顺序计算工作流事件 SHA-256 哈希。
func ComputeEventHash(event Event) (string, error) {
	prevHash, err := hex.DecodeString(event.PrevHash)
	if err != nil {
		return "", fmt.Errorf("decode previous workflow event hash: %w", err)
	}
	if len(prevHash) != sha256.Size {
		return "", fmt.Errorf("previous workflow event hash must decode to %d bytes", sha256.Size)
	}
	var body bytes.Buffer
	body.WriteString("cairn-workflow-event-v1\x00")
	writeUint32(&body, event.SchemaVersion)
	writeString(&body, event.EventID)
	writeString(&body, string(event.WorkflowID))
	writeString(&body, event.WorkspaceHash)
	writeUint64(&body, event.Seq)
	writeString(&body, event.OccurredAt.UTC().Format(time.RFC3339Nano))
	writeString(&body, string(event.Type))
	writeBytes(&body, event.Payload)
	writeBytes(&body, prevHash)
	sum := sha256.Sum256(body.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// VerifyChain 验证工作流事件的序号连续性、负载合法性与前后哈希引用。
func VerifyChain(events []Event) error {
	previous := GenesisHash
	for index, event := range events {
		expectedSeq := uint64(index + 1)
		if event.SchemaVersion > SchemaVersion {
			return fmt.Errorf("workflow event %d has unsupported schema version %d", event.Seq, event.SchemaVersion)
		}
		if err := validatePayload(event.Type, event.Payload); err != nil {
			return fmt.Errorf("workflow event %d payload: %w", event.Seq, err)
		}
		if event.Seq != expectedSeq {
			return fmt.Errorf("workflow event sequence mismatch: got %d want %d", event.Seq, expectedSeq)
		}
		if event.PrevHash != previous {
			return fmt.Errorf("workflow event %d previous hash mismatch", event.Seq)
		}
		actual, err := ComputeEventHash(event)
		if err != nil {
			return fmt.Errorf("hash workflow event %d: %w", event.Seq, err)
		}
		if actual != event.EventHash {
			return fmt.Errorf("workflow event %d hash mismatch", event.Seq)
		}
		previous = event.EventHash
	}
	return nil
}

// validatePayload 按事件类型反序列化并校验必填字段；未知类型失败关闭。
func validatePayload(eventType EventType, payload json.RawMessage) error {
	switch eventType {
	case EventRunCreated:
		var created RunCreatedPayload
		if err := json.Unmarshal(payload, &created); err != nil {
			return fmt.Errorf("invalid workflow_run_created payload: %w", err)
		}
		if len(created.Steps) == 0 {
			return errors.New("workflow_run_created requires at least one step")
		}
		if created.PlanSHA256 == "" || created.ConfigFingerprint == "" || created.AuthorizationSource == "" {
			return errors.New("workflow_run_created requires plan digest, config fingerprint and authorization source")
		}
		seen := make(map[string]struct{}, len(created.Steps))
		for index, spec := range created.Steps {
			if spec.ID == "" {
				return fmt.Errorf("workflow step %d has empty id", index)
			}
			if _, duplicate := seen[spec.ID]; duplicate {
				return fmt.Errorf("duplicate workflow step id %q", spec.ID)
			}
			seen[spec.ID] = struct{}{}
			if spec.Index != index {
				return fmt.Errorf("workflow step %q index %d does not match declaration order %d", spec.ID, spec.Index, index)
			}
			switch spec.Kind {
			case StepKindExecute, StepKindVerify, StepKindAccept:
			default:
				return fmt.Errorf("workflow step %q has unknown kind %q", spec.ID, spec.Kind)
			}
		}
		return nil
	case EventStepStarted:
		var started StepStartedPayload
		if err := json.Unmarshal(payload, &started); err != nil {
			return fmt.Errorf("invalid workflow_step_started payload: %w", err)
		}
		if started.StepID == "" {
			return errors.New("workflow_step_started requires step id")
		}
		return nil
	case EventStepFinished:
		var finished StepFinishedPayload
		if err := json.Unmarshal(payload, &finished); err != nil {
			return fmt.Errorf("invalid workflow_step_finished payload: %w", err)
		}
		if finished.StepID == "" {
			return errors.New("workflow_step_finished requires step id")
		}
		switch finished.State {
		case StepSucceeded, StepFailed, StepUncertain, StepCancelled:
			return nil
		default:
			return fmt.Errorf("workflow_step_finished has invalid terminal state %q", finished.State)
		}
	case EventRunFinished:
		var finished RunFinishedPayload
		if err := json.Unmarshal(payload, &finished); err != nil {
			return fmt.Errorf("invalid workflow_run_finished payload: %w", err)
		}
		switch finished.State {
		case RunStateCompleted, RunStateFailed, RunStateCancelled:
			return nil
		default:
			return fmt.Errorf("workflow_run_finished has invalid state %q", finished.State)
		}
	default:
		return fmt.Errorf("unknown workflow event type %q", eventType)
	}
}

// Snapshot 是工作流链重放后的只读投影；恢复位置与对账要求由此唯一确定。
type Snapshot struct {
	WorkflowID          ID
	WorkspaceHash       string
	ConfigFingerprint   string
	PlanSHA256          string
	AuthorizationSource string
	Task                StepIO
	CreatedAt           time.Time
	Steps               []Step
	// Kinds 按步骤 ID 映射创建事件声明的步骤种类。
	Kinds map[string]string
	// StepRuns 按步骤 ID 映射 step_started 声明的子运行标识；仅执行与验证
	// 步骤携带，人工门步骤为空。
	StepRuns          map[string]string
	Finished          bool
	FinishState       string
	ResumeIndex       int
	RequiresReconcile []string
	LastSeq           uint64
}

// Project 校验并重放工作流事件链，返回唯一权威投影；顺序流程约束在重放中
// 强制：任一步骤开始前，其前序步骤必须全部成功，且同刻只允许一个执行中步骤。
func Project(events []Event) (*Snapshot, error) {
	if len(events) == 0 {
		return nil, errors.New("workflow has no events")
	}
	if err := VerifyChain(events); err != nil {
		return nil, err
	}
	if events[0].Type != EventRunCreated {
		return nil, fmt.Errorf("first workflow event must be workflow_run_created, got %q", events[0].Type)
	}
	var created RunCreatedPayload
	if err := json.Unmarshal(events[0].Payload, &created); err != nil {
		return nil, fmt.Errorf("workflow_run_created payload: %w", err)
	}
	steps := make([]Step, len(created.Steps))
	kinds := make(map[string]string, len(created.Steps))
	for index, spec := range created.Steps {
		steps[index] = Step{ID: spec.ID, State: StepPending}
		kinds[spec.ID] = spec.Kind
	}
	plan, err := NewPlan(string(events[0].WorkflowID), steps)
	if err != nil {
		return nil, fmt.Errorf("workflow plan: %w", err)
	}
	snapshot := &Snapshot{
		WorkflowID:          events[0].WorkflowID,
		WorkspaceHash:       events[0].WorkspaceHash,
		ConfigFingerprint:   created.ConfigFingerprint,
		PlanSHA256:          created.PlanSHA256,
		AuthorizationSource: created.AuthorizationSource,
		Task:                created.Task,
		CreatedAt:           events[0].OccurredAt,
		Kinds:               kinds,
		StepRuns:            make(map[string]string, len(created.Steps)),
		ResumeIndex:         -1,
	}
	stepIndex := make(map[string]int, len(created.Steps))
	for index, spec := range created.Steps {
		stepIndex[spec.ID] = index
	}
	for _, event := range events[1:] {
		switch event.Type {
		case EventRunCreated:
			return nil, errors.New("duplicate workflow_run_created event")
		case EventStepStarted:
			var started StepStartedPayload
			if err := json.Unmarshal(event.Payload, &started); err != nil {
				return nil, fmt.Errorf("workflow_step_started payload: %w", err)
			}
			if err := ensureSequentialStart(plan, stepIndex, started.StepID); err != nil {
				return nil, err
			}
			if started.RunID != "" {
				snapshot.StepRuns[started.StepID] = started.RunID
			}
			if err := plan.MarkStep(started.StepID, StepRunning, started.Input, StepIO{}, "", event.OccurredAt); err != nil {
				return nil, fmt.Errorf("workflow step %q: %w", started.StepID, err)
			}
		case EventStepFinished:
			var finished StepFinishedPayload
			if err := json.Unmarshal(event.Payload, &finished); err != nil {
				return nil, fmt.Errorf("workflow_step_finished payload: %w", err)
			}
			if err := plan.MarkStep(finished.StepID, finished.State, StepIO{}, finished.Output, finished.ErrorClass, event.OccurredAt); err != nil {
				return nil, fmt.Errorf("workflow step %q: %w", finished.StepID, err)
			}
		case EventRunFinished:
			if snapshot.Finished {
				return nil, errors.New("duplicate workflow_run_finished event")
			}
			var finished RunFinishedPayload
			if err := json.Unmarshal(event.Payload, &finished); err != nil {
				return nil, fmt.Errorf("workflow_run_finished payload: %w", err)
			}
			snapshot.Finished = true
			snapshot.FinishState = finished.State
		}
	}
	snapshot.Steps = plan.Steps
	snapshot.ResumeIndex = plan.ResumeFrom()
	snapshot.RequiresReconcile = plan.RequiresReconcile()
	snapshot.LastSeq = events[len(events)-1].Seq
	return snapshot, nil
}

// ensureSequentialStart 强制顺序流程：目标步骤之前的所有步骤必须已成功，
// 且当前没有任何执行中或对账未决的步骤。
func ensureSequentialStart(plan *Plan, stepIndex map[string]int, stepID string) error {
	index, found := stepIndex[stepID]
	if !found {
		return fmt.Errorf("workflow step %q is not declared in the created plan", stepID)
	}
	for _, step := range plan.Steps[:index] {
		if step.State != StepSucceeded {
			return fmt.Errorf("workflow step %q cannot start before %q succeeds (state %s)", stepID, step.ID, step.State)
		}
	}
	for _, step := range plan.Steps {
		if step.State == StepRunning || step.State == StepUncertain {
			return fmt.Errorf("workflow step %q cannot start while %q is %s", stepID, step.ID, step.State)
		}
	}
	return nil
}

// writeUint32 将 uint32 以固定大端格式写入哈希输入缓冲区。
func writeUint32(buffer *bytes.Buffer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	writeBytes(buffer, encoded[:])
}

// writeUint64 将 uint64 以固定大端格式写入哈希输入缓冲区。
func writeUint64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	writeBytes(buffer, encoded[:])
}

// writeString 将字符串作为带长度前缀的字节序列写入缓冲区。
func writeString(buffer *bytes.Buffer, value string) { writeBytes(buffer, []byte(value)) }

// writeBytes 将字节序列及其 uint64 长度前缀写入缓冲区。
func writeBytes(buffer *bytes.Buffer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	buffer.Write(length[:])
	buffer.Write(value)
}
