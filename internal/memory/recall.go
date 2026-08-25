// Package memory 定义可信记忆控制面的受限值合同；本文件补充 M2-A 的只读召回
// 计划与临时证据账本，绝不承载 prompt、模型文本、路径或制品正文。
//
// Author: fxbin
package memory

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fxbin/cairn/internal/contract"
)

const (
	// RecallSchemaVersion 是 RecallPlan 与 EvidenceLedger 的固定结构版本。
	RecallSchemaVersion = 1
	// RecallStrategySQLiteExact 是当前唯一允许的确定性 SQLite 精确检索策略。
	RecallStrategySQLiteExact = "sqlite_exact"
	// RecallPrecisionCompact 只请求最小候选事实和证据定位。
	RecallPrecisionCompact = "compact"
	// RecallPrecisionTrace 请求候选的完整受限证据定位，但不增加来源正文。
	RecallPrecisionTrace = "trace"
	// DefaultCompactRecallLimit 限制紧凑投影的默认和最大候选数，控制上下文暴露面。
	DefaultCompactRecallLimit = 10
	// MaxCompactRecallLimit 是紧凑投影的硬上限，不能由调用方提高。
	MaxCompactRecallLimit = 10
	// DefaultTraceRecallLimit 是 trace 投影的默认候选数。
	DefaultTraceRecallLimit = DefaultRecallLimit
	// MaxTraceRecallLimit 是 trace 投影的硬上限。
	MaxTraceRecallLimit = MaxRecallLimit
	// TrustedVerificationAny 不对候选的可信验证标记施加过滤。
	TrustedVerificationAny = "any"
	// TrustedVerificationRequired 只接受可信验证为真的候选。
	TrustedVerificationRequired = "required"
	// TrustedVerificationNotRequired 只接受可信验证为假的候选，不推断其失败原因。
	TrustedVerificationNotRequired = "not_required"
	// RecallStopSufficient 表示计划具有可执行的精确范围。
	RecallStopSufficient = "sufficient"
	// RecallStopNoMatch 表示精确范围已执行但没有候选。
	RecallStopNoMatch = "no_match"
	// RecallStopEvidenceInsufficient 表示缺少可验证范围，不能扩大为全库最近记录。
	RecallStopEvidenceInsufficient = "evidence_insufficient"
	// RecallStopMaxRemediationReached 保留给未来受限补救轮次的停止状态。
	RecallStopMaxRemediationReached = "max_remediation_reached"
	// RecallStopSourceInvalid 表示 SQLite、Journal、Event 或 Artifact 来源校验失败。
	RecallStopSourceInvalid = "source_invalid"
	// LedgerStatusSufficient 表示账本中的候选和来源均已验证。
	LedgerStatusSufficient = "sufficient"
	// LedgerStatusInsufficient 表示缺少查询范围或没有精确命中。
	LedgerStatusInsufficient = "insufficient"
	// LedgerStatusSourceInvalid 表示来源不可信，账本不得包含候选条目。
	LedgerStatusSourceInvalid = "source_invalid"
	// EvidenceSlotRequired 表示账本构建前必须从既有来源取得的证据槽位。
	EvidenceSlotRequired = "required"
	// EvidenceSlotAvailable 表示槽位已由精确来源提供。
	EvidenceSlotAvailable = "available"
	// EvidenceSlotUnavailable 表示 M1 事实不能推出该槽位。
	EvidenceSlotUnavailable = "unavailable"
	// LedgerSourceEventJournal 表示证据定位直接来自已校验 Event Journal。
	LedgerSourceEventJournal = "event_journal"
	// LedgerSourceArtifactReference 表示证据定位还包含同一 Event 声明的 Artifact 摘要。
	LedgerSourceArtifactReference = "artifact_reference"
	// EventReferenceCountKind 明确计数的是去重 EventEvidenceRef，而非语义独立来源。
	EventReferenceCountKind = "event_reference_count"
	// StructuralConflictKind 是同一精确范围内候选事实不一致的受限冲突类型。
	StructuralConflictKind = "structural_conflict"
)

// RecallQuery 是一次临时、封闭的检索输入。它只接受当前工作区摘要、精确任务摘要、
// 受限枚举和 UTC 时间窗口，不能携带问题正文或任意检索文本。
type RecallQuery struct {
	WorkspaceHash       string     `json:"workspace_hash"`
	TaskPromptDigest    string     `json:"task_prompt_digest,omitempty"`
	Outcomes            []string   `json:"outcomes,omitempty"`
	FailureClasses      []string   `json:"failure_classes,omitempty"`
	TrustedVerification string     `json:"trusted_verification"`
	CreatedFrom         *time.Time `json:"created_from,omitempty"`
	CreatedUntil        *time.Time `json:"created_until,omitempty"`
	Limit               int        `json:"limit"`
	Precision           string     `json:"precision"`
}

