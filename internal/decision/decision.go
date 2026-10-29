// Package decision 定义项目级决策账本的合同与状态机：一条决策记录必须能
// 追溯到既有事实源中的证据，可以被撤销或替代，保留期耗尽后不再冒充现行事实。
//
// 当前状态：合同脚手架，尚无运行时消费方（Agent Loop、CLI 与 Journal 均未
// 接线）。决策账本不是运行事实源，不授予权限，不决定运行终态；注入上下文的
// 项目知识位于 Policy 之下，不能覆盖上层授权。生产接入前需先打通授权链并
// 补齐模型级评测（ADR-0022）。
//
// Author: fxbin
package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

const (
	// SchemaVersion 是决策记录合同的当前版本；读取端只拒绝更新版本。
	SchemaVersion = 1
	// recordIDPrefix 是决策记录标识的固定前缀。
	recordIDPrefix = "dec_"
	// contentFingerprintPrefix 是决策正文内容指纹的版本化域前缀。
	contentFingerprintPrefix = "cairn-decision-content-v1"
	// recordFingerprintPrefix 是决策记录标识派生所用的版本化域前缀。
	recordFingerprintPrefix = "cairn-decision-record-v1"
)

// 决策种类：区分权威项目决策、未验证观察与个人偏好。三者的可信等级不同，
// 权威决策才可以被当作项目规则引用。
const (
	// KindAuthoritative 是经人工确认的项目权威决策。
	KindAuthoritative = "authoritative"
	// KindObservation 是尚未被证据证实的观察线索。
	KindObservation = "unverified_observation"
	// KindPreference 是只对单个主体生效的个人偏好。
	KindPreference = "personal_preference"
)

// 决策状态机取值。
const (
	// StatusProposed 表示记录只是候选，尚未生效。
	StatusProposed = "proposed"
	// StatusActive 表示记录已由人工确认为现行事实。
	StatusActive = "active"
	// StatusSuperseded 表示记录已被同作用域的新决策替代。
	StatusSuperseded = "superseded"
	// StatusRevoked 表示记录被撤销，撤销前的历史仍可追溯。
	StatusRevoked = "revoked"
	// StatusRejected 表示候选在复核中被拒绝。
	StatusRejected = "rejected"
	// StatusExpired 表示记录保留期耗尽，不再代表现行事实。
	StatusExpired = "expired"
)

// 账本审计动作。
const (
	// ActionProposed 记录候选录入动作。
	ActionProposed = "proposed"
	// ActionConfirmed 记录人工确认动作。
	ActionConfirmed = "confirmed"
	// ActionReviewed 记录人工复核动作。
	ActionReviewed = "reviewed"
	// ActionRevoked 记录撤销动作。
	ActionRevoked = "revoked"
	// ActionRejected 记录拒绝动作。
	ActionRejected = "rejected"
	// ActionSuperseded 记录被替代动作。
	ActionSuperseded = "superseded"
)

// 撤销与拒绝理由属于封闭词表，不接受自由文本。
const (
	// RevokeReasonEvidenceInvalid 表示来源证据被判定无效。
	RevokeReasonEvidenceInvalid = "evidence_invalid"
	// RevokeReasonHumanRequest 表示操作者明确请求撤销。
	RevokeReasonHumanRequest = "human_request"
	// RejectReasonNotApproved 表示候选未通过复核。
	RejectReasonNotApproved = "not_approved"
)

// ApprovalSourceHuman 是人工确认的唯一合法来源；模型提出的候选只能走候选
// 状态，不能自行取得该来源。
const ApprovalSourceHuman = "human"

// 保留期与只读检索边界。
const (
	// DefaultTTL 是候选未显式指定保留期时的默认时长。
	DefaultTTL = 180 * 24 * time.Hour
	// MaxTTL 是单条决策记录的最长保留时长。
	MaxTTL = 365 * 24 * time.Hour
	// DefaultQueryLimit 是只读检索的默认上限。
	DefaultQueryLimit = 20
	// MaxQueryLimit 是单次只读检索的硬上限。
	MaxQueryLimit = 100
)

// 决策正文的字段长度上限，防止账本退化为自由文本仓库。
const (
	// MaxContextRunes 是上下文说明的最大字符数。
	MaxContextRunes = 200
	// MaxDecisionRunes 是决策陈述的最大字符数。
	MaxDecisionRunes = 400
	// MaxRationaleRunes 是决策理由的最大字符数。
	MaxRationaleRunes = 800
)

