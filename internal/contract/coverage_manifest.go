// coverage_manifest.go 定义领域无关的验证覆盖清单：逐对象状态、分母保护、
// 证据复用条件与保守失效。覆盖度与结论分离，清单本身不授予权限、不决定终态。
//
// Author: fxbin
package contract

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

var contentDigestPattern = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)

// validContentDigest 要求引用是带 sha256 域前缀的完整内容摘要，拒绝空壳引用。
func validContentDigest(digest string) bool {
	return contentDigestPattern.MatchString(digest)
}

const (
	// CoverageManifestVersion 是覆盖清单的当前格式版本。
	CoverageManifestVersion = 1
	// coverageManifestDigestPrefix 是清单摘要的版本化域前缀。
	coverageManifestDigestPrefix = "cairn-coverage-manifest-v1"
)

// CoverageStatus 是单个验证对象在覆盖清单中的状态。
type CoverageStatus string

const (
	// CoverageSelected 表示对象已随范围冻结登记，尚未准备执行。
	CoverageSelected CoverageStatus = "selected"
	// CoveragePending 表示对象等待执行或重新验证。
	CoveragePending CoverageStatus = "pending"
	// CoverageRunning 表示对象正在执行。
	CoverageRunning CoverageStatus = "running"
	// CoverageCompleted 表示对象已执行过；只代表执行事实，不代表通过。
	CoverageCompleted CoverageStatus = "completed"
	// CoverageReused 表示对象复用了历史证据，且已通过有效性核验。
	CoverageReused CoverageStatus = "reused"
	// CoverageFailed 表示验证执行失败，不等同于发现业务缺陷。
	CoverageFailed CoverageStatus = "failed"
	// CoverageSkipped 表示对象未执行，必须记录理由。
	CoverageSkipped CoverageStatus = "skipped"
	// CoverageWaived 表示对象经合法授权豁免，必须记录授权来源。
	CoverageWaived CoverageStatus = "waived"
	// CoverageExcluded 表示对象被排除；不得用于缩小分母或隐藏未覆盖对象。
	CoverageExcluded CoverageStatus = "excluded"
)

// CoverageEntry 保存一个验证对象的覆盖状态及其证据绑定。
type CoverageEntry struct {
	ObjectID       string         `json:"object_id"`
	Status         CoverageStatus `json:"status"`
	Reason         string         `json:"reason,omitempty"`
	Authorization  string         `json:"authorization,omitempty"`
	EvidenceRef    *Ref           `json:"evidence_ref,omitempty"`
	SubjectVersion string         `json:"subject_version,omitempty"`
	VerifierID     string         `json:"verifier_id,omitempty"`
	RulesetVersion string         `json:"ruleset_version,omitempty"`
}

// CoverageManifest 是冻结范围的覆盖清单。Digest 覆盖版本、范围摘要与全部条目，
// 读取方必须重算并比对。条目集合必须与范围对象一一对应，登记后不可增删。
type CoverageManifest struct {
	Version          int             `json:"version"`
	ScopeDigest      string          `json:"scope_digest"`
	AcceptanceDigest string          `json:"acceptance_digest"`
	Entries          []CoverageEntry `json:"entries"`
	Digest           string          `json:"digest"`
}

// coverageManifestContent 是摘要输入；Digest 字段自身不参与计算。
type coverageManifestContent struct {
	Version          int             `json:"version"`
	ScopeDigest      string          `json:"scope_digest"`
	AcceptanceDigest string          `json:"acceptance_digest"`
	Entries          []CoverageEntry `json:"entries"`
}

// CoverageUpdate 是一次状态推进请求。权威来源仍是 Journal；清单只按显式转换
// 推进，不根据自然语言总结自动改写状态。
type CoverageUpdate struct {
	ObjectID       string
	Status         CoverageStatus
	Reason         string
	Authorization  string
	EvidenceRef    *Ref
	SubjectVersion string
	VerifierID     string
	RulesetVersion string
	// reuseValidated 由 MarkReused 在 CanReuse 通过后设置，外部无法伪造。
	reuseValidated bool
}