// EvidenceSlot 表示一个受限证据槽位的需求或可用状态，不记录来源正文。
type EvidenceSlot struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// RecallPlan 是 RecallQuery 的确定性规范化结果；M2-A 的策略恒为 sqlite_exact，
// 且不会写入数据库、索引或运行状态。
type RecallPlan struct {
	SchemaVersion     int            `json:"schema_version"`
	Strategy          string         `json:"strategy"`
	NormalizedRequest RecallQuery    `json:"normalized_request"`
	RetrievalRound    int            `json:"retrieval_round"`
	EvidenceSlots     []EvidenceSlot `json:"evidence_slots"`
	StopReason        string         `json:"stop_reason"`
}

// LedgerGap 标注无法由 M1 固定事实推出的证据槽位或查询范围，避免以推测补全。
type LedgerGap struct {
	CandidateID string `json:"candidate_id,omitempty"`
	Slot        string `json:"slot"`
	Status      string `json:"status"`
}

// LedgerEvidence 是对单个已校验 EventEvidenceRef 的临时脱敏投影；不包含 Run ID、
// 路径、制品预览或 Event payload。
type LedgerEvidence struct {
	Source         string    `json:"source"`
	EventSeq       uint64    `json:"event_seq"`
	EventHash      string    `json:"event_hash"`
	EventType      string    `json:"event_type"`
	ArtifactSHA256 string    `json:"artifact_sha256,omitempty"`
	EventTime      time.Time `json:"event_time"`
}

// LedgerEntry 是一个已校验 Candidate 的一次性解释投影；其字段均来自 Candidate
// 或精确匹配的 Journal Event，不能成为新的记忆事实源。
type LedgerEntry struct {
	CandidateID                string           `json:"candidate_id"`
	Kind                       string           `json:"kind"`
	TaskPromptDigest           string           `json:"task_prompt_digest"`
	Outcome                    string           `json:"outcome"`
	FailureClass               string           `json:"failure_class,omitempty"`
	TrustedVerification        bool             `json:"trusted_verification"`
	MentionTime                time.Time        `json:"mention_time"`
	ExpiresAt                  time.Time        `json:"expires_at"`
	Status                     string           `json:"status"`
	SupersedesID               string           `json:"supersedes_id,omitempty"`
	VerificationTime           *time.Time       `json:"verification_time,omitempty"`
	VerificationTimeStatus     string           `json:"verification_time_status"`
	Evidence                   []LedgerEvidence `json:"evidence"`
	EventReferenceCount        int              `json:"event_reference_count"`
	EventReferenceCountKind    string           `json:"event_reference_count_kind"`
	DerivedTrustedVerification string           `json:"derived_trusted_verification"`
}

// StructuralConflict 保留同一精确范围内全部互相矛盾的候选与证据定位；Resolved 恒为
// false，召回器绝不按时间、相似度或可信标记裁决。
type StructuralConflict struct {
	Kind         string           `json:"kind"`
	CandidateIDs []string         `json:"candidate_ids"`
	Evidence     []LedgerEvidence `json:"evidence"`
	Resolved     bool             `json:"resolved"`
}

// EvidenceLedger 是一次性、只读、可序列化的运行上下文投影；它不写回 Memory、
// Journal、Artifact 或 Agent Loop。
type EvidenceLedger struct {
	SchemaVersion int                  `json:"schema_version"`
	Plan          RecallPlan           `json:"plan"`
	Entries       []LedgerEntry        `json:"entries"`
	Gaps          []LedgerGap          `json:"gaps"`
	Conflicts     []StructuralConflict `json:"conflicts"`
	Status        string               `json:"status"`
}