var (
	// ErrNotFound 表示指定记录不存在于当前账本。
	ErrNotFound = errors.New("decision record not found")
	// ErrConflict 表示录入内容与既有记录冲突。
	ErrConflict = errors.New("decision record conflict")
	// ErrDuplicateDecision 表示等价决策已经生效，不能重复录入。
	ErrDuplicateDecision = errors.New("equivalent decision already active")
	// ErrInvalidTransition 表示状态迁移不被合同允许。
	ErrInvalidTransition = errors.New("decision transition is not permitted")
	// ErrNotHumanApproval 表示确认缺少人工来源，候选不能自行生效。
	ErrNotHumanApproval = errors.New("decision activation requires human approval")
)

// DecisionRecord 是一条项目级决策记录。正文一经创建不可改写：变更一律通过
// 新增记录并声明 Supersedes 完成，被替代的旧记录转为 StatusSuperseded。
// 记录只保存作用域内的短文本、证据定位与审计标识，不保存路径、凭据或运行事实。
type DecisionRecord struct {
	SchemaVersion int           `json:"schema_version"`
	ID            string        `json:"id"`
	Project       string        `json:"project"`
	Owner         string        `json:"owner"`
	Kind          string        `json:"kind"`
	Context       string        `json:"context"`
	Decision      string        `json:"decision"`
	Rationale     string        `json:"rationale"`
	Evidence      []EvidenceRef `json:"evidence"`
	Status        string        `json:"status"`
	Supersedes    string        `json:"supersedes,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	ReviewedAt    *time.Time    `json:"reviewed_at,omitempty"`
	ExpiresAt     time.Time     `json:"expires_at"`
}

// AuditEntry 是决策状态迁移的脱敏审计行；Reason 只取封闭词表。
type AuditEntry struct {
	RecordID   string    `json:"record_id"`
	Action     string    `json:"action"`
	Reason     string    `json:"reason,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Proposal 是一次受控录入的输入：它只描述要记录的决策内容与证据定位，不接受
// 运行事实、权限声明、检索文本或任何路径。
type Proposal struct {
	// Project 是决策所属项目的受限标识。
	Project string
	// Owner 是提出决策的主体标识；个人偏好只对该主体可见。
	Owner string
	// Kind 是决策种类，取值见 KindAuthoritative 等常量。
	Kind string
	// Context 是决策适用的上下文说明。
	Context string
	// Decision 是决策陈述。
	Decision string
	// Rationale 是决策理由。
	Rationale string
	// Evidence 是支撑该结论的证据定位，至少一条。
	Evidence []EvidenceRef
	// Supersedes 是本决策替代的既有记录标识；为空表示新增决策。
	Supersedes string
	// TTL 是保留期；零值取 DefaultTTL。
	TTL time.Duration
	// Now 是决策的创建时刻；调用方必须从系统时钟或注入时钟取值。
	Now time.Time
}

// recordContent 是决策正文的规范化指纹载荷。它只包含作用域与正文，不含
// 时间戳，使内容相同的提案可以互相识别为等价决策。
type recordContent struct {
	Project   string        `json:"project"`
	Owner     string        `json:"owner"`
	Kind      string        `json:"kind"`
	Context   string        `json:"context"`
	Decision  string        `json:"decision"`
	Rationale string        `json:"rationale"`
	Evidence  []EvidenceRef `json:"evidence"`
}

// NewProposal 构造一条候选决策记录。正文先经长度与脱敏校验，证据定位必须
// 结构合法；构造结果永远处于候选状态，不是现行事实。
func NewProposal(proposal Proposal) (DecisionRecord, error) {
	if proposal.Now.IsZero() {
		return DecisionRecord{}, errors.New("decision proposal requires an explicit creation time")
	}
	if !validIdentifier(proposal.Project) {
		return DecisionRecord{}, errors.New("decision proposal project is invalid")
	}
	if !validIdentifier(proposal.Owner) {
		return DecisionRecord{}, errors.New("decision proposal owner is invalid")
	}
	switch proposal.Kind {
	case KindAuthoritative, KindObservation, KindPreference:
	default:
		return DecisionRecord{}, fmt.Errorf("decision proposal kind %q is invalid", proposal.Kind)
	}
	if err := validateText("context", proposal.Context, MaxContextRunes); err != nil {
		return DecisionRecord{}, err
	}
	if err := validateText("decision", proposal.Decision, MaxDecisionRunes); err != nil {
		return DecisionRecord{}, err
	}
	if err := validateText("rationale", proposal.Rationale, MaxRationaleRunes); err != nil {
		return DecisionRecord{}, err
	}
	if len(proposal.Evidence) == 0 {
		return DecisionRecord{}, errors.New("decision proposal requires at least one evidence reference")
	}
	for index, reference := range proposal.Evidence {
		if err := reference.Validate(); err != nil {
			return DecisionRecord{}, fmt.Errorf("decision evidence %d: %w", index, err)
		}
	}
	if proposal.Supersedes != "" && !validIdentifier(proposal.Supersedes) {
		return DecisionRecord{}, errors.New("decision proposal supersedes identifier is invalid")
	}
	ttl, err := NormalizeTTL(proposal.TTL)
	if err != nil {
		return DecisionRecord{}, err
	}
	createdAt := proposal.Now.UTC()
	contentKey, err := ContentKey(proposal.Project, proposal.Owner, proposal.Kind, proposal.Context, proposal.Decision, proposal.Rationale, proposal.Evidence)
	if err != nil {
		return DecisionRecord{}, err
	}
	record := DecisionRecord{
		SchemaVersion: SchemaVersion,
		ID:            RecordID(proposal.Project, proposal.Owner, proposal.Kind, contentKey, createdAt),
		Project:       proposal.Project,
		Owner:         proposal.Owner,
		Kind:          proposal.Kind,
		Context:       proposal.Context,
		Decision:      proposal.Decision,
		Rationale:     proposal.Rationale,
		Evidence:      append([]EvidenceRef(nil), proposal.Evidence...),
		Status:        StatusProposed,
		Supersedes:    proposal.Supersedes,
		CreatedAt:     createdAt,
		ExpiresAt:     createdAt.Add(ttl),
	}
	if err := record.Validate(); err != nil {
		return DecisionRecord{}, err
	}
	return record, nil
}

// ContentKey 返回决策正文的规范化内容指纹。内容相同即指纹相同，与创建时刻
// 无关，使账本能识别等价决策并阻止同一结论重复占用现行作用域。
func ContentKey(project, owner, kind, context, decision, rationale string, evidence []EvidenceRef) (string, error) {
	content := recordContent{
		Project:   project,
		Owner:     owner,
		Kind:      kind,
		Context:   context,
		Decision:  decision,
		Rationale: rationale,
		Evidence:  evidence,
	}
	if content.Evidence == nil {
		content.Evidence = []EvidenceRef{}
	}
	return canonicaljson.Fingerprint(contentFingerprintPrefix, content)
}

// RecordID 由作用域、所有者、种类、内容指纹与创建时刻派生。同一提案在同一
// 时刻重试恒定命中同一标识，不同创建时刻则各自独立，保留完整历史。
func RecordID(project, owner, kind, contentKey string, createdAt time.Time) string {
	sum := sha256.Sum256([]byte(recordFingerprintPrefix + "\x00" + project + "\x00" + owner + "\x00" + kind + "\x00" + contentKey + "\x00" + createdAt.UTC().Format(time.RFC3339Nano)))
	return recordIDPrefix + hex.EncodeToString(sum[:])
}

// NormalizeTTL 规范决策记录的保留期：零值取安全默认值，越界值失败关闭。
func NormalizeTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		return DefaultTTL, nil
	}
	if ttl < 0 || ttl > MaxTTL {
		return 0, fmt.Errorf("decision TTL must be within 1ns and %s", MaxTTL)
	}
	return ttl, nil
}

