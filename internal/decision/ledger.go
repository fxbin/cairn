// Package decision 补充决策账本的进程内实现：受控录入、状态迁移与只读查询
// 都在此完成。账本不写 Event Journal、不落数据库、不接入 Agent Loop，因此
// 不能成为运行事实源；所有查询都按项目与主体作用域过滤。
//
// Author: fxbin
package decision

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// HumanApproval 是人工确认凭据。只有携带人工来源的确认才能把候选提升为
// 现行决策；模型只能提出候选。本类型本身不构成来源证明：确认必须来自
// 面向人的入口，来源保证由应用层授权链承担。
type HumanApproval struct {
	// Approver 是确认人的受限标识。
	Approver string
	// At 是确认时刻。
	At time.Time
}

// NewHumanApproval 构造一次人工确认凭据；确认人与时刻都不接受空值。
func NewHumanApproval(approver string, at time.Time) (HumanApproval, error) {
	if !validIdentifier(approver) {
		return HumanApproval{}, errors.New("decision approval requires a valid approver identifier")
	}
	if at.IsZero() {
		return HumanApproval{}, errors.New("decision approval requires an explicit time")
	}
	return HumanApproval{Approver: approver, At: at.UTC()}, nil
}

// Query 是一次只读检索的边界条件。它只接受项目与主体作用域、种类与状态
// 过滤和条数上限，不接受检索文本或跨项目通配。
type Query struct {
	// Project 是检索所属项目；必须与记录项目一致。
	Project string
	// Principal 是发起检索的主体；个人偏好只对该主体可见。
	Principal string
	// Kinds 是可选的种类过滤；为空表示不过滤。
	Kinds []string
	// Statuses 是可选的有效状态过滤；为空表示返回作用域内的全部记录，
	// 其中已过期记录受 IncludeExpired 控制。
	Statuses []string
	// Limit 是返回条数上限；零值取 DefaultQueryLimit。
	Limit int
	// IncludeExpired 为真时同时返回已过期记录，用于历史追溯。
	IncludeExpired bool
	// Now 是判定保留期是否耗尽的时刻。
	Now time.Time
}

// Ledger 是决策账本的进程内实现。它保存记录与其审计行，并保证同项目同
// 主体下的等价决策不会重复占用现行作用域。
type Ledger struct {
	mutex    sync.RWMutex
	records  []DecisionRecord
	audits   []AuditEntry
	byID     map[string]int
	byDigest map[string]int
}

// NewLedger 创建一个空账本。
func NewLedger() *Ledger {
	return &Ledger{byID: map[string]int{}, byDigest: map[string]int{}}
}

// Propose 录入一条候选决策。内容与既有候选等价时返回该候选，实现重试幂等；
// 与现行记录等价时失败关闭，防止同一结论重复占用现行作用域。
func (ledger *Ledger) Propose(proposal Proposal) (DecisionRecord, error) {
	record, err := NewProposal(proposal)
	if err != nil {
		return DecisionRecord{}, err
	}
	digest, err := ContentKey(record.Project, record.Owner, record.Kind, record.Context, record.Decision, record.Rationale, record.Evidence)
	if err != nil {
		return DecisionRecord{}, err
	}
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	if index, found := ledger.byDigest[digest]; found {
		existing := ledger.records[index]
		switch existing.EffectiveStatus(proposal.Now) {
		case StatusProposed:
			return existing, nil
		case StatusActive:
			return DecisionRecord{}, fmt.Errorf("%w: %s", ErrDuplicateDecision, existing.ID)
		}
	}
	ledger.records = append(ledger.records, record)
	ledger.byID[record.ID] = len(ledger.records) - 1
	ledger.byDigest[digest] = len(ledger.records) - 1
	ledger.audits = append(ledger.audits, AuditEntry{
		RecordID:   record.ID,
		Action:     ActionProposed,
		Actor:      record.Owner,
		OccurredAt: record.CreatedAt,
	})
	return record, nil
}