// ReuseCandidate 是历史证据复用候选。全部绑定字段必须匹配冻结范围，且不存在
// 已知影响可信度的变化，才允许进入复用状态。
type ReuseCandidate struct {
	ObjectID         string
	SubjectVersion   string
	AcceptanceDigest string
	Ruleset          string
	RulesetVersion   string
	VerifierID       string
	VerifierPolicy   string
	EvidenceRef      Ref
	Invalidated      bool
	// Conclusion 与 CoverageComplete 必须与冻结证据一致：只有此前通过且覆盖
	// 完整的证据才可复用，避免与 PR #82 的验收口径产生分歧。
	Conclusion       string
	CoverageComplete bool
}

// CoverageSummary 是覆盖度投影。Complete 只表示全部必需对象已被实际执行，
// 不包含通过与否；结论由 Acceptance Evaluation 独立评估。
type CoverageSummary struct {
	Total                int      `json:"total"`
	Required             int      `json:"required"`
	Executed             int      `json:"executed"`
	Covered              int      `json:"covered"`
	AuthorizedExceptions int      `json:"authorized_exceptions"`
	UncoveredRequired    []string `json:"uncovered_required"`
	Complete             bool     `json:"complete"`
}

// NewCoverageManifest 为冻结范围创建清单，所有对象初始为 Selected。分母在此时
// 固定，后续只能转换状态，不能增删对象或改变必需性。
func NewCoverageManifest(scope VerificationScope) (CoverageManifest, error) {
	if err := scope.Validate(); err != nil {
		return CoverageManifest{}, fmt.Errorf("verification scope: %w", err)
	}
	entries := make([]CoverageEntry, 0, len(scope.Objects))
	for _, object := range scope.Objects {
		entries = append(entries, CoverageEntry{ObjectID: object.ID, Status: CoverageSelected})
	}
	digest, err := coverageManifestDigest(scope.Digest, scope.AcceptanceDigest, entries)
	if err != nil {
		return CoverageManifest{}, err
	}
	return CoverageManifest{
		Version: CoverageManifestVersion, ScopeDigest: scope.Digest,
		AcceptanceDigest: scope.AcceptanceDigest, Entries: entries, Digest: digest,
	}, nil
}

// Validate 复核版本、条目结构与摘要；任何不一致都失败关闭。
func (manifest CoverageManifest) Validate() error {
	if manifest.Version != CoverageManifestVersion {
		return fmt.Errorf("coverage manifest version %d is unsupported", manifest.Version)
	}
	if manifest.ScopeDigest == "" || manifest.AcceptanceDigest == "" {
		return errors.New("coverage manifest has no scope or acceptance anchor")
	}
	if err := validateCoverageEntries(manifest.Entries); err != nil {
		return err
	}
	digest, err := coverageManifestDigest(manifest.ScopeDigest, manifest.AcceptanceDigest, manifest.Entries)
	if err != nil {
		return err
	}
	if manifest.Digest != digest {
		return errors.New("coverage manifest digest does not match its entries")
	}
	return nil
}

// ValidateAgainst 在 Validate 基础上核对清单与冻结范围一一对应，且证据版本
// 与范围对象一致。任何漂移都失败关闭，防止用陈旧证据验收新产物。
func (manifest CoverageManifest) ValidateAgainst(scope VerificationScope) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("verification scope: %w", err)
	}
	if manifest.ScopeDigest != scope.Digest || manifest.AcceptanceDigest != scope.AcceptanceDigest {
		return errors.New("coverage manifest scope or acceptance digest does not match the frozen scope")
	}
	if len(manifest.Entries) != len(scope.Objects) {
		return errors.New("coverage manifest entry set does not match the frozen scope")
	}
	objects := make(map[string]VerificationObject, len(scope.Objects))
	for _, object := range scope.Objects {
		objects[object.ID] = object
	}
	for _, entry := range manifest.Entries {
		object, found := objects[entry.ObjectID]
		if !found {
			return fmt.Errorf("coverage manifest has unknown object %q", entry.ObjectID)
		}
		delete(objects, entry.ObjectID)
		if entry.Status == CoverageCompleted || entry.Status == CoverageReused {
			if entry.SubjectVersion != object.Version || entry.RulesetVersion != object.RulesetVersion {
				return fmt.Errorf("coverage entry %q evidence version does not match the frozen object", entry.ObjectID)
			}
		}
	}
	if len(objects) != 0 {
		return errors.New("coverage manifest is missing frozen objects")
	}
	return nil
}