// ValidTransition 判断决策状态迁移是否合法。候选只能被人工确认、拒绝、撤销
// 或到期；现行决策可以被撤销、被替代或到期，任何终态都不可再迁出。
func ValidTransition(from, to string) error {
	switch from {
	case StatusProposed:
		switch to {
		case StatusActive, StatusRejected, StatusRevoked, StatusExpired:
			return nil
		}
	case StatusActive:
		switch to {
		case StatusSuperseded, StatusRevoked, StatusExpired:
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
}

// IsTerminalStatus 判断状态是否为不可再迁出的终态。
func IsTerminalStatus(status string) bool {
	switch status {
	case StatusSuperseded, StatusRevoked, StatusRejected, StatusExpired:
		return true
	default:
		return false
	}
}

// EffectiveStatus 返回记录在给定时刻的有效状态。保留期耗尽时返回过期，
// 使过期记录不再冒充现行事实。
func (record DecisionRecord) EffectiveStatus(now time.Time) string {
	if !IsTerminalStatus(record.Status) && !record.ExpiresAt.IsZero() && !now.IsZero() && !now.Before(record.ExpiresAt) {
		return StatusExpired
	}
	return record.Status
}

// IsAuthoritative 判断记录是否可以作为项目规则引用：必须处于现行状态、种类
// 为权威决策且保留期未耗尽。未验证观察与个人偏好即使现行也不是权威规则。
func (record DecisionRecord) IsAuthoritative(now time.Time) bool {
	return record.EffectiveStatus(now) == StatusActive && record.Kind == KindAuthoritative
}

// VisibleTo 判断记录是否对给定主体可见：项目必须一致，个人偏好只对记录
// 所有者可见。跨项目与跨用户的记录一律不可见。
func (record DecisionRecord) VisibleTo(project, principal string) bool {
	if record.Project != project {
		return false
	}
	if record.Kind == KindPreference {
		return record.Owner == principal
	}
	return true
}

// Validate 校验决策记录的完整闭合合同：作用域标识、种类、正文边界、证据
// 定位、保留期与状态词表。
func (record DecisionRecord) Validate() error {
	if record.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported decision schema version %d", record.SchemaVersion)
	}
	if !validIdentifier(record.Project) || !validIdentifier(record.Owner) || !validIdentifier(record.ID) {
		return errors.New("decision record identifier fields are invalid")
	}
	switch record.Kind {
	case KindAuthoritative, KindObservation, KindPreference:
	default:
		return fmt.Errorf("decision record kind %q is invalid", record.Kind)
	}
	switch record.Status {
	case StatusProposed, StatusActive, StatusSuperseded, StatusRevoked, StatusRejected, StatusExpired:
	default:
		return fmt.Errorf("decision record status %q is invalid", record.Status)
	}
	if err := validateText("context", record.Context, MaxContextRunes); err != nil {
		return err
	}
	if err := validateText("decision", record.Decision, MaxDecisionRunes); err != nil {
		return err
	}
	if err := validateText("rationale", record.Rationale, MaxRationaleRunes); err != nil {
		return err
	}
	if len(record.Evidence) == 0 {
		return errors.New("decision record requires at least one evidence reference")
	}
	for index, reference := range record.Evidence {
		if err := reference.Validate(); err != nil {
			return fmt.Errorf("decision evidence %d: %w", index, err)
		}
	}
	if record.Supersedes != "" && !validIdentifier(record.Supersedes) {
		return errors.New("decision record supersedes identifier is invalid")
	}
	if record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) || record.ExpiresAt.Sub(record.CreatedAt) > MaxTTL {
		return errors.New("decision record retention period is invalid")
	}
	if record.ReviewedAt != nil {
		if record.ReviewedAt.IsZero() || record.ReviewedAt.Before(record.CreatedAt) {
			return errors.New("decision record review time is invalid")
		}
	}
	contentKey, err := ContentKey(record.Project, record.Owner, record.Kind, record.Context, record.Decision, record.Rationale, record.Evidence)
	if err != nil {
		return err
	}
	if record.ID != RecordID(record.Project, record.Owner, record.Kind, contentKey, record.CreatedAt) {
		return errors.New("decision record ID does not match scope, content and creation time")
	}
	return nil
}

