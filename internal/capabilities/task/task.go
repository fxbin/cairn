// [INPUT]: 依赖运行标识与共享脱敏合同。
// [OUTPUT]: 提供任务生命周期、分配、工作流绑定与完成证据锚点。
// [POS]: 定义任务事实。应用层交叉核验 Run Journal 与工作流证据。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package task 定义持久任务生命周期。
// Author: fxbin
package task

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

const (
	// SchemaVersion 标识当前任务持久化合同。
	SchemaVersion = 1
	maxTextBytes  = 8 << 10
	maxItems      = 16
)

var (
	// ErrConflict 表示任务版本或分配身份与请求不一致。
	ErrConflict = errors.New("task revision or assignment conflict")
	// ErrTransition 表示当前状态不允许请求的生命周期迁移。
	ErrTransition = errors.New("invalid task transition")
)

// ID 唯一标识一项跨会话任务。
type ID string

// State 表示由任务所有者推进的持久生命周期。
type State string

const (
	// Active 允许为任务分配运行。
	Active State = "active"
	// Paused 暂停后续分配。
	Paused State = "paused"
	// Cancelled 终结任务并保留历史。
	Cancelled State = "cancelled"
	// Completed 记录用户基于可信验证证据的验收。
	Completed State = "completed"
)

// Definition 保存冻结的目标和用户提供的验收条件。Criteria 是新版结构化
// 合同；为空表示旧任务，完成时按既有可信验证规则兼容评估。
type Definition struct {
	Goal             string                         `json:"goal"`
	Acceptance       []string                       `json:"acceptance"`
	Criteria         []contract.AcceptanceCriterion `json:"criteria,omitempty"`
	AcceptanceDigest string                         `json:"acceptance_digest,omitempty"`
}

// Progress 保存用户确认的当前进度与待办。
type Progress struct {
	Summary   string   `json:"summary,omitempty"`
	Remaining []string `json:"remaining,omitempty"`
}

// Evidence 固定已核验的 Run 事件与证据制品。自主任务另绑定工作流身份和报告。
type Evidence struct {
	RunID            domain.RunID  `json:"run_id"`
	Sequence         uint64        `json:"seq"`
	EventHash        string        `json:"event_hash"`
	AcceptanceDigest string        `json:"acceptance_digest,omitempty"`
	EvidenceRef      *contract.Ref `json:"evidence_ref,omitempty"`
	WorkflowID       string        `json:"workflow_id,omitempty"`
}