// Normalize 复制并校验 RecallQuery 的受限字段，规范化枚举顺序和默认值；它不将
// digest 解释成语义，也不因范围不足扩展检索条件。
func (query RecallQuery) Normalize() (RecallQuery, error) {
	if !validDigest(query.WorkspaceHash) {
		return RecallQuery{}, errors.New("memory recall workspace hash is invalid")
	}
	if query.TaskPromptDigest != "" && !validDigest(query.TaskPromptDigest) {
		return RecallQuery{}, errors.New("memory recall task digest is invalid")
	}
	outcomes, err := normalizeOutcomes(query.Outcomes)
	if err != nil {
		return RecallQuery{}, err
	}
	failureClasses, err := normalizeFailureClasses(query.FailureClasses)
	if err != nil {
		return RecallQuery{}, err
	}
	trustedVerification := query.TrustedVerification
	if trustedVerification == "" {
		trustedVerification = TrustedVerificationAny
	}
	if trustedVerification != TrustedVerificationAny && trustedVerification != TrustedVerificationRequired && trustedVerification != TrustedVerificationNotRequired {
		return RecallQuery{}, errors.New("memory recall trusted verification filter is invalid")
	}
	createdFrom, createdUntil, err := normalizeTimeWindow(query.CreatedFrom, query.CreatedUntil)
	if err != nil {
		return RecallQuery{}, err
	}
	precision := query.Precision
	if precision == "" {
		precision = RecallPrecisionCompact
	}
	if precision != RecallPrecisionCompact && precision != RecallPrecisionTrace {
		return RecallQuery{}, errors.New("memory recall precision is invalid")
	}
	limit, err := normalizePrecisionLimit(query.Limit, precision)
	if err != nil {
		return RecallQuery{}, err
	}
	return RecallQuery{
		WorkspaceHash:       query.WorkspaceHash,
		TaskPromptDigest:    query.TaskPromptDigest,
		Outcomes:            outcomes,
		FailureClasses:      failureClasses,
		TrustedVerification: trustedVerification,
		CreatedFrom:         createdFrom,
		CreatedUntil:        createdUntil,
		Limit:               limit,
		Precision:           precision,
	}, nil
}

// BuildRecallPlan 生成 M2-A 的唯一 sqlite_exact 计划。若范围不足，返回带
// evidence_insufficient 停止理由的计划而不是按“最近记录”猜测。
func BuildRecallPlan(query RecallQuery) (RecallPlan, error) {
	normalized, err := query.Normalize()
	if err != nil {
		return RecallPlan{}, err
	}
	scopeStatus := EvidenceSlotAvailable
	stopReason := RecallStopSufficient
	if !normalized.hasEvidenceScope() {
		scopeStatus = EvidenceSlotUnavailable
		stopReason = RecallStopEvidenceInsufficient
	}
	return RecallPlan{
		SchemaVersion:     RecallSchemaVersion,
		Strategy:          RecallStrategySQLiteExact,
		NormalizedRequest: normalized,
		RetrievalRound:    0,
		EvidenceSlots: []EvidenceSlot{
			{Name: "query_scope", Status: scopeStatus},
			{Name: "terminal_event", Status: EvidenceSlotRequired},
			{Name: "verification_state", Status: EvidenceSlotRequired},
			{Name: "artifact_reference", Status: EvidenceSlotRequired},
		},
		StopReason: stopReason,
	}, nil
}

// NewInsufficientLedger 构造没有候选的失败关闭账本，用于范围不足或精确无命中。
func NewInsufficientLedger(plan RecallPlan, stopReason string) EvidenceLedger {
	gapSlot := "query_scope"
	if stopReason == RecallStopNoMatch {
		gapSlot = "exact_match"
	}
	plan.StopReason = stopReason
	return EvidenceLedger{
		SchemaVersion: RecallSchemaVersion,
		Plan:          plan,
		Entries:       []LedgerEntry{},
		Gaps:          []LedgerGap{{Slot: gapSlot, Status: EvidenceSlotUnavailable}},
		Conflicts:     []StructuralConflict{},
		Status:        LedgerStatusInsufficient,
	}
}

// NewSourceInvalidLedger 构造没有候选的来源损坏账本；调用者仍应保留原始错误用于
// 稳定退出码，但不得把错误正文写进账本。
func NewSourceInvalidLedger(plan RecallPlan) EvidenceLedger {
	plan.StopReason = RecallStopSourceInvalid
	return EvidenceLedger{
		SchemaVersion: RecallSchemaVersion,
		Plan:          plan,
		Entries:       []LedgerEntry{},
		Gaps:          []LedgerGap{{Slot: "source_integrity", Status: EvidenceSlotUnavailable}},
		Conflicts:     []StructuralConflict{},
		Status:        LedgerStatusSourceInvalid,
	}
}

