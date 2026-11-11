// Package memory 定义可信记忆控制面的受限值合同；本文件补充 M3 的事务化
// 巩固合同与确定性归纳纯函数。归纳只消费已验证 run_outcome 候选的不可变
// 结构化事实，不接触任务正文、模型输出、路径、制品内容或任何自由文本。
//
// Author: fxbin
package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	// ConsolidationSchemaVersion 是巩固计划、派生候选与载荷合同的当前版本。
	ConsolidationSchemaVersion = 1
	// StrategyTaskOutcomeStability 归纳同一任务摘要下完全一致的终态分布。
	StrategyTaskOutcomeStability = "task_outcome_stability"
	// StrategyFailureClassRecurrence 归纳同一失败类别跨任务的重复出现。
	StrategyFailureClassRecurrence = "failure_class_recurrence"
	// KindPattern 表示已由多条独立情节支撑的可复用规律候选。
	KindPattern = "pattern"
	// KindHypothesis 表示未证明的推断候选，默认不得改变任何运行行为。
	KindHypothesis = "hypothesis"
	// PatternMinSupport 是 pattern 候选允许的最少独立来源数，不可被调用方调低。
	PatternMinSupport = 2
	// HypothesisMinSupport 是 hypothesis 候选允许的最少样本数。
	HypothesisMinSupport = 2
	// HypothesisMinDistinctTasks 要求 hypothesis 覆盖至少两个不同任务摘要。
	HypothesisMinDistinctTasks = 2
	// DerivedStatusActive 表示派生候选尚未被拒绝或替代。
	DerivedStatusActive = "active"
	// DerivedStatusRejected 表示派生候选被显式拒绝。
	DerivedStatusRejected = "rejected"
	// DerivedStatusSuperseded 表示派生候选已被同范围新候选替代。
	DerivedStatusSuperseded = "superseded"
	// DerivedAuditConsolidated 记录派生候选由一次巩固事务写入。
	DerivedAuditConsolidated = "consolidated"
	// DerivedAuditRejected 记录派生候选被显式拒绝。
	DerivedAuditRejected = "rejected"
	// DerivedAuditSuperseded 记录派生候选被替代。
	DerivedAuditSuperseded = "superseded"
	// RejectInsufficientEvidence 表示操作者认定来源证据不再充分。
	RejectInsufficientEvidence = "insufficient_evidence"
	// RejectHumanRequest 表示操作者明确请求拒绝该候选。
	RejectHumanRequest = "human_request"
	// ConsolidationStatusCommitted 表示巩固事务已完整提交。
	ConsolidationStatusCommitted = "committed"
	// consolidationIDPrefix 是巩固事务标识的固定前缀。
	consolidationIDPrefix = "cons_"
	// patternIDPrefix 是 pattern 派生候选标识的固定前缀。
	patternIDPrefix = "pat_"
	// hypothesisIDPrefix 是 hypothesis 派生候选标识的固定前缀。
	hypothesisIDPrefix = "hyp_"
)

// ConsolidationInputs 是一次确定性归纳的完整输入集合。Candidates 必须来自
// 同一 workspace 且均为已通过跨库验证的 run_outcome 候选；调用方负责在巩固
// 时刻筛选 active，本结构不再查看或修改候选状态。
type ConsolidationInputs struct {
	WorkspaceHash string
	Candidates    []Candidate
}

// PatternPayload 是 pattern 派生候选的封闭载荷；每个字段都可由源候选的
// 不可变事实重算得到，验证方据此比对而不信任存储文本。
type PatternPayload struct {
	TaskPromptDigest    string    `json:"task_prompt_digest"`
	Outcome             string    `json:"outcome"`
	FailureClass        string    `json:"failure_class,omitempty"`
	TrustedVerification bool      `json:"trusted_verification"`
	Support             int       `json:"support"`
	FirstCreatedAt      time.Time `json:"first_created_at"`
	LastCreatedAt       time.Time `json:"last_created_at"`
}