// validateText 校验决策正文：长度有界、不得包含凭据式内容、绝对路径、家目录
// 引用或路径跳转片段。任一命中即失败关闭，不做静默改写，避免正文被悄悄
// 篡改后仍冒充原始结论。
func validateText(field, value string, maxRunes int) error {
	if value == "" {
		return fmt.Errorf("decision %s is required", field)
	}
	if len([]rune(value)) > maxRunes {
		return fmt.Errorf("decision %s exceeds %d characters", field, maxRunes)
	}
	if containsControlRune(value) {
		return fmt.Errorf("decision %s contains control characters", field)
	}
	if containsCredentialMaterial(value) {
		return fmt.Errorf("decision %s contains credential-like material", field)
	}
	if containsAbsolutePath(value) {
		return fmt.Errorf("decision %s contains an absolute or escaping path", field)
	}
	return nil
}

// containsControlRune 判断文本是否含有除换行与制表符以外的控制字符。
func containsControlRune(value string) bool {
	for _, character := range value {
		if character == '\n' || character == '\t' {
			continue
		}
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

// validIdentifier 仅接受固定 ASCII 标识，阻断路径分隔符、空白与自由文本。
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

// validDigest 仅接受带 sha256 前缀的完整不可逆摘要。
func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	hexDigest := strings.TrimPrefix(value, "sha256:")
	if len(hexDigest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hexDigest)
	return err == nil
}