// Apply 按显式转换推进若干条目并计算新摘要。转换非法或状态字段不满足要求时
// 整批失败关闭，不产生部分更新。
func (manifest CoverageManifest) Apply(updates []CoverageUpdate) (CoverageManifest, error) {
	if err := manifest.Validate(); err != nil {
		return CoverageManifest{}, err
	}
	cloned := manifest
	cloned.Entries = append([]CoverageEntry(nil), manifest.Entries...)
	indexed := make(map[string]int, len(cloned.Entries))
	for index, entry := range cloned.Entries {
		indexed[entry.ObjectID] = index
	}
	for _, update := range updates {
		index, found := indexed[update.ObjectID]
		if !found {
			return CoverageManifest{}, fmt.Errorf("coverage update references unknown object %q", update.ObjectID)
		}
		entry := &cloned.Entries[index]
		if !coverageTransitionAllowed(entry.Status, update.Status) {
			return CoverageManifest{}, fmt.Errorf("coverage transition %s -> %s for %q is not allowed", entry.Status, update.Status, update.ObjectID)
		}
		// Reused 必须经 CanReuse 有效性核验，只能通过 MarkReused 进入。
		if update.Status == CoverageReused && !update.reuseValidated {
			return CoverageManifest{}, errors.New("reused status requires MarkReused with a validated reuse candidate")
		}
		next := CoverageEntry{ObjectID: entry.ObjectID, Status: update.Status}
		switch update.Status {
		case CoverageCompleted, CoverageReused:
			if update.EvidenceRef == nil || !validContentDigest(update.EvidenceRef.SHA256) ||
				update.SubjectVersion == "" || update.VerifierID == "" || update.RulesetVersion == "" {
				return CoverageManifest{}, fmt.Errorf("coverage entry %q requires a valid evidence reference, subject version, verifier and ruleset version", update.ObjectID)
			}
			reference := *update.EvidenceRef
			next.EvidenceRef = &reference
			next.SubjectVersion = update.SubjectVersion
			next.VerifierID = update.VerifierID
			next.RulesetVersion = update.RulesetVersion
		case CoverageFailed, CoverageSkipped:
			if strings.TrimSpace(update.Reason) == "" {
				return CoverageManifest{}, fmt.Errorf("coverage entry %q requires a reason for %s", update.ObjectID, update.Status)
			}
			next.Reason = update.Reason
		case CoverageWaived, CoverageExcluded:
			if strings.TrimSpace(update.Reason) == "" || strings.TrimSpace(update.Authorization) == "" {
				return CoverageManifest{}, fmt.Errorf("coverage entry %q requires a reason and authorization for %s", update.ObjectID, update.Status)
			}
			next.Reason = update.Reason
			next.Authorization = update.Authorization
		case CoveragePending, CoverageRunning, CoverageSelected:
			// 这些状态不携带终态证据；保留失效/跳过理由，但清空证据绑定。
			next.Reason = update.Reason
		}
		cloned.Entries[index] = next
	}
	digest, err := coverageManifestDigest(cloned.ScopeDigest, cloned.AcceptanceDigest, cloned.Entries)
	if err != nil {
		return CoverageManifest{}, err
	}
	cloned.Digest = digest
	if err := cloned.Validate(); err != nil {
		return CoverageManifest{}, err
	}
	return cloned, nil
}

// ApplyBound 在 Apply 后按冻结范围校验，确保 Completed 条目的证据版本与对象
// 一致。只调用 Apply 而不核对范围的清单不得用于验收判定。
func (manifest CoverageManifest) ApplyBound(scope VerificationScope, updates []CoverageUpdate) (CoverageManifest, error) {
	applied, err := manifest.Apply(updates)
	if err != nil {
		return CoverageManifest{}, err
	}
	if err := applied.ValidateAgainst(scope); err != nil {
		return CoverageManifest{}, err
	}
	return applied, nil
}

