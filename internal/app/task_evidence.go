// [INPUT]: 依赖 Run Journal 验证事件、冻结验收合同与内容寻址制品存储。
// [OUTPUT]: 从运行证据制品投影可校验证据，供任务完成与只读视图共用。
// [POS]: 证据引用归 Journal；本文件只校验，不改变运行事实或授予权限。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接标准级证据制品与任务验收判定。
// Author: fxbin
package app

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// acceptanceEvidenceFromRun 从指定验证事件读取标准级证据并绑定到冻结合同。
// 结构性问题（引用损坏、摘要不符、来源漂移）返回错误；证据合法但不满足合同
// （覆盖不完整、标准失败）返回 nil 且不报错，使只读视图仍可展示该运行。
func acceptanceEvidenceFromRun(events []domain.Event, verificationSeq uint64, stateDir, workspaceHash string, acceptance contract.AcceptanceContract) (*contract.AcceptanceEvidence, error) {
	if verificationSeq == 0 || verificationSeq > uint64(len(events)) {
		return nil, nil
	}
	finish := events[verificationSeq-1]
	if finish.Type != domain.EventVerificationFinished || finish.TurnID == nil {
		return nil, nil
	}
	var finished domain.VerificationFinishedPayload
	if err := json.Unmarshal(finish.Payload, &finished); err != nil {
		return nil, fmt.Errorf("decode verification finish: %w", err)
	}
	if len(finished.EvidenceRef) == 0 || string(finished.EvidenceRef) == "null" {
		return nil, nil
	}
	var reference contract.Ref
	if err := json.Unmarshal(finished.EvidenceRef, &reference); err != nil {
		return nil, errors.New("invalid acceptance evidence reference")
	}
	content, err := readArtifactContent(manifestArtifactRoot(stateDir, workspaceHash), reference)
	if err != nil {
		return nil, fmt.Errorf("read acceptance evidence: %w", err)
	}
	var evidence contract.AcceptanceEvidence
	if err := json.Unmarshal(content, &evidence); err != nil {
		return nil, errors.New("decode acceptance evidence")
	}
	if err := evidence.Validate(); err != nil {
		return nil, fmt.Errorf("invalid acceptance evidence: %w", err)
	}
	started, found := verificationStartedForFinish(events, int(verificationSeq-1), *finish.TurnID, finished.VerificationID)
	if !found {
		return nil, errors.New("acceptance evidence has no matching verification start")
	}
	if evidence.RunID != string(finish.RunID) || evidence.Source.VerificationID != finished.VerificationID {
		return nil, errors.New("acceptance evidence source does not match its run")
	}
	if finished.WorkspaceObservation == "" || finished.WorkspaceObservation != started.workspaceObservation {
		return nil, errors.New("acceptance evidence workspace observation drifted")
	}
	if finished.VerifiedMutationSeq == nil || *finished.VerifiedMutationSeq != started.lastMutationSeq {
		return nil, errors.New("acceptance evidence mutation sequence does not match its plan")
	}
	if evidence.Source.CommandsFingerprint != started.commandsFingerprint {
		return nil, errors.New("acceptance evidence command fingerprint does not match its plan")
	}
	if started.planRef == nil || evidence.Source.PlanRef == nil || evidence.Source.PlanRef.SHA256 != started.planRef.SHA256 {
		return nil, errors.New("acceptance evidence plan reference does not match its plan")
	}
	if evidence.AcceptanceDigest != acceptance.Digest {
		return nil, errors.New("acceptance evidence contract digest mismatch")
	}
	if err := evidence.Satisfies(acceptance, workspaceHash, started.workspaceObservation); err != nil {
		return nil, nil
	}
	return &evidence, nil
}

// evaluationFromEvidence 从已校验证据与冻结合同计算多级验收结论。当前没有独立
// Verifier 的 Finding 源，Findings 为空；这不等于"没有问题"，只表示本 Run 尚无
// 结构化发现。Coverage 从证据自带的覆盖计数派生，不重新引入第二事实源。
func evaluationFromEvidence(acceptance contract.AcceptanceContract, evidence *contract.AcceptanceEvidence) (contract.AcceptanceEvaluation, error) {
	if evidence == nil {
		return contract.AcceptanceEvaluation{}, errors.New("acceptance evidence is required for evaluation")
	}
	coverage := contract.CoverageSummary{
		Total:                len(acceptance.Criteria),
		Required:             evidence.Coverage.Required,
		Covered:              evidence.Coverage.Covered,
		Executed:             evidence.Coverage.Passed,
		AuthorizedExceptions: 0,
		UncoveredRequired:    []string{},
		Complete:             evidence.Coverage.Complete,
	}
	return contract.EvaluateAcceptanceVerdict(contract.EvaluationInput{
		Acceptance: acceptance,
		Coverage:   coverage,
		Evidence:   evidence,
	})
}

// verificationStartRecord 保存验证开始事件中供证据校验的绑定字段。
type verificationStartRecord struct {
	verificationID       string
	commandsFingerprint  string
	lastMutationSeq      uint64
	workspaceObservation string
	planRef              *contract.Ref
}

// verificationStartedForFinish 在同一轮次内定位 finish 对应的 started 事件。
func verificationStartedForFinish(events []domain.Event, finishIndex int, turnID domain.TurnID, verificationID string) (verificationStartRecord, bool) {
	for index := finishIndex - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationStarted || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload struct {
			VerificationID       string        `json:"verification_id"`
			CommandsFingerprint  string        `json:"commands_fingerprint"`
			LastMutationSeq      uint64        `json:"last_mutation_seq"`
			WorkspaceObservation string        `json:"workspace_observation"`
			PlanRef              *contract.Ref `json:"plan_ref"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.VerificationID != verificationID {
			return verificationStartRecord{}, false
		}
		return verificationStartRecord{
			verificationID:       payload.VerificationID,
			commandsFingerprint:  payload.CommandsFingerprint,
			lastMutationSeq:      payload.LastMutationSeq,
			workspaceObservation: payload.WorkspaceObservation,
			planRef:              payload.PlanRef,
		}, true
	}
	return verificationStartRecord{}, false
}
