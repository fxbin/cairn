// Package memory 定义可信记忆控制面的受限值合同；它只保存已验证运行的
// 结构化结果与不可逆证据定位，不承载 prompt、模型文本、路径或制品正文。
// 本文件定义可信记忆基础领域合同。
//
// Author: fxbin
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// SchemaVersion 是当前 Memory SQLite 记录合同版本。
	SchemaVersion = 1
	// KindRunOutcome 是唯一允许持久化的情节记忆种类。
	KindRunOutcome = "run_outcome"
	// StatusActive 表示候选可被只读 recall 返回。
	StatusActive = "active"
	// StatusRevoked 表示候选因有限理由被人工或证据流程撤回。
	StatusRevoked = "revoked"
	// StatusSuperseded 表示候选已被同作用域的新候选原子替代。
	StatusSuperseded = "superseded"
	// AuditCaptured 记录首次写入候选的审计动作。
	AuditCaptured = "captured"
	// AuditRevoked 记录撤销动作的审计动作。
	AuditRevoked = "revoked"
	// AuditSuperseded 记录被替代候选的审计动作。
	AuditSuperseded = "superseded"
	// RevokeEvidenceInvalid 表示来源证据已被判定无效。
	RevokeEvidenceInvalid = "evidence_invalid"
	// RevokeHumanRequest 表示操作者明确请求撤销。
	RevokeHumanRequest = "human_request"
)

const (
	// DefaultTTL 是 capture 未显式指定时的最短期保留时间；记忆不允许无限期保留。
	DefaultTTL = 30 * 24 * time.Hour
	// MaxTTL 限制单条情节记忆的最长保留时间，防止未复核信息长期滞留。
	MaxTTL = 90 * 24 * time.Hour
	// DefaultRecallLimit 是只读检索的安全默认上限。
	DefaultRecallLimit = 20
	// MaxRecallLimit 是单次只读检索的硬上限。
	MaxRecallLimit = 100
)

var (
	// ErrNotFound 表示指定候选不存在于当前 Memory Store。
	ErrNotFound = errors.New("memory candidate not found")
	// ErrConflict 表示幂等 capture 发现同一 Run 的输入事实已经发生冲突。
	ErrConflict = errors.New("memory candidate conflict")
)

// EventEvidenceRef 是一个不可逆的 Event 定位；若同一事件声明了经过校验的
// 制品，则只记录其 SHA-256，而绝不复制 artifact.Ref 的预览或元数据。
type EventEvidenceRef struct {
	RunID          string `json:"run_id"`
	EventSeq       uint64 `json:"event_seq"`
	EventHash      string `json:"event_hash"`
	EventType      string `json:"event_type"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
}

// Candidate 是唯一允许写入的 run_outcome 情节记忆。字段全部是固定词表、
// 不可逆摘要或运行标识，不能承载自由文本和来源内容。
type Candidate struct {
	ID                  string             `json:"id"`
	SchemaVersion       int                `json:"schema_version"`
	Kind                string             `json:"kind"`
	WorkspaceHash       string             `json:"workspace_hash"`
	TaskPromptDigest    string             `json:"task_prompt_digest"`
	RunID               string             `json:"run_id"`
	Outcome             string             `json:"outcome"`
	FailureClass        string             `json:"failure_class,omitempty"`
	TrustedVerification bool               `json:"trusted_verification"`
	CreatedAt           time.Time          `json:"created_at"`
	ExpiresAt           time.Time          `json:"expires_at"`
	Status              string             `json:"status"`
	SupersedesID        string             `json:"supersedes_id,omitempty"`
	Evidence            []EventEvidenceRef `json:"evidence"`
}

// AuditEntry 是候选状态变化的脱敏审计行；Reason 仅在撤销时使用有限枚举。
type AuditEntry struct {
	CandidateID string    `json:"candidate_id"`
	Action      string    `json:"action"`
	Reason      string    `json:"reason,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// InspectResult 是单一候选及其只读审计历史的投影。
type InspectResult struct {
	Candidate Candidate    `json:"candidate"`
	Audit     []AuditEntry `json:"audit"`
}

// RecallOptions 描述当前工作区内的确定性白名单检索条件。M2-A 仅增加受限的
// SQLite 精确过滤字段，不接受自然语言、近似相似度或跨工作区范围。
type RecallOptions struct {
	WorkspaceHash       string
	TaskPromptDigest    string
	Outcomes            []string
	FailureClasses      []string
	TrustedVerification string
	CreatedFrom         *time.Time
	CreatedUntil        *time.Time
	Precision           string
	Limit               int
	Now                 time.Time
}

// NewRunOutcomeCandidate 从已验证的 Run 投影构造固定结构候选；它不会接受或
// 持久化任务正文、模型消息、文件路径、命令参数或自由文本摘要。
func NewRunOutcomeCandidate(workspaceHash, runID, taskPromptDigest, outcome, failureClass string, trustedVerification bool, createdAt time.Time, ttl time.Duration, evidence []EventEvidenceRef, supersedesID string) (Candidate, error) {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl <= 0 || ttl > MaxTTL {
		return Candidate{}, fmt.Errorf("memory TTL must be within 1ns and %s", MaxTTL)
	}
	createdAt = createdAt.UTC()
	candidate := Candidate{
		ID:                  CandidateID(workspaceHash, runID),
		SchemaVersion:       SchemaVersion,
		Kind:                KindRunOutcome,
		WorkspaceHash:       workspaceHash,
		TaskPromptDigest:    taskPromptDigest,
		RunID:               runID,
		Outcome:             outcome,
		FailureClass:        failureClass,
		TrustedVerification: trustedVerification,
		CreatedAt:           createdAt,
		ExpiresAt:           createdAt.Add(ttl),
		Status:              StatusActive,
		SupersedesID:        supersedesID,
		Evidence:            append([]EventEvidenceRef(nil), evidence...),
	}
	if err := candidate.Validate(); err != nil {
		return Candidate{}, err
	}
	return candidate, nil
}

// CandidateID 以 workspace 与 Run 的不可逆摘要生成幂等 capture 标识；同一 Run
// 重试永远定位同一候选，且不依赖任务正文或任何用户输入文本。
func CandidateID(workspaceHash, runID string) string {
	sum := sha256.Sum256([]byte(workspaceHash + "\x00" + runID))
	return "mem_" + hex.EncodeToString(sum[:])
}

// NormalizeTTL 规范 capture 的保留期：空值使用安全默认值，越界值失败关闭。
func NormalizeTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		return DefaultTTL, nil
	}
	if ttl < 0 || ttl > MaxTTL {
		return 0, fmt.Errorf("memory TTL must be within 1ns and %s", MaxTTL)
	}
	return ttl, nil
}