// NewEvidenceLedger 汇总已验证的候选条目并显式保留结构化冲突；空条目使用 no_match
// 的不足账本，避免将“无结果”包装成成功。
func NewEvidenceLedger(plan RecallPlan, entries []LedgerEntry) EvidenceLedger {
	if len(entries) == 0 {
		return NewInsufficientLedger(plan, RecallStopNoMatch)
	}
	entries = append([]LedgerEntry(nil), entries...)
	gaps := ledgerGaps(entries)
	entries = projectLedgerEntries(entries, plan.NormalizedRequest.Precision)
	conflicts := DetectStructuralConflicts(entries)
	plan.EvidenceSlots = resolveEvidenceSlots(plan.EvidenceSlots, gaps)
	plan.StopReason = RecallStopSufficient
	return EvidenceLedger{
		SchemaVersion: RecallSchemaVersion,
		Plan:          plan,
		Entries:       entries,
		Gaps:          gaps,
		Conflicts:     conflicts,
		Status:        LedgerStatusSufficient,
	}
}

// DetectStructuralConflicts 在同一精确查询范围内按 task digest 检查 outcome、
// failure class 与可信验证标记的矛盾，并保留全部候选而不选择胜者。
func DetectStructuralConflicts(entries []LedgerEntry) []StructuralConflict {
	byDigest := make(map[string][]LedgerEntry)
	for _, entry := range entries {
		byDigest[entry.TaskPromptDigest] = append(byDigest[entry.TaskPromptDigest], entry)
	}
	digests := make([]string, 0, len(byDigest))
	for digest := range byDigest {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	conflicts := make([]StructuralConflict, 0)
	for _, digest := range digests {
		group := byDigest[digest]
		if len(group) < 2 || !hasStructuralConflict(group) {
			continue
		}
		candidateIDs := make([]string, 0, len(group))
		evidence := make([]LedgerEvidence, 0)
		for _, entry := range group {
			candidateIDs = append(candidateIDs, entry.CandidateID)
			evidence = append(evidence, entry.Evidence...)
		}
		conflicts = append(conflicts, StructuralConflict{
			Kind:         StructuralConflictKind,
			CandidateIDs: candidateIDs,
			Evidence:     deduplicateEvidence(evidence),
			Resolved:     false,
		})
	}
	return conflicts
}

// hasEvidenceScope 判断请求是否至少给出一种可以审计的精确范围；Limit 与 Precision
// 本身不是范围，不能授权全库“最近记录”检索。
func (query RecallQuery) hasEvidenceScope() bool {
	return query.TaskPromptDigest != "" || len(query.Outcomes) != 0 || len(query.FailureClasses) != 0 || query.TrustedVerification != TrustedVerificationAny || query.CreatedFrom != nil
}

// normalizeOutcomes 去重并排序固定 outcome 枚举，拒绝任意自由文本值。
func normalizeOutcomes(outcomes []string) ([]string, error) {
	seen := make(map[string]struct{}, len(outcomes))
	for _, outcome := range outcomes {
		if outcome != "completed" && outcome != "failed" && outcome != "cancelled" {
			return nil, fmt.Errorf("memory recall outcome %q is invalid", outcome)
		}
		seen[outcome] = struct{}{}
	}
	return sortedKeys(seen), nil
}

// normalizeFailureClasses 去重并排序既有终态分类词表，拒绝未由 Run 合同产生的 token。
func normalizeFailureClasses(failureClasses []string) ([]string, error) {
	seen := make(map[string]struct{}, len(failureClasses))
	for _, failureClass := range failureClasses {
		if !validFailureClass(failureClass) {
			return nil, errors.New("memory recall failure class is invalid")
		}
		seen[failureClass] = struct{}{}
	}
	return sortedKeys(seen), nil
}

// normalizePrecisionLimit 根据投影精度选择固定默认值和上限，避免 compact 请求通过
// 大 limit 旁路最小暴露边界。
func normalizePrecisionLimit(limit int, precision string) (int, error) {
	if precision == "" {
		precision = RecallPrecisionCompact
	}
	var defaultLimit, maxLimit int
	switch precision {
	case RecallPrecisionCompact:
		defaultLimit, maxLimit = DefaultCompactRecallLimit, MaxCompactRecallLimit
	case RecallPrecisionTrace:
		defaultLimit, maxLimit = DefaultTraceRecallLimit, MaxTraceRecallLimit
	default:
		return 0, errors.New("memory recall precision is invalid")
	}
	if limit == 0 {
		return defaultLimit, nil
	}
	if limit < 0 || limit > maxLimit {
		return 0, fmt.Errorf("memory recall limit must be within 1 and %d for %s precision", maxLimit, precision)
	}
	return limit, nil
}

// validFailureClass 限制 M2-A 过滤器为当前终态分类会写入的固定词表；它不改变
// M1 已持久化 Candidate 的校验规则，只阻止查询入口接受任意 token。
func validFailureClass(value string) bool {
	return contract.IsKnownTerminalFailureClass(value)
}

// normalizeTimeWindow 只接受完整的 UTC 半开区间，避免调用方用单端时间扩大检索。
func normalizeTimeWindow(createdFrom, createdUntil *time.Time) (*time.Time, *time.Time, error) {
	if (createdFrom == nil) != (createdUntil == nil) {
		return nil, nil, errors.New("memory recall time window requires both bounds")
	}
	if createdFrom == nil {
		return nil, nil, nil
	}
	if createdFrom.IsZero() || createdUntil.IsZero() || createdFrom.Location() != time.UTC || createdUntil.Location() != time.UTC || !createdFrom.Before(*createdUntil) {
		return nil, nil, errors.New("memory recall time window must be an ordered UTC interval")
	}
	from := createdFrom.UTC()
	until := createdUntil.UTC()
	return &from, &until, nil
}

// sortedKeys 将封闭 token 集合转换为稳定顺序，保证同一结构化查询的计划可复现。
func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	return keys
}

