// Package decision 补充决策账本的证据模型：证据定位只引用既有事实源的稳定
// 标识与摘要，不复制证据正文。核查由外部解析器完成，解析器必须基于 Journal、
// Artifact、Git 或测试结果判定存在性，不能凭记录自身断言。
//
// Author: fxbin
package decision

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// EvidenceKind 是决策证据的来源种类；每种来源都有稳定且可核查的标识。
type EvidenceKind string

const (
	// EvidenceKindJournalEvent 指向运行事件 Journal 中的一条事件。
	EvidenceKindJournalEvent EvidenceKind = "journal_event"
	// EvidenceKindArtifact 指向内容寻址制品存储中的一份制品。
	EvidenceKindArtifact EvidenceKind = "artifact"
	// EvidenceKindGitCommit 指向一次 Git 提交。
	EvidenceKindGitCommit EvidenceKind = "git_commit"
	// EvidenceKindTestResult 指向一次测试或验证结果的稳定摘要。
	EvidenceKindTestResult EvidenceKind = "test_result"
)

// 证据解析的稳定结果分类；解析失败只暴露分类，不暴露底层路径与命令输出。
const (
	// EvidenceResolutionFound 表示证据已在事实源中定位到。
	EvidenceResolutionFound = "found"
	// EvidenceResolutionMissing 表示证据在事实源中不存在。
	EvidenceResolutionMissing = "missing"
	// EvidenceResolutionFailed 表示解析过程失败，无法判定存在性。
	EvidenceResolutionFailed = "failed"
)

// EvidenceRef 是一条决策结论所依据的证据定位。它只保存稳定标识与摘要，
// 不保存证据正文、路径、命令或凭据。
type EvidenceRef struct {
	// Kind 是证据来源种类。
	Kind EvidenceKind `json:"kind"`
	// RunID 是 Journal 事件的所属运行标识，仅 journal_event 使用。
	RunID string `json:"run_id,omitempty"`
	// EventSeq 是 Journal 事件在链上的序号，仅 journal_event 使用。
	EventSeq uint64 `json:"event_seq,omitempty"`
	// EventHash 是 Journal 事件的内容哈希，仅 journal_event 使用。
	EventHash string `json:"event_hash,omitempty"`
	// Digest 是内容寻址摘要，仅 artifact 与 test_result 使用。
	Digest string `json:"digest,omitempty"`
	// CommitID 是 Git 提交标识，仅 git_commit 使用。
	CommitID string `json:"commit_id,omitempty"`
}

// Validate 校验证据定位的结构合法性与字段归属：每种来源只允许携带自己的
// 标识字段，出现他源字段即失败关闭，避免一条证据同时冒充多个来源。
func (ref EvidenceRef) Validate() error {
	switch ref.Kind {
	case EvidenceKindJournalEvent:
		if !validIdentifier(ref.RunID) {
			return errors.New("journal event evidence requires a valid run identifier")
		}
		if ref.EventSeq == 0 {
			return errors.New("journal event evidence requires a positive event sequence")
		}
		if !validHexDigest(ref.EventHash) {
			return errors.New("journal event evidence requires a hex event hash")
		}
		if ref.Digest != "" || ref.CommitID != "" {
			return errors.New("journal event evidence must not carry foreign source fields")
		}
	case EvidenceKindArtifact, EvidenceKindTestResult:
		if !validDigest(ref.Digest) {
			return errors.New("artifact or test result evidence requires a sha256 digest")
		}
		if ref.RunID != "" || ref.EventSeq != 0 || ref.EventHash != "" || ref.CommitID != "" {
			return errors.New("artifact or test result evidence must not carry foreign source fields")
		}
	case EvidenceKindGitCommit:
		if !validCommitID(ref.CommitID) {
			return errors.New("git commit evidence requires a hex commit identifier")
		}
		if ref.RunID != "" || ref.EventSeq != 0 || ref.EventHash != "" || ref.Digest != "" {
			return errors.New("git commit evidence must not carry foreign source fields")
		}
	default:
		return fmt.Errorf("unsupported evidence kind %q", ref.Kind)
	}
	return nil
}