// Task 保存工作区作用域、冻结定义和当前任务事实。
type Task struct {
	SchemaVersion   int              `json:"schema_version"`
	ID              ID               `json:"id"`
	WorkspaceHash   string           `json:"workspace_hash"`
	Definition      Definition       `json:"definition"`
	Revision        uint64           `json:"revision"`
	State           State            `json:"state"`
	Progress        Progress         `json:"progress"`
	Completion      *Evidence        `json:"completion,omitempty"`
	WorkflowBinding *WorkflowBinding `json:"workflow_binding,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// Assignment 冻结一次请求到会话与 Run 创建事件的绑定，早于 Run Journal 提交。
type Assignment struct {
	ID                string       `json:"id"`
	Revision          uint64       `json:"task_revision"`
	RunID             domain.RunID `json:"run_id"`
	SessionID         string       `json:"session_id"`
	PreviousRunID     domain.RunID `json:"previous_run_id,omitempty"`
	CreationEventID   string       `json:"creation_event_id"`
	ConfigFingerprint string       `json:"config_fingerprint"`
	CreatedAt         time.Time    `json:"created_at"`
}

// New 创建版本 1 的任务。验收条件在整个生命周期内保持冻结。
func New(workspaceHash string, definition Definition, now time.Time) (Task, error) {
	goal, err := cleanText(definition.Goal, false)
	if err != nil {
		return Task{}, fmt.Errorf("task goal: %w", err)
	}
	acceptance, criteria, digest, err := freezeDefinition(definition)
	if err != nil {
		return Task{}, err
	}
	return Task{
		SchemaVersion: SchemaVersion, ID: ID(domain.NewID("task")), WorkspaceHash: workspaceHash,
		Definition: Definition{Goal: goal, Acceptance: acceptance, Criteria: criteria, AcceptanceDigest: digest}, Revision: 1, State: Active,
		Progress:  Progress{Remaining: append([]string(nil), acceptance...)},
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// freezeDefinition 校验并冻结验收条件。结构化标准优先；旧调用方只提供文本时
// 保留无合同的兼容形态，由应用层决定是否补全检查绑定。
func freezeDefinition(definition Definition) ([]string, []contract.AcceptanceCriterion, string, error) {
	if len(definition.Criteria) == 0 {
		acceptance, err := cleanItems(definition.Acceptance, true)
		if err != nil {
			return nil, nil, "", fmt.Errorf("task acceptance: %w", err)
		}
		return acceptance, nil, "", nil
	}
	criteria := contract.CloneAcceptanceCriteria(definition.Criteria)
	for index := range criteria {
		criteria[index].Statement = strings.TrimSpace(criteria[index].Statement)
	}
	acceptanceContract, err := contract.NewAcceptanceContract(criteria)
	if err != nil {
		return nil, nil, "", fmt.Errorf("task acceptance contract: %w", err)
	}
	statements := make([]string, len(acceptanceContract.Criteria))
	for index, criterion := range acceptanceContract.Criteria {
		statements[index] = criterion.Statement
	}
	if len(definition.Acceptance) != 0 {
		provided, err := cleanItems(definition.Acceptance, true)
		if err != nil {
			return nil, nil, "", fmt.Errorf("task acceptance: %w", err)
		}
		if !slices.Equal(provided, statements) {
			return nil, nil, "", errors.New("task acceptance statements do not match criteria")
		}
	}
	return statements, acceptanceContract.Criteria, acceptanceContract.Digest, nil
}

// Contract 返回冻结的结构化验收合同。旧任务没有合同时返回 found=false；
// 摘要或标准正文与存储事实不一致时失败关闭。
func (current Definition) Contract() (contract.AcceptanceContract, bool, error) {
	if len(current.Criteria) == 0 {
		return contract.AcceptanceContract{}, false, nil
	}
	acceptanceContract, err := contract.NewAcceptanceContract(current.Criteria)
	if err != nil {
		return contract.AcceptanceContract{}, false, err
	}
	if current.AcceptanceDigest == "" || acceptanceContract.Digest != current.AcceptanceDigest {
		return contract.AcceptanceContract{}, false, errors.New("task acceptance digest does not match criteria")
	}
	statements := make([]string, len(acceptanceContract.Criteria))
	for index, criterion := range acceptanceContract.Criteria {
		statements[index] = criterion.Statement
	}
	if !slices.Equal(current.Acceptance, statements) {
		return contract.AcceptanceContract{}, false, errors.New("task acceptance statements do not match criteria")
	}
	return acceptanceContract, true, nil
}

// RecordProgress 更新用户确认的进度。终态任务保留已验收的快照。
func (current *Task) RecordProgress(input Progress) error {
	if current.State != Active && current.State != Paused {
		return ErrTransition
	}
	summary, err := cleanText(input.Summary, true)
	if err != nil {
		return fmt.Errorf("task progress: %w", err)
	}
	remaining, err := cleanItems(input.Remaining, false)
	if err != nil {
		return fmt.Errorf("remaining task work: %w", err)
	}
	current.Progress = Progress{Summary: summary, Remaining: remaining}
	return nil
}

// Transition 执行显式生命周期迁移。完成需要应用层核验的证据。
func (current *Task) Transition(next State, evidence *Evidence) error {
	switch next {
	case Active:
		if current.State != Paused {
			return ErrTransition
		}
	case Paused:
		if current.State != Active {
			return ErrTransition
		}
	case Cancelled:
		if current.State != Active && current.State != Paused {
			return ErrTransition
		}
	case Completed:
		if current.State != Active || len(current.Progress.Remaining) != 0 ||
			evidence == nil || evidence.RunID == "" || evidence.Sequence == 0 || evidence.EventHash == "" {
			return ErrTransition
		}
		if current.WorkflowBinding != nil {
			if !current.WorkflowBinding.Attached || evidence.WorkflowID != current.WorkflowBinding.WorkflowID {
				return ErrTransition
			}
		} else if evidence.WorkflowID != "" {
			return ErrTransition
		}
		if current.Definition.AcceptanceDigest != "" {
			if evidence.AcceptanceDigest != current.Definition.AcceptanceDigest || evidence.EvidenceRef == nil {
				return ErrTransition
			}
		} else if evidence.AcceptanceDigest != "" || evidence.EvidenceRef != nil {
			return ErrTransition
		}
	default:
		return ErrTransition
	}
	current.State = next
	current.Completion = cloneEvidence(evidence)
	return nil
}

// Equal 按值比较证据锚点，包括解引用后的制品引用；指针相等不能代表同一证据。
func (evidence Evidence) Equal(other Evidence) bool {
	if evidence.RunID != other.RunID || evidence.Sequence != other.Sequence || evidence.EventHash != other.EventHash ||
		evidence.AcceptanceDigest != other.AcceptanceDigest || evidence.WorkflowID != other.WorkflowID {
		return false
	}
	if (evidence.EvidenceRef == nil) != (other.EvidenceRef == nil) {
		return false
	}
	return evidence.EvidenceRef == nil || *evidence.EvidenceRef == *other.EvidenceRef
}

// cloneEvidence 复制证据引用，避免调用方后续修改改变任务事实。
func cloneEvidence(evidence *Evidence) *Evidence {
	if evidence == nil {
		return nil
	}
	cloned := *evidence
	if evidence.EvidenceRef != nil {
		reference := *evidence.EvidenceRef
		cloned.EvidenceRef = &reference
	}
	return &cloned
}

// Prompt 从任务快照构造本次 Run 的输入。运行权限仍来自冻结配置及审批。
func (current Task) Prompt() string {
	return fmt.Sprintf("目标\n%s\n\n验收条件\n- %s\n\n已确认进度\n%s\n\n剩余事项\n%s",
		current.Definition.Goal, strings.Join(current.Definition.Acceptance, "\n- "),
		current.Progress.Summary, strings.Join(current.Progress.Remaining, "\n"))
}

// cleanItems 验证条数并复制脱敏后的文本，避免别名改变已冻结输入。
func cleanItems(input []string, required bool) ([]string, error) {
	if len(input) > maxItems || (required && len(input) == 0) {
		return nil, errors.New("invalid task item count")
	}
	result := make([]string, 0, len(input))
	for _, item := range input {
		text, err := cleanText(item, false)
		if err != nil {
			return nil, err
		}
		result = append(result, text)
	}
	return result, nil
}

// cleanText 限制输入体积并复用共享脱敏。SQLite 只保存脱敏后的文本。
func cleanText(input string, optional bool) (string, error) {
	text := strings.TrimSpace(input)
	if len(input) > maxTextBytes || !utf8.ValidString(input) || (!optional && text == "") {
		return "", errors.New("invalid task text")
	}
	return string(contract.Redact([]byte(text))), nil
}
