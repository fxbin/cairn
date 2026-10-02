// 本文件承载沙箱激活版本的试运行解析：把演化账本中当前激活的压缩参数
// 变体应用到运行配置副本上。解析是显式选择加入的只读操作，无激活时配置
// 原样返回；任何账本不一致都在运行开始前失败关闭。生产环境不存在激活入
// 口，本文件不改变任何默认行为。
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
)

// ApplyCompactionVariant 把压缩参数变体的四项可生效值应用到基线配置副本
// 上并复核整体合法性；它是变体应用的唯一实现，评测臂装配与试运行解析共
// 用，防止两处公式漂移。
func ApplyCompactionVariant(base config.Config, variant memory.CompactionParamsVariant) (config.Config, error) {
	if err := variant.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("apply compaction variant: %w", err)
	}
	expanded := base
	expanded.Context.MaxPromptTokens = variant.MaxPromptTokens
	expanded.Context.MaxSummaryBytes = variant.MaxSummaryBytes
	expanded.Context.InventoryBudgetPercent = variant.InventoryBudgetPercent
	expanded.Context.ThresholdReservePercent = variant.ThresholdReservePercent
	if err := config.Validate(expanded); err != nil {
		return config.Config{}, fmt.Errorf("activated variant config invalid: %w", err)
	}
	return expanded, nil
}

// ResolveTrialActivation 解析沙箱环境中当前激活的压缩参数版本并应用到配
// 置副本。三态语义：激活存在且健康→返回覆盖后的配置与真；无激活（含已
// 回退）→原配置与假；记忆库缺失或指针指向非激活版本→失败关闭——用户显
// 式请求试运行时，沉默回基线等于伪造实验条件。
func (service Service) ResolveTrialActivation(ctx context.Context, workspace, stateDir string, configuration config.Config) (config.Config, bool, error) {
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		return config.Config{}, false, err
	}
	store, err := openReadOnlyMemory(stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return config.Config{}, false, fmt.Errorf("trial activation requires a memory store with an active promotion; none exists at this state directory")
		}
		return config.Config{}, false, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, stateDir, workspaceHash); err != nil {
		return config.Config{}, false, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	activation, found, err := store.ActivationFor(memory.TargetKindContextCompactionParams)
	if err != nil {
		return config.Config{}, false, err
	}
	if !found {
		return configuration, false, nil
	}
	promotions, err := store.ListPromotions(memory.TargetKindContextCompactionParams)
	if err != nil {
		return config.Config{}, false, err
	}
	for _, promotion := range promotions {
		if promotion.ID != activation.PromotionID {
			continue
		}
		if promotion.Status != memory.PromotionStatusSandboxActivated {
			return config.Config{}, false, fmt.Errorf("trial activation points at version %s in state %s; the ledger is inconsistent", promotion.ID, promotion.Status)
		}
		if promotion.SchemaVersion != memory.PromotionSchemaVersion {
			return config.Config{}, false, fmt.Errorf("trial activation points at legacy success evidence for version %s", promotion.ID)
		}
		resolved, applyErr := ApplyCompactionVariant(configuration, promotion.Variant)
		if applyErr != nil {
			return config.Config{}, false, fmt.Errorf("apply activated version %s: %w", promotion.ID, applyErr)
		}
		return resolved, true, nil
	}
	return config.Config{}, false, fmt.Errorf("trial activation references version %s missing from the ledger", activation.PromotionID)
}