// Confirm 把候选确认为现行决策。确认必须携带人工来源；若记录声明了替代
// 目标，被替代记录同时转为已被替代，作用域必须一致且当前处于现行状态。
func (ledger *Ledger) Confirm(id string, approval HumanApproval) (DecisionRecord, error) {
	if approval.Approver == "" || approval.At.IsZero() {
		return DecisionRecord{}, fmt.Errorf("%w: missing approver or approval time", ErrNotHumanApproval)
	}
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	index, found := ledger.byID[id]
	if !found {
		return DecisionRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	record := ledger.records[index]
	if err := ValidTransition(record.EffectiveStatus(approval.At), StatusActive); err != nil {
		return DecisionRecord{}, err
	}
	if record.Supersedes != "" {
		targetIndex, targetFound := ledger.byID[record.Supersedes]
		if !targetFound {
			return DecisionRecord{}, fmt.Errorf("%w: superseded record %s", ErrNotFound, record.Supersedes)
		}
		target := ledger.records[targetIndex]
		if target.Project != record.Project {
			return DecisionRecord{}, fmt.Errorf("%w: superseded record belongs to another project", ErrConflict)
		}
		if err := ValidTransition(target.EffectiveStatus(approval.At), StatusSuperseded); err != nil {
			return DecisionRecord{}, err
		}
	}
	record.Status = StatusActive
	reviewedAt := approval.At.UTC()
	record.ReviewedAt = &reviewedAt
	ledger.records[index] = record
	ledger.audits = append(ledger.audits, AuditEntry{
		RecordID:   record.ID,
		Action:     ActionConfirmed,
		Actor:      approval.Approver,
		OccurredAt: reviewedAt,
	})
	if record.Supersedes != "" {
		targetIndex := ledger.byID[record.Supersedes]
		target := ledger.records[targetIndex]
		target.Status = StatusSuperseded
		ledger.records[targetIndex] = target
		ledger.audits = append(ledger.audits, AuditEntry{
			RecordID:   target.ID,
			Action:     ActionSuperseded,
			Reason:     record.ID,
			Actor:      approval.Approver,
			OccurredAt: reviewedAt,
		})
	}
	return record, nil
}

// Review 复核一条现行决策：刷新复核时间并按新保留期延长有效期。被替代、
// 撤销、拒绝或已过期的记录不可复核。
func (ledger *Ledger) Review(id string, approval HumanApproval, ttl time.Duration) (DecisionRecord, error) {
	if approval.Approver == "" || approval.At.IsZero() {
		return DecisionRecord{}, fmt.Errorf("%w: missing approver or approval time", ErrNotHumanApproval)
	}
	normalized, err := NormalizeTTL(ttl)
	if err != nil {
		return DecisionRecord{}, err
	}
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	index, found := ledger.byID[id]
	if !found {
		return DecisionRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	record := ledger.records[index]
	if record.EffectiveStatus(approval.At) != StatusActive {
		return DecisionRecord{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, record.EffectiveStatus(approval.At), StatusActive)
	}
	reviewedAt := approval.At.UTC()
	record.ReviewedAt = &reviewedAt
	record.ExpiresAt = reviewedAt.Add(normalized)
	if err := record.Validate(); err != nil {
		return DecisionRecord{}, err
	}
	ledger.records[index] = record
	ledger.audits = append(ledger.audits, AuditEntry{
		RecordID:   record.ID,
		Action:     ActionReviewed,
		Actor:      approval.Approver,
		OccurredAt: reviewedAt,
	})
	return record, nil
}

// Revoke 撤销一条记录。撤销理由只接受封闭词表；记录本身保留在账本中，
// 历史、来源与作废链都可追溯，撤销后的记录不再代表现行事实。
func (ledger *Ledger) Revoke(id, reason string, at time.Time) (DecisionRecord, error) {
	if !validRevokeReason(reason) {
		return DecisionRecord{}, fmt.Errorf("decision revocation reason %q is invalid", reason)
	}
	if at.IsZero() {
		return DecisionRecord{}, errors.New("decision revocation requires an explicit time")
	}
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	index, found := ledger.byID[id]
	if !found {
		return DecisionRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	record := ledger.records[index]
	if err := ValidTransition(record.EffectiveStatus(at), StatusRevoked); err != nil {
		return DecisionRecord{}, err
	}
	record.Status = StatusRevoked
	ledger.records[index] = record
	ledger.audits = append(ledger.audits, AuditEntry{
		RecordID:   record.ID,
		Action:     ActionRevoked,
		Reason:     reason,
		OccurredAt: at.UTC(),
	})
	return record, nil
}

// Reject 拒绝一条候选。拒绝理由只接受封闭词表；被拒绝的候选不可再确认。
func (ledger *Ledger) Reject(id, reason string, at time.Time) (DecisionRecord, error) {
	if reason != RejectReasonNotApproved {
		return DecisionRecord{}, fmt.Errorf("decision rejection reason %q is invalid", reason)
	}
	if at.IsZero() {
		return DecisionRecord{}, errors.New("decision rejection requires an explicit time")
	}
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	index, found := ledger.byID[id]
	if !found {
		return DecisionRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	record := ledger.records[index]
	if err := ValidTransition(record.EffectiveStatus(at), StatusRejected); err != nil {
		return DecisionRecord{}, err
	}
	record.Status = StatusRejected
	ledger.records[index] = record
	ledger.audits = append(ledger.audits, AuditEntry{
		RecordID:   record.ID,
		Action:     ActionRejected,
		Reason:     reason,
		OccurredAt: at.UTC(),
	})
	return record, nil
}

// Get 返回指定记录；不存在时返回 ErrNotFound。
func (ledger *Ledger) Get(id string) (DecisionRecord, error) {
	ledger.mutex.RLock()
	defer ledger.mutex.RUnlock()
	index, found := ledger.byID[id]
	if !found {
		return DecisionRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return ledger.records[index], nil
}

// Query 只读检索当前作用域内的决策。跨项目与跨主体的记录一律不可见；
// 已过期记录默认不返回，避免过期结论冒充现行事实。
func (ledger *Ledger) Query(query Query) ([]DecisionRecord, error) {
	if !validIdentifier(query.Project) {
		return nil, errors.New("decision query requires a valid project identifier")
	}
	if !validIdentifier(query.Principal) {
		return nil, errors.New("decision query requires a valid principal identifier")
	}
	if query.Now.IsZero() {
		return nil, errors.New("decision query requires an explicit time")
	}
	kinds, err := normalizeKinds(query.Kinds)
	if err != nil {
		return nil, err
	}
	statuses, err := normalizeStatuses(query.Statuses)
	if err != nil {
		return nil, err
	}
	limit, err := normalizeLimit(query.Limit)
	if err != nil {
		return nil, err
	}
	ledger.mutex.RLock()
	defer ledger.mutex.RUnlock()
	matched := make([]DecisionRecord, 0, limit)
	for _, record := range ledger.records {
		if !record.VisibleTo(query.Project, query.Principal) {
			continue
		}
		if len(kinds) > 0 && !containsString(kinds, record.Kind) {
			continue
		}
		status := record.EffectiveStatus(query.Now)
		if len(statuses) > 0 {
			if !containsString(statuses, status) {
				continue
			}
		} else if status == StatusExpired && !query.IncludeExpired {
			continue
		}
		matched = append(matched, record)
	}
	sort.SliceStable(matched, func(left, right int) bool {
		leftTime := recordSortTime(matched[left])
		rightTime := recordSortTime(matched[right])
		if !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		return matched[left].ID < matched[right].ID
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

// Audits 返回指定记录的脱敏审计历史，按发生顺序排列。
func (ledger *Ledger) Audits(id string) []AuditEntry {
	ledger.mutex.RLock()
	defer ledger.mutex.RUnlock()
	entries := make([]AuditEntry, 0, len(ledger.audits))
	for _, entry := range ledger.audits {
		if entry.RecordID == id {
			entries = append(entries, entry)
		}
	}
	return entries
}

// recordSortTime 返回记录参与排序的时刻：已复核记录用复核时间，未复核记录
// 用创建时间，使排序在同项目内可复现。
func recordSortTime(record DecisionRecord) time.Time {
	if record.ReviewedAt != nil {
		return record.ReviewedAt.UTC()
	}
	return record.CreatedAt.UTC()
}

// validRevokeReason 判断撤销理由是否属于封闭词表。
func validRevokeReason(reason string) bool {
	switch reason {
	case RevokeReasonEvidenceInvalid, RevokeReasonHumanRequest:
		return true
	default:
		return false
	}
}

// normalizeKinds 规范化种类过滤：去重排序并拒绝未知种类。
func normalizeKinds(kinds []string) ([]string, error) {
	seen := make(map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case KindAuthoritative, KindObservation, KindPreference:
		default:
			return nil, fmt.Errorf("decision query kind %q is invalid", kind)
		}
		seen[kind] = struct{}{}
	}
	normalized := make([]string, 0, len(seen))
	for kind := range seen {
		normalized = append(normalized, kind)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// normalizeStatuses 规范化状态过滤：去重排序并拒绝未知状态。
func normalizeStatuses(statuses []string) ([]string, error) {
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		switch status {
		case StatusProposed, StatusActive, StatusSuperseded, StatusRevoked, StatusRejected, StatusExpired:
		default:
			return nil, fmt.Errorf("decision query status %q is invalid", status)
		}
		seen[status] = struct{}{}
	}
	normalized := make([]string, 0, len(seen))
	for status := range seen {
		normalized = append(normalized, status)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// normalizeLimit 规范只读检索上限，避免调用方以零值绕过资源边界。
func normalizeLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultQueryLimit, nil
	}
	if limit < 0 || limit > MaxQueryLimit {
		return 0, fmt.Errorf("decision query limit must be within 1 and %d", MaxQueryLimit)
	}
	return limit, nil
}

// containsString 判断集合中是否含有给定值。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