// hasStructuralConflict 判断同一 digest 的候选是否在允许的结构化事实中不一致。
func hasStructuralConflict(entries []LedgerEntry) bool {
	first := entries[0]
	for _, entry := range entries[1:] {
		if entry.Outcome != first.Outcome || entry.FailureClass != first.FailureClass || entry.TrustedVerification != first.TrustedVerification {
			return true
		}
	}
	return false
}

// deduplicateEvidence 保持先出现顺序去重 EventEvidenceRef 投影，明确计数不把同一
// Event 的多个 Artifact 引用误称为独立来源。
func deduplicateEvidence(evidence []LedgerEvidence) []LedgerEvidence {
	seen := make(map[string]struct{}, len(evidence))
	result := make([]LedgerEvidence, 0, len(evidence))
	for _, item := range evidence {
		key := fmt.Sprintf("%d:%s:%s", item.EventSeq, item.EventHash, item.EventType)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

// CountEventReferences 按 EventEvidenceRef 的事件身份计算去重计数；同一事件的多个
// Artifact 摘要不增加计数，且该值绝不表示语义独立来源数量。
func CountEventReferences(evidence []LedgerEvidence) int {
	return len(deduplicateEvidence(evidence))
}

// ledgerGaps 汇总每个候选无法由 M1 精确事实推出的验证和制品槽位，避免成功账本
// 将 unavailable 隐藏为字段缺省。
func ledgerGaps(entries []LedgerEntry) []LedgerGap {
	gaps := make([]LedgerGap, 0)
	for _, entry := range entries {
		if entry.VerificationTimeStatus == EvidenceSlotUnavailable {
			gaps = append(gaps, LedgerGap{CandidateID: entry.CandidateID, Slot: "verification_time", Status: EvidenceSlotUnavailable})
		}
		hasArtifact := false
		for _, evidence := range entry.Evidence {
			if evidence.ArtifactSHA256 != "" {
				hasArtifact = true
				break
			}
		}
		if !hasArtifact {
			gaps = append(gaps, LedgerGap{CandidateID: entry.CandidateID, Slot: "artifact_reference", Status: EvidenceSlotUnavailable})
		}
	}
	return gaps
}

// projectLedgerEntries 按已规范化精度复制条目；compact 只保留计数和候选事实，
// trace 才暴露完整的已验证 Evidence 定位。
func projectLedgerEntries(entries []LedgerEntry, precision string) []LedgerEntry {
	projected := append([]LedgerEntry(nil), entries...)
	if precision != RecallPrecisionCompact {
		return projected
	}
	for index := range projected {
		projected[index].Evidence = []LedgerEvidence{}
	}
	return projected
}

// resolveEvidenceSlots 将账本中明确的 unavailable 槽位回写到计划投影，不增加任何
// 新事实；terminal_event 已在构建条目前精确验证，因此始终可用。
func resolveEvidenceSlots(slots []EvidenceSlot, gaps []LedgerGap) []EvidenceSlot {
	resolved := append([]EvidenceSlot(nil), slots...)
	for index := range resolved {
		if resolved[index].Name == "terminal_event" {
			resolved[index].Status = EvidenceSlotAvailable
			continue
		}
		for _, gap := range gaps {
			if resolved[index].Name == gap.Slot || (resolved[index].Name == "verification_state" && gap.Slot == "verification_time") {
				resolved[index].Status = EvidenceSlotUnavailable
				break
			}
		}
		if resolved[index].Status == EvidenceSlotRequired {
			resolved[index].Status = EvidenceSlotAvailable
		}
	}
	return resolved
}