// HypothesisPayload 是 hypothesis 派生候选的封闭载荷；它只描述重复出现的
// 结构化事实，不携带结论文本，也不推断根因。
type HypothesisPayload struct {
	FailureClass        string    `json:"failure_class"`
	Support             int       `json:"support"`
	DistinctTaskDigests int       `json:"distinct_task_digests"`
	FirstCreatedAt      time.Time `json:"first_created_at"`
	LastCreatedAt       time.Time `json:"last_created_at"`
}

// SourceCandidateRef 是派生候选的间接证据：只指向源 run_outcome 候选的
// 稳定 ID，不复制其事实或事件定位。
type SourceCandidateRef struct {
	CandidateID string `json:"candidate_id"`
}

// DerivedCandidate 是巩固事务写入的 pattern 或 hypothesis 候选。Payload 是
// 与 Kind 对应的封闭载荷序列化结果，Validate 会按 Kind 解析并全量校验。
type DerivedCandidate struct {
	ID                        string               `json:"id"`
	SchemaVersion             int                  `json:"schema_version"`
	Kind                      string               `json:"kind"`
	ConsolidationID           string               `json:"consolidation_id"`
	WorkspaceHash             string               `json:"workspace_hash"`
	ScopeDigest               string               `json:"scope_digest"`
	Payload                   json.RawMessage      `json:"payload"`
	CreatedAt                 time.Time            `json:"created_at"`
	ExpiresAt                 time.Time            `json:"expires_at"`
	Status                    string               `json:"status"`
	SupersedesID              string               `json:"supersedes_id,omitempty"`
	SupersedesConsolidationID string               `json:"supersedes_consolidation_id,omitempty"`
	Evidence                  []SourceCandidateRef `json:"evidence"`
}

// PlannedConsolidation 是一次单写入者事务的完整计划：固定策略、封存的输入
// 摘要与全部派生候选。存储层在同一 SQLite 事务内提交或整体回滚。
type PlannedConsolidation struct {
	ConsolidationID      string             `json:"consolidation_id"`
	SchemaVersion        int                `json:"schema_version"`
	Strategy             string             `json:"strategy"`
	WorkspaceHash        string             `json:"workspace_hash"`
	InputsDigest         string             `json:"inputs_digest"`
	InputIDs             []string           `json:"input_ids"`
	FactAnchor           time.Time          `json:"fact_anchor"`
	MinPatternSupport    int                `json:"min_pattern_support"`
	MinHypothesisSupport int                `json:"min_hypothesis_support"`
	Derived              []DerivedCandidate `json:"derived"`
}

// ConsolidationOptions 描述一次归纳的受控参数；下限常量不允许被调低，
// TTL 沿用情节记忆的默认值与上限。
type ConsolidationOptions struct {
	Now                  time.Time
	TTL                  time.Duration
	MinPatternSupport    int
	MinHypothesisSupport int
}

