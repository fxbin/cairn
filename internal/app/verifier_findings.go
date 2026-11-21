// [INPUT]: 依赖 Verifier 子运行输出与工作区制品库的证据校验。
// [OUTPUT]: 返回经真实证据核验、去重与状态推进的 Finding 清单。
// [POS]: 组合门面；证据必须在真实制品库在档，确认结果驱动有界返工。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件组合独立验证与 Finding 核验。
// Author: fxbin
package app

import "github.com/fxbin/cairn/internal/contract"

// ValidatedFindings 是一次独立验证经核验后的结果。只有 Confirmed 且阻塞的
// 发现才驱动返工；Inconclusive 保留为待调查，不等于没有问题。
type ValidatedFindings struct {
	Confirmed    []contract.Finding `json:"confirmed"`
	Inconclusive []contract.Finding `json:"inconclusive"`
	Rejected     []contract.Finding `json:"rejected"`
	BlockingIDs  []string           `json:"blocking_ids"`
}

// ValidateVerifierFindings 把 Verifier 产出的待核验 Finding 与真实工作区制品库
// 绑定后逐条核验。证据引用必须在制品库在档且摘要一致，否则不得确认。
func ValidateVerifierFindings(result VerifierResult, stateDir, workspaceHash, validator string) (ValidatedFindings, error) {
	verify := WorkspaceEvidenceVerifier(stateDir, workspaceHash)
	confirmed, inconclusive, rejected, err := DeduplicateAndValidate(result.Findings, validator, verify)
	if err != nil {
		return ValidatedFindings{}, err
	}
	return ValidatedFindings{
		Confirmed:    confirmed,
		Inconclusive: inconclusive,
		Rejected:     rejected,
		BlockingIDs:  BlockingFindingIDs(confirmed),
	}, nil
}
