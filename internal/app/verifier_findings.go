// [INPUT]: 依赖 Verifier 输出与宿主校验的目标 Run。
// [OUTPUT]: 返回去重后的未定论 Finding 清单。
// [POS]: 模型提交的制品引用只供调查，确定性核验负责确认。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件组合独立验证与 Finding 核验。
// Author: fxbin
package app

import (
	"errors"

	"github.com/fxbin/cairn/internal/contract"
)

// ValidatedFindings 是一次独立验证经核验后的结果。只有 Confirmed 且阻塞的
// 发现才驱动返工；Inconclusive 保留为待调查，不等于没有问题。
type ValidatedFindings struct {
	Confirmed    []contract.Finding `json:"confirmed"`
	Inconclusive []contract.Finding `json:"inconclusive"`
	Rejected     []contract.Finding `json:"rejected"`
	BlockingIDs  []string           `json:"blocking_ids"`
}

// ValidateVerifierFindings 收束独立只读审查的待核验发现。制品存在只证明
// 引用真实，不能证明具体缺陷；未经确定性复核的发现一律保留为未定论。
func ValidateVerifierFindings(result VerifierResult, validator string) (ValidatedFindings, error) {
	for _, finding := range result.Findings {
		if result.target.TargetRunID == "" || finding.TargetID != string(result.target.TargetRunID) {
			return ValidatedFindings{}, errors.New("finding does not belong to the verified target")
		}
	}
	confirmed, inconclusive, rejected, err := DeduplicateAndValidate(result.Findings, validator, nil)
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