// MarkReused 是进入 Reused 状态的唯一入口：先执行 CanReuse 的全部有效性核验，
// 再按冻结范围写入条目。历史成功回执本身不构成复用理由。
func (manifest CoverageManifest) MarkReused(scope VerificationScope, candidate ReuseCandidate) (CoverageManifest, error) {
	if err := CanReuse(scope, candidate); err != nil {
		return CoverageManifest{}, fmt.Errorf("reuse validation failed: %w", err)
	}
	reference := candidate.EvidenceRef
	return manifest.ApplyBound(scope, []CoverageUpdate{{
		ObjectID: candidate.ObjectID, Status: CoverageReused,
		EvidenceRef: &reference, SubjectVersion: candidate.SubjectVersion,
		VerifierID: candidate.VerifierID, RulesetVersion: candidate.RulesetVersion,
		reuseValidated: true,
	}})
}

// Invalidate 把受影响条目退回 Pending 并清除证据绑定。无法证明未受影响时
// 必须保守重新验证；不在可失效状态的条目返回错误，避免静默掩盖状态。
func (manifest CoverageManifest) Invalidate(objectIDs []string, reason string) (CoverageManifest, error) {
	if strings.TrimSpace(reason) == "" {
		return CoverageManifest{}, errors.New("invalidation requires a reason")
	}
	updates := make([]CoverageUpdate, 0, len(objectIDs))
	for _, id := range objectIDs {
		updates = append(updates, CoverageUpdate{ObjectID: id, Status: CoveragePending, Reason: reason})
	}
	return manifest.Apply(updates)
}

// Summary 投影覆盖度。Required 取自冻结范围，不取清单条目，避免通过删除条目
// 缩小分母；未执行、失败、跳过、豁免、排除都不计入 Covered。
func (manifest CoverageManifest) Summary(scope VerificationScope) (CoverageSummary, error) {
	if err := manifest.ValidateAgainst(scope); err != nil {
		return CoverageSummary{}, err
	}
	required := make(map[string]bool, len(scope.Objects))
	for _, object := range scope.Objects {
		required[object.ID] = object.Required
	}
	summary := CoverageSummary{Total: len(scope.Objects), UncoveredRequired: []string{}}
	byID := make(map[string]CoverageEntry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		byID[entry.ObjectID] = entry
	}
	for _, object := range scope.Objects {
		entry := byID[object.ID]
		switch entry.Status {
		case CoverageCompleted, CoverageReused:
			summary.Executed++
			if required[object.ID] {
				summary.Covered++
			}
		case CoverageWaived, CoverageExcluded:
			if required[object.ID] {
				summary.AuthorizedExceptions++
			}
		}
		if required[object.ID] && entry.Status != CoverageCompleted && entry.Status != CoverageReused {
			summary.UncoveredRequired = append(summary.UncoveredRequired, object.ID)
		}
		if required[object.ID] {
			summary.Required++
		}
	}
	sort.Strings(summary.UncoveredRequired)
	summary.Complete = summary.Required > 0 && summary.Covered == summary.Required
	return summary, nil
}