// NormalizeRecallLimit 规范只读检索上限，避免调用方以零值绕过资源边界。
func NormalizeRecallLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultRecallLimit, nil
	}
	if limit < 0 || limit > MaxRecallLimit {
		return 0, fmt.Errorf("memory recall limit must be within 1 and %d", MaxRecallLimit)
	}
	return limit, nil
}

// ValidRevocationReason 判断撤销理由是否属于封闭词表。
func ValidRevocationReason(reason string) bool {
	return reason == RevokeEvidenceInvalid || reason == RevokeHumanRequest
}

// Validate 检查候选仅包含 M0/M1 允许的结构化、脱敏字段与受限状态。
func (candidate Candidate) Validate() error {
	if candidate.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported memory candidate schema version %d", candidate.SchemaVersion)
	}
	if candidate.Kind != KindRunOutcome {
		return fmt.Errorf("unsupported memory candidate kind %q", candidate.Kind)
	}
	if candidate.ID != CandidateID(candidate.WorkspaceHash, candidate.RunID) {
		return errors.New("memory candidate ID does not match workspace and run")
	}
	if !validDigest(candidate.WorkspaceHash) || !validDigest(candidate.TaskPromptDigest) {
		return errors.New("memory candidate digest is invalid")
	}
	if !validIdentifier(candidate.RunID) || !validIdentifier(candidate.ID) || (candidate.SupersedesID != "" && !validIdentifier(candidate.SupersedesID)) {
		return errors.New("memory candidate identifier is invalid")
	}
	switch candidate.Outcome {
	case "completed", "failed", "cancelled":
	default:
		return fmt.Errorf("memory candidate outcome %q is invalid", candidate.Outcome)
	}
	if candidate.FailureClass != "" && !validToken(candidate.FailureClass) {
		return errors.New("memory candidate failure class is invalid")
	}
	switch candidate.Status {
	case StatusActive, StatusRevoked, StatusSuperseded:
	default:
		return fmt.Errorf("memory candidate status %q is invalid", candidate.Status)
	}
	if candidate.CreatedAt.IsZero() || candidate.ExpiresAt.IsZero() || !candidate.ExpiresAt.After(candidate.CreatedAt) || candidate.ExpiresAt.Sub(candidate.CreatedAt) > MaxTTL {
		return errors.New("memory candidate retention period is invalid")
	}
	if len(candidate.Evidence) == 0 {
		return errors.New("memory candidate requires evidence")
	}
	for index, reference := range candidate.Evidence {
		if err := reference.Validate(candidate.RunID); err != nil {
			return fmt.Errorf("memory evidence %d: %w", index, err)
		}
	}
	return nil
}

// Validate 检查证据定位在结构上只包含当前 Run 的不可逆 Event/Artifact 标识。
func (reference EventEvidenceRef) Validate(expectedRunID string) error {
	if reference.RunID != expectedRunID || !validIdentifier(reference.RunID) || reference.EventSeq == 0 || !validDigest(reference.EventHash) || !validToken(reference.EventType) {
		return errors.New("memory event evidence is invalid")
	}
	if reference.ArtifactSHA256 != "" && !validDigest(reference.ArtifactSHA256) {
		return errors.New("memory artifact evidence digest is invalid")
	}
	return nil
}

// validDigest 仅接受完整、带 sha256 前缀的不可逆摘要。
func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

// validIdentifier 仅接受固定 ASCII 标识，阻断路径分隔符、空白和自由文本。
func validIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// validToken 接受内部受控词表使用的简短 token，拒绝空白、路径和自由文本。
func validToken(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
