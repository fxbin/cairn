// 本文件装配仅属于当前 Boot 的记忆与消息工具回调。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/runtime/memory"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// bootTools 将工具依赖绑定到当前 Boot；正常执行、恢复与对账共用此装配。
// 记忆范围由服务端工作区强制确定，消息仅写本实例持有的 Journal。
// 最小组合不构造回调；回调不改变工具分类或绕过 Loop 的 Policy 授权链。
func (service Service) bootTools(boot *Boot, stateDir string) []tools.Tool {
	if boot.Config.Preset == config.PresetMinimal {
		return presetTools(boot.Workspace, boot.ExecTool, boot.Config.Preset, boot.Config.Tools)
	}
	explain := tools.MemoryExplainFunc(func(ctx context.Context, request tools.MemoryRecallRequest) (json.RawMessage, error) {
		ledger, err := service.ExplainMemoryRecall(ctx, MemoryExplainOptions{
			Query: memory.RecallQuery{
				TaskPromptDigest:    request.TaskPromptDigest,
				Outcomes:            request.Outcomes,
				FailureClasses:      request.FailureClasses,
				TrustedVerification: request.TrustedVerification,
				CreatedFrom:         request.CreatedFrom,
				CreatedUntil:        request.CreatedUntil,
				Limit:               request.Limit,
				Precision:           request.Precision,
			},
			Workspace: boot.Workspace.Root,
			StateDir:  stateDir,
			Config:    boot.Config,
			Now:       time.Now().UTC(),
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(ledger)
	})
	deliver := tools.MessageDeliverFunc(func(ctx context.Context, toRunID, content, reason string) error {
		return deliverRunMessage(ctx, boot.Journal, boot.Artifacts, boot.WorkspaceHash, domain.RunID(toRunID), content, reason)
	})
	return presetToolsWithCallbacks(boot.Workspace, boot.ExecTool, boot.Config.Preset, boot.Config.Tools, explain, deliver)
}