// CanReuse 判断历史证据是否满足全部复用条件。任一条件不满足或存在已知失效
// 都拒绝复用，调用方必须重新验证；存在历史成功回执不构成复用理由。
func CanReuse(scope VerificationScope, candidate ReuseCandidate) error {
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("verification scope: %w", err)
	}
	object, found := scope.Object(candidate.ObjectID)
	if !found {
		return errors.New("reuse candidate references an unknown object")
	}
	if candidate.Invalidated {
		return errors.New("reuse candidate is marked invalidated")
	}
	if candidate.SubjectVersion != object.Version {
		return errors.New("reuse candidate subject version does not match the frozen object")
	}
	if candidate.AcceptanceDigest != scope.AcceptanceDigest {
		return errors.New("reuse candidate acceptance digest does not match the frozen contract")
	}
	if candidate.Ruleset != object.Ruleset || candidate.RulesetVersion != object.RulesetVersion {
		return errors.New("reuse candidate ruleset does not match the frozen object")
	}
	if strings.TrimSpace(candidate.VerifierID) == "" || strings.TrimSpace(candidate.VerifierPolicy) == "" {
		return errors.New("reuse candidate has no verifier identity or policy")
	}
	if !validContentDigest(candidate.EvidenceRef.SHA256) {
		return errors.New("reuse candidate has an invalid evidence reference")
	}
	if candidate.Conclusion != ConclusionPassed || !candidate.CoverageComplete {
		return errors.New("reuse candidate evidence was not a passed, fully covered result")
	}
	return nil
}

// coverageTransitionAllowed 判断状态转换是否合法。终态证据只在 Completed/Reused
// 之间刷新，失效统一退回 Pending，不允许直接改写成另一对象的通过状态。
func coverageTransitionAllowed(from, to CoverageStatus) bool {
	switch from {
	case CoverageSelected:
		return to == CoveragePending || to == CoverageExcluded
	case CoveragePending:
		return to == CoverageRunning || to == CoverageSkipped || to == CoverageWaived || to == CoverageReused
	case CoverageRunning:
		return to == CoverageCompleted || to == CoverageFailed || to == CoverageSkipped || to == CoveragePending
	case CoverageCompleted:
		return to == CoveragePending || to == CoverageCompleted
	case CoverageReused:
		return to == CoveragePending || to == CoverageReused
	case CoverageFailed:
		return to == CoveragePending
	case CoverageSkipped:
		return to == CoveragePending
	case CoverageWaived:
		return to == CoveragePending
	case CoverageExcluded:
		return to == CoveragePending
	default:
		return false
	}
}

// validateCoverageEntries 校验条目集合结构与状态相关字段，拒绝重复对象与未知状态。
func validateCoverageEntries(entries []CoverageEntry) error {
	if len(entries) == 0 {
		return errors.New("coverage manifest has no entries")
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if !verificationToken.MatchString(entry.ObjectID) {
			return errors.New("coverage entry has an invalid object id")
		}
		if _, duplicate := seen[entry.ObjectID]; duplicate {
			return fmt.Errorf("coverage entry %q is duplicated", entry.ObjectID)
		}
		seen[entry.ObjectID] = struct{}{}
		switch entry.Status {
		case CoverageSelected, CoveragePending, CoverageRunning:
			// 非终态不携带证据绑定。
		case CoverageCompleted, CoverageReused:
			if entry.EvidenceRef == nil || !validContentDigest(entry.EvidenceRef.SHA256) ||
				entry.SubjectVersion == "" || entry.VerifierID == "" || entry.RulesetVersion == "" {
				return fmt.Errorf("coverage entry %q has incomplete evidence binding", entry.ObjectID)
			}
		case CoverageFailed, CoverageSkipped:
			if strings.TrimSpace(entry.Reason) == "" {
				return fmt.Errorf("coverage entry %q has no reason", entry.ObjectID)
			}
		case CoverageWaived, CoverageExcluded:
			if strings.TrimSpace(entry.Reason) == "" || strings.TrimSpace(entry.Authorization) == "" {
				return fmt.Errorf("coverage entry %q has no reason or authorization", entry.ObjectID)
			}
		default:
			return fmt.Errorf("coverage entry %q has an invalid status", entry.ObjectID)
		}
	}
	return nil
}

// coverageManifestDigest 用规范化 JSON 计算清单摘要；条目顺序参与摘要。
func coverageManifestDigest(scopeDigest, acceptanceDigest string, entries []CoverageEntry) (string, error) {
	return canonicaljson.Fingerprint(coverageManifestDigestPrefix, coverageManifestContent{
		Version:          CoverageManifestVersion,
		ScopeDigest:      scopeDigest,
		AcceptanceDigest: acceptanceDigest,
		Entries:          entries,
	})
}