// Key 返回证据定位的稳定比较键，用于去重与引用核对。
func (ref EvidenceRef) Key() string {
	switch ref.Kind {
	case EvidenceKindJournalEvent:
		return string(ref.Kind) + ":" + ref.RunID + ":" + strconv.FormatUint(ref.EventSeq, 10) + ":" + ref.EventHash
	case EvidenceKindGitCommit:
		return string(ref.Kind) + ":" + ref.CommitID
	default:
		return string(ref.Kind) + ":" + ref.Digest
	}
}

// EvidenceResolver 判定一条证据定位能否在既有事实源中解析到实体。实现方
// 必须基于真实的 Journal、Artifact、Git 或测试结果判定存在性，不得依据
// 决策记录自身的断言返回存在。
type EvidenceResolver interface {
	Resolve(ctx context.Context, ref EvidenceRef) (EvidenceResolution, error)
}

// EvidenceResolution 是证据解析结果；Found 为假时 Digest 必须为空。
type EvidenceResolution struct {
	// Found 表示证据是否在事实源中定位到。
	Found bool
	// Digest 是解析到的实体摘要，用于与引用交叉核对。
	Digest string
}

// EvidenceStatus 是单条证据的核查结论。
type EvidenceStatus struct {
	// RecordID 是被核查的决策记录标识。
	RecordID string
	// Ref 是被核查的证据定位。
	Ref EvidenceRef
	// Status 是核查分类，取值见 EvidenceResolutionFound 等常量。
	Status string
	// Digest 是解析到的实体摘要；未定位到时为空。
	Digest string
}

// EvidenceReport 是一次只读证据核查的汇总结果。
type EvidenceReport struct {
	// CheckedAt 是核查时刻。
	CheckedAt time.Time
	// Statuses 是每条证据的逐项结论。
	Statuses []EvidenceStatus
	// Resolved 是定位成功的证据条数。
	Resolved int
	// Unresolved 是未能定位到证据的条数。
	Unresolved int
}

// VerifyEvidence 只读核查一批决策记录的证据定位。它不修改账本，也不把
// 解析失败的底层错误正文写入报告，只保留稳定分类，避免路径与命令输出泄露。
func VerifyEvidence(ctx context.Context, resolver EvidenceResolver, records []DecisionRecord, now time.Time) (EvidenceReport, error) {
	if resolver == nil {
		return EvidenceReport{}, errors.New("evidence resolver is required")
	}
	if now.IsZero() {
		return EvidenceReport{}, errors.New("evidence verification requires an explicit time")
	}
	report := EvidenceReport{CheckedAt: now.UTC(), Statuses: []EvidenceStatus{}}
	for _, record := range records {
		for _, ref := range record.Evidence {
			status := EvidenceStatus{RecordID: record.ID, Ref: ref}
			resolution, err := resolver.Resolve(ctx, ref)
			switch {
			case err != nil:
				status.Status = EvidenceResolutionFailed
			case resolution.Found:
				status.Status = EvidenceResolutionFound
				status.Digest = resolution.Digest
				report.Resolved++
			default:
				status.Status = EvidenceResolutionMissing
				report.Unresolved++
			}
			report.Statuses = append(report.Statuses, status)
		}
	}
	return report, nil
}

// validHexDigest 判断摘要是否为不带算法前缀的完整十六进制 SHA-256。
func validHexDigest(value string) bool {
	return validHex(value, 64)
}

// validCommitID 判断标识是否为完整十六进制的 Git 提交号。
func validCommitID(value string) bool {
	return validHex(value, 40) || validHex(value, 64)
}

// validHex 判断值是否为指定长度的十六进制串。
func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F') {
			continue
		}
		return false
	}
	return true
}