// InputsDigest 对有序去重的源候选 ID 集合生成确定性摘要，作为事务的
// preimage 封存；集合相同即摘要相同，与候选状态无关。
func InputsDigest(candidateIDs []string) string {
	unique := make(map[string]struct{}, len(candidateIDs))
	for _, id := range candidateIDs {
		unique[id] = struct{}{}
	}
	ordered := sortedStringSet(unique)
	sum := sha256.Sum256([]byte("cairn-memory-inputs-v1\x00" + joinWithNUL(ordered)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ConsolidationID 由策略、workspace 与输入摘要派生：同一输入集合的重试命中
// 同一巩固事务，输入集合变化产生新事务，天然幂等。
func ConsolidationID(strategy, workspaceHash, inputsDigest string) string {
	sum := sha256.Sum256([]byte("cairn-memory-consolidation-v1\x00" + strategy + "\x00" + workspaceHash + "\x00" + inputsDigest))
	return consolidationIDPrefix + hex.EncodeToString(sum[:])
}

// PatternID 由 workspace 与任务摘要派生，作为同范围 pattern 的稳定替代锚点。
func PatternID(workspaceHash, taskPromptDigest string) string {
	sum := sha256.Sum256([]byte("cairn-memory-pattern-v1\x00" + workspaceHash + "\x00" + taskPromptDigest))
	return patternIDPrefix + hex.EncodeToString(sum[:])
}

// HypothesisID 由 workspace 与失败类别派生，作为同范围 hypothesis 的稳定替代锚点。
func HypothesisID(workspaceHash, failureClass string) string {
	sum := sha256.Sum256([]byte("cairn-memory-hypothesis-v1\x00" + workspaceHash + "\x00" + failureClass))
	return hypothesisIDPrefix + hex.EncodeToString(sum[:])
}

// HypothesisScopeDigest 将 hypothesis 的归纳范围折叠为不可逆摘要。
func HypothesisScopeDigest(workspaceHash, failureClass string) string {
	sum := sha256.Sum256([]byte(workspaceHash + "\x00" + failureClass))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IsDerivedID 判断标识是否属于派生候选（pattern 或 hypothesis）的固定前缀；
// 供展示层路由使用，不参与任何权限或事实判定。
func IsDerivedID(candidateID string) bool {
	return strings.HasPrefix(candidateID, patternIDPrefix) || strings.HasPrefix(candidateID, hypothesisIDPrefix)
}

// ValidRejectionReason 判断派生候选拒绝理由是否属于封闭词表。
func ValidRejectionReason(reason string) bool {
	return reason == RejectInsufficientEvidence || reason == RejectHumanRequest
}

// DeriveConsolidations 从输入候选派生固定策略的巩固计划。它是无 I/O 的
// 纯函数：样本不足的策略不产生计划；派生候选按 scope 稳定排序。派生锚点
// 取输入组内最后创建时间而非调用时钟——同一输入集合恒产生同一计划，重试
// 幂等；保留期自全部输入中最新的候选起算，options.Now 仅用于拒绝时钟回拨。
// 同范围旧候选的替代由存储层事务判定。
func DeriveConsolidations(inputs ConsolidationInputs, options ConsolidationOptions) ([]PlannedConsolidation, error) {
	if err := validateConsolidationInputs(inputs); err != nil {
		return nil, err
	}
	if options.Now.IsZero() {
		return nil, errors.New("consolidation requires an explicit frozen clock")
	}
	latestInput := inputs.Candidates[0].CreatedAt
	for _, candidate := range inputs.Candidates[1:] {
		if candidate.CreatedAt.After(latestInput) {
			latestInput = candidate.CreatedAt
		}
	}
	if options.Now.Before(latestInput) {
		return nil, errors.New("consolidation clock must not predate the newest input candidate")
	}
	ttl, err := NormalizeTTL(options.TTL)
	if err != nil {
		return nil, err
	}
	minPattern := options.MinPatternSupport
	if minPattern == 0 {
		minPattern = PatternMinSupport
	}
	if minPattern < PatternMinSupport {
		return nil, fmt.Errorf("pattern support must be at least %d", PatternMinSupport)
	}
	minHypothesis := options.MinHypothesisSupport
	if minHypothesis == 0 {
		minHypothesis = HypothesisMinSupport
	}
	if minHypothesis < HypothesisMinSupport {
		return nil, fmt.Errorf("hypothesis support must be at least %d", HypothesisMinSupport)
	}
	inputIDs := make([]string, 0, len(inputs.Candidates))
	for _, candidate := range inputs.Candidates {
		inputIDs = append(inputIDs, candidate.ID)
	}
	inputsDigest := InputsDigest(inputIDs)
	plans := make([]PlannedConsolidation, 0, 2)
	if patternPlan, ok := derivePatterns(inputs, inputsDigest, minPattern, minHypothesis, ttl, latestInput); ok {
		plans = append(plans, patternPlan)
	}
	if hypothesisPlan, ok := deriveHypotheses(inputs, inputsDigest, minPattern, minHypothesis, ttl, latestInput); ok {
		plans = append(plans, hypothesisPlan)
	}
	sort.Slice(plans, func(left, right int) bool {
		return plans[left].Strategy < plans[right].Strategy
	})
	return plans, nil
}

// derivePatterns 归纳 task_outcome_stability 计划：同任务摘要下 outcome、
// failure class 与可信验证完全一致且样本足够的组才派生 pattern。
func derivePatterns(inputs ConsolidationInputs, inputsDigest string, minPatternSupport, minHypothesisSupport int, ttl time.Duration, anchor time.Time) (PlannedConsolidation, bool) {
	groups := make(map[string][]Candidate)
	for _, candidate := range inputs.Candidates {
		groups[candidate.TaskPromptDigest] = append(groups[candidate.TaskPromptDigest], candidate)
	}
	derived := make([]DerivedCandidate, 0)
	for _, digest := range sortedStringSet(digestKeySet(groups)) {
		group := groups[digest]
		if len(group) < minPatternSupport || !consistentRunFacts(group) {
			continue
		}
		first, last := createdBounds(group)
		payload, err := json.Marshal(PatternPayload{
			TaskPromptDigest:    digest,
			Outcome:             group[0].Outcome,
			FailureClass:        group[0].FailureClass,
			TrustedVerification: group[0].TrustedVerification,
			Support:             len(group),
			FirstCreatedAt:      first,
			LastCreatedAt:       last,
		})
		if err != nil {
			continue
		}
		consolidationID := ConsolidationID(StrategyTaskOutcomeStability, inputs.WorkspaceHash, inputsDigest)
		derived = append(derived, DerivedCandidate{
			ID:              PatternID(inputs.WorkspaceHash, digest),
			SchemaVersion:   ConsolidationSchemaVersion,
			Kind:            KindPattern,
			ConsolidationID: consolidationID,
			WorkspaceHash:   inputs.WorkspaceHash,
			ScopeDigest:     digest,
			Payload:         payload,
			CreatedAt:       anchor,
			ExpiresAt:       anchor.Add(ttl),
			Status:          DerivedStatusActive,
			Evidence:        sourceReferences(group),
		})
	}
	if len(derived) == 0 {
		return PlannedConsolidation{}, false
	}
	return PlannedConsolidation{
		ConsolidationID:      ConsolidationID(StrategyTaskOutcomeStability, inputs.WorkspaceHash, inputsDigest),
		SchemaVersion:        ConsolidationSchemaVersion,
		Strategy:             StrategyTaskOutcomeStability,
		WorkspaceHash:        inputs.WorkspaceHash,
		InputsDigest:         inputsDigest,
		InputIDs:             inputIDList(inputs),
		FactAnchor:           anchor,
		MinPatternSupport:    minPatternSupport,
		MinHypothesisSupport: minHypothesisSupport,
		Derived:              derived,
	}, true
}

// deriveHypotheses 归纳 failure_class_recurrence 计划：同一失败类别样本足够
// 且覆盖多个任务摘要时派生 hypothesis，载荷显式保持未证明状态。
func deriveHypotheses(inputs ConsolidationInputs, inputsDigest string, minPatternSupport, minHypothesisSupport int, ttl time.Duration, anchor time.Time) (PlannedConsolidation, bool) {
	groups := make(map[string][]Candidate)
	for _, candidate := range inputs.Candidates {
		if candidate.FailureClass == "" {
			continue
		}
		groups[candidate.FailureClass] = append(groups[candidate.FailureClass], candidate)
	}
	derived := make([]DerivedCandidate, 0)
	for _, failureClass := range sortedStringSet(digestKeySet(groups)) {
		group := groups[failureClass]
		distinct := distinctTaskDigests(group)
		if len(group) < minHypothesisSupport || distinct < HypothesisMinDistinctTasks {
			continue
		}
		first, last := createdBounds(group)
		payload, err := json.Marshal(HypothesisPayload{
			FailureClass:        failureClass,
			Support:             len(group),
			DistinctTaskDigests: distinct,
			FirstCreatedAt:      first,
			LastCreatedAt:       last,
		})
		if err != nil {
			continue
		}
		consolidationID := ConsolidationID(StrategyFailureClassRecurrence, inputs.WorkspaceHash, inputsDigest)
		derived = append(derived, DerivedCandidate{
			ID:              HypothesisID(inputs.WorkspaceHash, failureClass),
			SchemaVersion:   ConsolidationSchemaVersion,
			Kind:            KindHypothesis,
			ConsolidationID: consolidationID,
			WorkspaceHash:   inputs.WorkspaceHash,
			ScopeDigest:     HypothesisScopeDigest(inputs.WorkspaceHash, failureClass),
			Payload:         payload,
			CreatedAt:       anchor,
			ExpiresAt:       anchor.Add(ttl),
			Status:          DerivedStatusActive,
			Evidence:        sourceReferences(group),
		})
	}
	if len(derived) == 0 {
		return PlannedConsolidation{}, false
	}
	return PlannedConsolidation{
		ConsolidationID:      ConsolidationID(StrategyFailureClassRecurrence, inputs.WorkspaceHash, inputsDigest),
		SchemaVersion:        ConsolidationSchemaVersion,
		Strategy:             StrategyFailureClassRecurrence,
		WorkspaceHash:        inputs.WorkspaceHash,
		InputsDigest:         inputsDigest,
		InputIDs:             inputIDList(inputs),
		FactAnchor:           anchor,
		MinPatternSupport:    minPatternSupport,
		MinHypothesisSupport: minHypothesisSupport,
		Derived:              derived,
	}, true
}

// Validate 校验派生候选的标识、载荷、证据下限、保留期与状态词表；载荷按
// Kind 解析为封闭结构并全量校验，拒绝任何自由文本或字段缺失。
func (candidate DerivedCandidate) Validate() error {
	if candidate.SchemaVersion != ConsolidationSchemaVersion {
		return fmt.Errorf("unsupported derived candidate schema version %d", candidate.SchemaVersion)
	}
	if candidate.ConsolidationID == "" || !validIdentifier(candidate.ConsolidationID) {
		return errors.New("derived candidate consolidation id is invalid")
	}
	if !validDigest(candidate.WorkspaceHash) {
		return errors.New("derived candidate workspace hash is invalid")
	}
	if !validDigest(candidate.ScopeDigest) {
		return errors.New("derived candidate scope digest is invalid")
	}
	if candidate.CreatedAt.IsZero() || candidate.ExpiresAt.IsZero() || !candidate.ExpiresAt.After(candidate.CreatedAt) || candidate.ExpiresAt.Sub(candidate.CreatedAt) > MaxTTL {
		return errors.New("derived candidate retention period is invalid")
	}
	if (candidate.SupersedesID == "") != (candidate.SupersedesConsolidationID == "") {
		return errors.New("derived candidate supersedes version key is incomplete")
	}
	if candidate.SupersedesID != "" && (!validIdentifier(candidate.SupersedesID) || !validIdentifier(candidate.SupersedesConsolidationID)) {
		return errors.New("derived candidate supersedes version key is invalid")
	}
	switch candidate.Status {
	case DerivedStatusActive, DerivedStatusRejected, DerivedStatusSuperseded:
	default:
		return fmt.Errorf("derived candidate status %q is invalid", candidate.Status)
	}
	switch candidate.Kind {
	case KindPattern:
		return candidate.validatePattern()
	case KindHypothesis:
		return candidate.validateHypothesis()
	default:
		return fmt.Errorf("derived candidate kind %q is invalid", candidate.Kind)
	}
}

// validatePattern 校验 pattern 标识、载荷与证据下限的完整一致性。
func (candidate DerivedCandidate) validatePattern() error {
	var payload PatternPayload
	if err := strictUnmarshal(candidate.Payload, &payload); err != nil {
		return fmt.Errorf("pattern payload is invalid: %w", err)
	}
	if candidate.ID != PatternID(candidate.WorkspaceHash, payload.TaskPromptDigest) || candidate.ScopeDigest != payload.TaskPromptDigest {
		return errors.New("pattern candidate id or scope does not match payload")
	}
	switch payload.Outcome {
	case "completed", "failed", "cancelled":
	default:
		return fmt.Errorf("pattern payload outcome %q is invalid", payload.Outcome)
	}
	if payload.FailureClass != "" && !validToken(payload.FailureClass) {
		return errors.New("pattern payload failure class is invalid")
	}
	if payload.Support < PatternMinSupport || payload.Support != len(candidate.Evidence) {
		return errors.New("pattern support must match evidence count and meet the minimum")
	}
	if payload.FirstCreatedAt.IsZero() || payload.LastCreatedAt.IsZero() || payload.LastCreatedAt.Before(payload.FirstCreatedAt) {
		return errors.New("pattern payload time bounds are invalid")
	}
	return validateSourceReferences(candidate.Evidence)
}

// validateHypothesis 校验 hypothesis 标识、载荷与证据下限的完整一致性。
func (candidate DerivedCandidate) validateHypothesis() error {
	var payload HypothesisPayload
	if err := strictUnmarshal(candidate.Payload, &payload); err != nil {
		return fmt.Errorf("hypothesis payload is invalid: %w", err)
	}
	if candidate.ID != HypothesisID(candidate.WorkspaceHash, payload.FailureClass) || candidate.ScopeDigest != HypothesisScopeDigest(candidate.WorkspaceHash, payload.FailureClass) {
		return errors.New("hypothesis candidate id or scope does not match payload")
	}
	if !validToken(payload.FailureClass) {
		return errors.New("hypothesis payload failure class is invalid")
	}
	if payload.Support < HypothesisMinSupport || payload.Support != len(candidate.Evidence) || payload.DistinctTaskDigests < HypothesisMinDistinctTasks || payload.DistinctTaskDigests > payload.Support {
		return errors.New("hypothesis support must match evidence count and meet the minimums")
	}
	if payload.FirstCreatedAt.IsZero() || payload.LastCreatedAt.IsZero() || payload.LastCreatedAt.Before(payload.FirstCreatedAt) {
		return errors.New("hypothesis payload time bounds are invalid")
	}
	return validateSourceReferences(candidate.Evidence)
}

// validateConsolidationInputs 拒绝空集、跨 workspace 与非 run_outcome 输入；
// 输入候选自身必须已通过 M1 合同校验。
func validateConsolidationInputs(inputs ConsolidationInputs) error {
	if !validDigest(inputs.WorkspaceHash) {
		return errors.New("consolidation workspace hash is invalid")
	}
	if len(inputs.Candidates) == 0 {
		return errors.New("consolidation requires at least one source candidate")
	}
	seen := make(map[string]struct{}, len(inputs.Candidates))
	for index, candidate := range inputs.Candidates {
		if candidate.Kind != KindRunOutcome {
			return fmt.Errorf("consolidation input %d must be a run_outcome candidate", index)
		}
		if candidate.WorkspaceHash != inputs.WorkspaceHash {
			return fmt.Errorf("consolidation input %d belongs to a different workspace", index)
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("consolidation input %d: %w", index, err)
		}
		if _, duplicate := seen[candidate.ID]; duplicate {
			return fmt.Errorf("consolidation input %d duplicates candidate %s", index, candidate.ID)
		}
		seen[candidate.ID] = struct{}{}
	}
	return nil
}

// validateSourceReferences 要求证据非空、标识合法且按 ID 有序去重。
func validateSourceReferences(evidence []SourceCandidateRef) error {
	if len(evidence) == 0 {
		return errors.New("derived candidate requires source evidence")
	}
	seen := make(map[string]struct{}, len(evidence))
	previous := ""
	for index, reference := range evidence {
		if !validIdentifier(reference.CandidateID) {
			return fmt.Errorf("derived evidence %d has an invalid candidate id", index)
		}
		if _, duplicate := seen[reference.CandidateID]; duplicate {
			return fmt.Errorf("derived evidence %d duplicates candidate %s", index, reference.CandidateID)
		}
		if previous != "" && reference.CandidateID < previous {
			return errors.New("derived evidence must be sorted by candidate id")
		}
		previous = reference.CandidateID
		seen[reference.CandidateID] = struct{}{}
	}
	return nil
}

// strictUnmarshal 拒绝未知字段和任意尾随 JSON 值，防止载荷在存储层被附加
// 额外语义或拼接成多值流。
func strictUnmarshal(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("payload must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

// consistentRunFacts 判断同组候选的终态事实是否完全一致。
func consistentRunFacts(group []Candidate) bool {
	first := group[0]
	for _, candidate := range group[1:] {
		if candidate.Outcome != first.Outcome || candidate.FailureClass != first.FailureClass || candidate.TrustedVerification != first.TrustedVerification {
			return false
		}
	}
	return true
}

// createdBounds 返回组内候选创建时间的最早与最晚值，时间均已是 UTC。
func createdBounds(group []Candidate) (time.Time, time.Time) {
	first, last := group[0].CreatedAt, group[0].CreatedAt
	for _, candidate := range group[1:] {
		if candidate.CreatedAt.Before(first) {
			first = candidate.CreatedAt
		}
		if candidate.CreatedAt.After(last) {
			last = candidate.CreatedAt
		}
	}
	return first, last
}

// distinctTaskDigests 统计组内不同任务摘要的数量。
func distinctTaskDigests(group []Candidate) int {
	seen := make(map[string]struct{}, len(group))
	for _, candidate := range group {
		seen[candidate.TaskPromptDigest] = struct{}{}
	}
	return len(seen)
}

// sourceReferences 将组内候选转换为有序去重的间接证据引用。
func sourceReferences(group []Candidate) []SourceCandidateRef {
	ids := make([]string, 0, len(group))
	for _, candidate := range group {
		ids = append(ids, candidate.ID)
	}
	sort.Strings(ids)
	references := make([]SourceCandidateRef, 0, len(ids))
	for _, id := range ids {
		references = append(references, SourceCandidateRef{CandidateID: id})
	}
	return references
}

// inputIDList 返回输入候选 ID 的有序列表，用于巩固计划的 write-set 声明。
func inputIDList(inputs ConsolidationInputs) []string {
	ids := make([]string, 0, len(inputs.Candidates))
	for _, candidate := range inputs.Candidates {
		ids = append(ids, candidate.ID)
	}
	sort.Strings(ids)
	return ids
}

// digestKeySet 提取分组键集合，供稳定排序遍历。
func digestKeySet(groups map[string][]Candidate) map[string]struct{} {
	keys := make(map[string]struct{}, len(groups))
	for key := range groups {
		keys[key] = struct{}{}
	}
	return keys
}

// sortedStringSet 返回集合键的稳定排序结果，保证归纳输出可复现。
func sortedStringSet(values map[string]struct{}) []string {
	ordered := make([]string, 0, len(values))
	for value := range values {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	return ordered
}

// joinWithNUL 以 NUL 分隔连接有序 ID，避免分隔符歧义破坏摘要稳定性。
func joinWithNUL(values []string) string {
	result := ""
	for index, value := range values {
		if index != 0 {
			result += "\x00"
		}
		result += value
	}
	return result
}
