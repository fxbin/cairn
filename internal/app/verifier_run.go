// [INPUT]: 依赖父运行 Journal、只读 Verifier 配置与子 Run 执行入口。
// [OUTPUT]: 启动独立只读 Verifier 子运行并返回待核验 Finding。
// [POS]: 复用既有委派与子运行语义，不实现第二套 Agent Loop。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件把独立 Verifier 接入既有子运行编排。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// VerifierResult 是一次独立验证的可审计输出。Findings 仅为待核验发现；
// VerifierRunID 绑定独立子 Run Journal，供后续 Finding Validation 追溯。
type VerifierResult struct {
	VerifierRunID domain.RunID
	Findings      []contract.Finding
	FinalText     string
}

// RunVerifier 主动发起独立只读 Verifier 子运行。它先写父 Journal 的委派事实，
// 再以只读配置启动子 Run，最后从子 Journal 终态解析结构化 Finding。子 Run 不
// 继承 Worker 推理历史、不修改产物、预算受父约束；解析失败关闭。
func (service Service) RunVerifier(ctx context.Context, request VerifierRequest) (VerifierResult, error) {
	if err := validateVerifierRequest(request); err != nil {
		return VerifierResult{}, err
	}
	childConfig, err := verifierConfig(request.Config)
	if err != nil {
		return VerifierResult{}, err
	}
	// 打开父 Journal 供预算钳制与委派事实共用；工作区哈希必须与子 Run 一致。
	hash := verifierWorkspaceHash(request)
	journal, err := openJournal(request.StateDir, hash)
	if err != nil {
		return VerifierResult{}, err
	}
	defer journal.Close()
	// 预算钳制：Verifier 轮数不得超过父配置额度，也不得超过父已消耗后的余量。
	// 复用 delegate 的同一口径，避免把父全额预算借给独立验证。
	if request.Config.Budget.MaxTurns > 0 {
		opened, err := parentOpenedTurns(ctx, journal, request.ParentRunID)
		if err != nil {
			return VerifierResult{}, err
		}
		remaining := request.Config.Budget.MaxTurns - opened
		if remaining < 1 {
			return VerifierResult{}, errors.New("parent run has no remaining turns for verification")
		}
		if childConfig.Budget.MaxTurns <= 0 || childConfig.Budget.MaxTurns > remaining {
			childConfig.Budget.MaxTurns = remaining
		}
	}
	childID := domain.NewRunID()
	prompt := verifierTaskPrompt(request)
	// 委派事实先于子 Run 落盘：validateSubagentRunParent 要求父 Journal 中
	// 存在该子身份的 delegated 事实，否则子运行启动失败。
	if _, err := RecordSubagentDelegated(ctx, journal, request.ParentRunID, hash, DelegateOptions{
		ParentRunID: request.ParentRunID,
		ChildRunID:  childID,
		Task:        prompt,
		Budget:      SubagentBudget{MaxTurns: childConfig.Budget.MaxTurns},
	}, time.Now().UTC()); err != nil {
		return VerifierResult{}, err
	}
	_, runErr := service.Run(ctx, RunOptions{
		Task:             prompt,
		Workspace:        request.Workspace,
		StateDir:         request.StateDir,
		Config:           childConfig,
		PreassignedRunID: childID,
		ParentRunID:      request.ParentRunID,
		ParentTools:      request.ParentTools,
	})
	// 子运行失败、取消或等待审批时不解析其输出，避免来自失败运行的发现被使用。
	if runErr != nil {
		return VerifierResult{}, runErr
	}
	events, loadErr := journal.Load(ctx, childID)
	if loadErr != nil {
		return VerifierResult{}, loadErr
	}
	finalText := verifierFinalText(events)
	if finalText == "" {
		return VerifierResult{}, fmt.Errorf("%w: no final text from verifier run", ErrVerifierOutput)
	}
	findings, parseErr := parseVerifierOutput(finalText, request)
	if parseErr != nil {
		return VerifierResult{}, parseErr
	}
	return VerifierResult{VerifierRunID: childID, Findings: findings, FinalText: finalText}, nil
}

// validateVerifierRequest 校验主动验证请求的必需绑定，缺一失败关闭。
func validateVerifierRequest(request VerifierRequest) error {
	if request.ParentRunID == "" || request.TargetRunID == "" || request.Workspace == "" || request.StateDir == "" {
		return errors.New("verifier request requires parent, target, workspace and state directory")
	}
	if request.ParentRunID == request.TargetRunID {
		return errors.New("verifier parent and target must differ")
	}
	if err := request.Acceptance.Validate(); err != nil {
		return fmt.Errorf("verifier acceptance: %w", err)
	}
	if len(request.ParentTools) == 0 {
		return errors.New("verifier request requires the parent tool surface")
	}
	return nil
}

// verifierWorkspaceHash 复用 workspaceFor 的哈希口径，保证委派事实与子 Run
// 绑定同一工作区身份。
func verifierWorkspaceHash(request VerifierRequest) string {
	_, hash, err := workspaceFor(request.Workspace, request.Config)
	if err != nil {
		return ""
	}
	return hash
}
