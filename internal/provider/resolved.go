// 本文件实现由显式事实组装 ResolvedProviderProfile 的应用层入口：身份经
// 静态注册表派生，能力与生成参数复用契约层既有解析与指纹算法，凭据槽位
// 引用做确定性形状校验。本包不依赖 config，事实由调用方（app 装配层）从
// 已校验配置投影，保证解析规则单一来源。
//
// Author: fxbin
package provider

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/contract"
)

// ResolvedFacts 是组装 ResolvedProviderProfile 所需的全部显式事实；能力
// 声明必须显式在场（未知能力不可运行），模型修订缺席由 RevisionAbsent
// 语义表达，credential_ref 首版取 api_key_env 原文。
type ResolvedFacts struct {
	Selection     SelectionFacts
	Capability    *contract.ProviderCapabilityDeclaration
	Generation    *contract.ProviderGenerationDeclaration
	ModelRevision string
	CredentialRef string
}

// ResolveResolvedProfile 由事实组装 run 启动身份合同：selection 经注册表
// 派生；能力指纹复算自能力卡合同（cairn-provider-capability-v1）；生成参数
// 走契约层两态解析；凭据槽位引用形状非法失败关闭。任何事实缺失或非法都
// 在此失败关闭，调用方不得以部分事实构造。
func ResolveResolvedProfile(facts ResolvedFacts) (contract.ResolvedProviderProfile, error) {
	selection, err := ResolveSelection(facts.Selection)
	if err != nil {
		return contract.ResolvedProviderProfile{}, err
	}
	if facts.Capability == nil {
		return contract.ResolvedProviderProfile{}, errors.New("resolved profile requires an explicit capability declaration")
	}
	// 当前唯一注册协议对应历史 ProviderKind；协议集合扩充时此处必须改为
	// 显式映射，不得静默假定。
	card, err := contract.ResolveProviderCard(contract.ProviderKindOpenAICompatible, facts.Selection.Model, facts.Capability)
	if err != nil {
		return contract.ResolvedProviderProfile{}, err
	}
	capabilityDigest, err := card.Fingerprint()
	if err != nil {
		return contract.ResolvedProviderProfile{}, fmt.Errorf("fingerprint capability card: %w", err)
	}
	generation, err := contract.ResolveGenerationProfile(facts.Generation, card)
	if err != nil {
		return contract.ResolvedProviderProfile{}, err
	}
	revision := facts.ModelRevision
	if revision == "" {
		revision = contract.RevisionAbsent
	}
	if !contract.ValidCredentialRefShape(facts.CredentialRef) {
		return contract.ResolvedProviderProfile{}, errors.New("credential_ref must be a portable environment variable name")
	}
	return contract.ResolvedProviderProfile{
		SchemaVersion:         contract.ResolvedProfileSchemaVersion,
		Selection:             selection,
		CapabilityFingerprint: capabilityDigest,
		Generation:            generation,
		ModelRevision:         revision,
		CredentialRef:         facts.CredentialRef,
	}, nil
}
