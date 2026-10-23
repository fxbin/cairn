package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

const automaticVerificationResponse = "Cairn runtime: frozen verification passed after successful apply_patch."

// VerificationCommand 是 Loop 在完成前按配置执行的冻结命令。
type VerificationCommand struct {
	ID        string
	Arguments json.RawMessage
}

// verificationPlan 是持久化为制品并绑定审批的验证计划快照。
type verificationPlan struct {
	Version             string                `json:"version"`
	VerificationID      string                `json:"verification_id"`
	ConfigFingerprint   string                `json:"config_fingerprint"`
	LastMutationSeq     uint64                `json:"last_mutation_seq"`
	CommandsFingerprint string                `json:"commands_fingerprint"`
	Commands            []VerificationCommand `json:"commands"`
}

// verificationProposalPayload 是验证命令提议事件的可回放载荷。
type verificationProposalPayload struct {
	VerificationID string          `json:"verification_id"`
	CommandIndex   int             `json:"command_index"`
	PlanRef        *artifact.Ref   `json:"plan_ref,omitempty"`
	Arguments      json.RawMessage `json:"arguments"`
}

// verificationPersistenceError 表示验证已留下可恢复的持久化事实，但后续
// finished 或 feedback 证据写入失败；调用方必须保留开放 Run 供 Resume 补齐。
type verificationPersistenceError struct {
	cause error
}

// Error 返回不包含验证输出正文的稳定持久化失败说明。
func (err *verificationPersistenceError) Error() string {
	return fmt.Sprintf("persist verification evidence: %v", err.cause)
}

// Unwrap 暴露底层存储错误，便于调用方保留原始失败分类。
func (err *verificationPersistenceError) Unwrap() error {
	return err.cause
}

// runVerification 创建并执行冻结验证计划，任何失败都会反馈给后续模型轮次。
func (loop *Loop) runVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) error {
	if err := loop.quiesceMCP(ctx, projection, input); err != nil {
		return err
	}
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return fmt.Errorf("load run before verification: %w", err)
	}
	if outcome, found, outcomeErr := completedVerificationForTurn(events, turnID); outcomeErr != nil {
		return outcomeErr
	} else if found {
		if outcome.OK {
			return loop.requireCompletedVerificationObservation(events, turnID, projection)
		}
		if !outcome.OK && outcome.Feedback != "" {
			return &ports.VerificationFailureError{CommandID: outcome.CommandID, Feedback: outcome.Feedback}
		}
		return nil
	}
	// 冻结计划无命令时一律以 OK:false 完成（ADR-0017：完成与可信分离）。
	// 有命令时必须真实执行，包括 last_mutation_seq==0：零变更不能跳过验证，
	// 否则模型可在未修复时拿到可信完成（发布阻断）。
	if len(loop.Verification) == 0 {
		return loop.finishUnverifiedRun(ctx, projection, input, turnID)
	}
	verificationID := domain.NewID("verify")
	plan, planRef, err := loop.createVerificationPlan(verificationID, projection.LastMutationSeq)
	if err != nil {
		return err
	}
	observation, err := loop.verificationObservation()
	if err != nil {
		return fmt.Errorf("observe workspace before verification plan: %w", err)
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: mustJSON(struct {
		VerificationID       string        `json:"verification_id"`
		MutationSeq          uint64        `json:"last_mutation_seq"`
		PlanRef              *artifact.Ref `json:"plan_ref,omitempty"`
		CommandsFingerprint  string        `json:"commands_fingerprint"`
		WorkspaceObservation string        `json:"workspace_observation"`
	}{VerificationID: verificationID, MutationSeq: projection.LastMutationSeq, PlanRef: planRef, CommandsFingerprint: plan.CommandsFingerprint, WorkspaceObservation: observation})}); err != nil {
		return err
	}
	return loop.runVerificationCommands(ctx, projection, input, turnID, verificationID, planRef, plan.CommandsFingerprint, 0)
}

// finishUnverifiedRun 处理"有工作区变更但未配置验证命令"的完成（ADR-0017）：
// 完成事实与可信等级分离——如实发出零命令验证的 started 与 finished
// {OK:false}（变更未经验证），随后按既有路径落 run_completed。完成可信
// 判定公式将自动把该完成归为不可信，证据等级不弱化。
func (loop *Loop) finishUnverifiedRun(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) error {
	verificationID := domain.NewID("verify")
	plan, planRef, err := loop.createVerificationPlan(verificationID, projection.LastMutationSeq)
	if err != nil {
		return err
	}
	observation, err := loop.verificationObservation()
	if err != nil {
		return fmt.Errorf("observe workspace before verification plan: %w", err)
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: mustJSON(struct {
		VerificationID       string        `json:"verification_id"`
		MutationSeq          uint64        `json:"last_mutation_seq"`
		PlanRef              *artifact.Ref `json:"plan_ref,omitempty"`
		CommandsFingerprint  string        `json:"commands_fingerprint"`
		WorkspaceObservation string        `json:"workspace_observation"`
	}{VerificationID: verificationID, MutationSeq: projection.LastMutationSeq, PlanRef: planRef, CommandsFingerprint: plan.CommandsFingerprint, WorkspaceObservation: observation})}); err != nil {
		return err
	}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{VerificationID: verificationID, OK: false})}); err != nil {
		return &verificationPersistenceError{cause: err}
	}
	return nil
}

// createVerificationPlan 冻结验证命令及配置指纹，并将计划作为脱敏制品保存。
func (loop *Loop) createVerificationPlan(verificationID string, lastMutationSeq uint64) (verificationPlan, *artifact.Ref, error) {
	commandsFingerprint, err := fingerprintVerificationCommands(loop.Verification)
	if err != nil {
		return verificationPlan{}, nil, err
	}
	plan := verificationPlan{
		Version:             contract.VerificationPlanVersion,
		VerificationID:      verificationID,
		ConfigFingerprint:   loop.ConfigFingerprint,
		LastMutationSeq:     lastMutationSeq,
		CommandsFingerprint: commandsFingerprint,
		Commands:            append([]VerificationCommand(nil), loop.Verification...),
	}
	if loop.Artifacts == nil {
		return plan, nil, nil
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return verificationPlan{}, nil, fmt.Errorf("encode verification plan: %w", err)
	}
	ref, err := loop.Artifacts.Put(encoded, "application/json")
	if err != nil {
		return verificationPlan{}, nil, fmt.Errorf("store verification plan: %w", err)
	}
	return plan, &ref, nil
}

// runVerificationCommands 从指定索引顺序执行冻结计划中的验证命令。
func (loop *Loop) runVerificationCommands(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, verificationID string, planRef *artifact.Ref, commandsFingerprint string, startIndex int) error {
	commands, err := loop.verificationCommands(verificationID, planRef, commandsFingerprint)
	if err != nil {
		return err
	}
	if startIndex < 0 || startIndex > len(commands) {
		return fmt.Errorf("verification command start index %d is outside frozen plan", startIndex)
	}
	for index := startIndex; index < len(commands); index++ {
		if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
			return err
		}
		command := commands[index]
		operationID := domain.NewOperationID()
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationCommandProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(verificationProposalPayload{VerificationID: verificationID, CommandIndex: index, PlanRef: planRef, Arguments: command.Arguments})}); err != nil {
			return err
		}
		planSHA := ""
		if planRef != nil {
			planSHA = planRef.SHA256
		}
		observation, err := loop.verificationObservation()
		if err != nil {
			return fmt.Errorf("observe workspace for verification approval: %w", err)
		}
		decision, err := loop.Gate.DecideVerificationWithObservation(command.Arguments, planSHA, index, loop.ConfigFingerprint, observation)
		if err != nil {
			return err
		}
		decision = freshApprovalDecision(*projection, decision)
		if err := loop.append(ctx, projection, policyDecisionDraft(input, turnID, operationID, decision)); err != nil {
			return err
		}
		if decision.Effect == policy.Ask {
			if err := loop.append(ctx, projection, approvalRequestedDraft(input, turnID, operationID, decision)); err != nil {
				return err
			}
			return &ports.WaitingApprovalError{RunID: input.RunID, OperationID: operationID, Fingerprint: decision.Fingerprint, Summary: decision.Summary}
		}
		if decision.Effect == policy.Deny {
			result := policyFailureResult(decision)
			commitContext, cancel := loop.commitContext(ctx)
			defer cancel()
			if err := loop.append(commitContext, projection, domain.EventDraft{
				RunID:         input.RunID,
				WorkspaceHash: input.WorkspaceHash,
				Type:          domain.EventOperationFailed,
				TurnID:        &turnID,
				OperationID:   &operationID,
				Payload:       mustJSON(operationPayload{ToolName: "exec_command", Result: result}),
			}); err != nil {
				return &verificationPersistenceError{cause: err}
			}
			if err := loop.append(commitContext, projection, domain.EventDraft{
				RunID:         input.RunID,
				WorkspaceHash: input.WorkspaceHash,
				Type:          domain.EventVerificationFinished,
				TurnID:        &turnID,
				Payload: mustJSON(domain.VerificationFinishedPayload{
					VerificationID:    verificationID,
					OK:                false,
					ResultFingerprint: stableToolResultFingerprint(result),
				}),
			}); err != nil {
				return &verificationPersistenceError{cause: err}
			}
			return &ports.PolicyDeniedError{Summary: decision.Summary}
		}
		if err := loop.executeVerificationCommand(ctx, projection, input, turnID, operationID, command, observation); err != nil {
			return err
		}
	}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{VerificationID: verificationID, OK: true})}); err != nil {
		return &verificationPersistenceError{cause: err}
	}
	return nil
}

// verificationCommands 从持久化计划恢复冻结命令；没有制品引用时仅在当前
// 配置摘要与 started 事件一致时兼容恢复，缺少摘要或发生漂移均失败关闭。
func (loop *Loop) verificationCommands(verificationID string, planRef *artifact.Ref, commandsFingerprint string) ([]VerificationCommand, error) {
	if planRef == nil {
		commands := append([]VerificationCommand(nil), loop.Verification...)
		if err := validateVerificationCommands(commands); err != nil {
			return nil, err
		}
		if commandsFingerprint == "" {
			return nil, errors.New("verification plan has neither an artifact reference nor a persisted command fingerprint")
		}
		currentFingerprint, err := fingerprintVerificationCommands(commands)
		if err != nil {
			return nil, err
		}
		if currentFingerprint != commandsFingerprint {
			return nil, errors.New("current verification commands do not match the persisted command fingerprint")
		}
		return commands, nil
	}
	if loop.Artifacts == nil {
		return nil, errors.New("artifact store is required to resume a frozen verification plan")
	}
	content, err := loop.Artifacts.Open(*planRef)
	if err != nil {
		return nil, fmt.Errorf("open frozen verification plan: %w", err)
	}
	var plan verificationPlan
	if err := json.Unmarshal(content, &plan); err != nil {
		return nil, fmt.Errorf("decode frozen verification plan: %w", err)
	}
	if plan.Version != contract.VerificationPlanVersion || plan.VerificationID != verificationID {
		return nil, errors.New("frozen verification plan identity does not match the persisted verification")
	}
	if err := validateVerificationCommands(plan.Commands); err != nil {
		return nil, err
	}
	actualFingerprint, err := fingerprintVerificationCommands(plan.Commands)
	if err != nil {
		return nil, err
	}
	if plan.CommandsFingerprint != actualFingerprint {
		return nil, errors.New("frozen verification plan command fingerprint mismatch")
	}
	if commandsFingerprint != "" && plan.CommandsFingerprint != commandsFingerprint {
		return nil, errors.New("frozen verification plan does not match the persisted command fingerprint")
	}
	return append([]VerificationCommand(nil), plan.Commands...), nil
}

// fingerprintVerificationCommands 为验证命令序列计算稳定摘要，用于在无制品
// 存储时检测恢复配置漂移，并校验制品内计划未被替换。
func fingerprintVerificationCommands(commands []VerificationCommand) (string, error) {
	projection := make([]contract.VerificationCommandSpec, 0, len(commands))
	for _, command := range commands {
		projection = append(projection, contract.VerificationCommandSpec{ID: command.ID, Arguments: command.Arguments})
	}
	return contract.FingerprintVerificationCommands(projection)
}

// FingerprintVerificationCommands 返回冻结验证命令序列的规范化稳定摘要，
// 供应用层能力清单与事件侧指纹比对共用同一算法。
func FingerprintVerificationCommands(commands []VerificationCommand) (string, error) {
	return fingerprintVerificationCommands(commands)
}

// validateVerificationCommands 拒绝无法安全重放的冻结命令。
func validateVerificationCommands(commands []VerificationCommand) error {
	for index, command := range commands {
		if strings.TrimSpace(command.ID) == "" || !json.Valid(command.Arguments) {
			return fmt.Errorf("verification command %d is incomplete", index)
		}
	}
	return nil
}

// verificationCommand 返回冻结计划中的指定命令。
func (loop *Loop) verificationCommand(verificationID string, planRef *artifact.Ref, commandsFingerprint string, index int) (VerificationCommand, error) {
	commands, err := loop.verificationCommands(verificationID, planRef, commandsFingerprint)
	if err != nil {
		return VerificationCommand{}, err
	}
	if index < 0 || index >= len(commands) {
		return VerificationCommand{}, errors.New("verification command index does not match frozen plan")
	}
	return commands[index], nil
}

// completedVerificationOutcome 描述当前轮次已经落盘的验证结果及可选反馈。
type completedVerificationOutcome struct {
	OK        bool
	CommandID string
	Feedback  string
}

// completedVerificationForTurn 返回同一轮次最新的验证完成事实；只消费该完成
// 事件之后的反馈，避免旧验证失败污染后续成功验证。
func completedVerificationForTurn(events []domain.Event, turnID domain.TurnID) (completedVerificationOutcome, bool, error) {
	finishedIndex := -1
	var outcome completedVerificationOutcome
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationFinished || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload domain.VerificationFinishedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return completedVerificationOutcome{}, false, fmt.Errorf("decode verification_finished payload: %w", err)
		}
		outcome.OK = payload.OK
		finishedIndex = index
		break
	}
	if finishedIndex < 0 {
		return completedVerificationOutcome{}, false, nil
	}
	for index := finishedIndex + 1; index < len(events); index++ {
		event := events[index]
		if event.Type != domain.EventVerificationFeedbackAppended || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return completedVerificationOutcome{}, false, fmt.Errorf("decode verification feedback: %w", err)
		}
		outcome.Feedback = payload.Text
	}
	return outcome, true, nil
}

// incompleteVerificationRecord 保存最近一次尚无 finished 事件的验证计划。
type incompleteVerificationRecord struct {
	turnID               domain.TurnID
	startIndex           int
	verificationID       string
	planRef              *artifact.Ref
	commandsFingerprint  string
	lastMutationSeq      uint64
	workspaceObservation string
}

// latestIncompleteVerification 按事件顺序配对 started/finished；旧 finished 未带
// verification_id 时仍关闭同轮次最近一次验证。
func latestIncompleteVerification(events []domain.Event) (incompleteVerificationRecord, bool, error) {
	var active *incompleteVerificationRecord
	for index, event := range events {
		switch event.Type {
		case domain.EventVerificationStarted:
			if event.TurnID == nil {
				continue
			}
			var payload struct {
				VerificationID       string        `json:"verification_id"`
				PlanRef              *artifact.Ref `json:"plan_ref"`
				CommandsFingerprint  string        `json:"commands_fingerprint"`
				LastMutationSeq      uint64        `json:"last_mutation_seq"`
				WorkspaceObservation string        `json:"workspace_observation"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return incompleteVerificationRecord{}, false, fmt.Errorf("decode verification_started payload: %w", err)
			}
			active = &incompleteVerificationRecord{
				turnID:               *event.TurnID,
				startIndex:           index,
				verificationID:       payload.VerificationID,
				planRef:              payload.PlanRef,
				commandsFingerprint:  payload.CommandsFingerprint,
				lastMutationSeq:      payload.LastMutationSeq,
				workspaceObservation: payload.WorkspaceObservation,
			}
		case domain.EventVerificationFinished:
			if active == nil || event.TurnID == nil || *event.TurnID != active.turnID {
				continue
			}
			var payload domain.VerificationFinishedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return incompleteVerificationRecord{}, false, fmt.Errorf("decode verification_finished payload: %w", err)
			}
			if payload.VerificationID == "" || active.verificationID == "" || payload.VerificationID == active.verificationID {
				active = nil
			}
		}
	}
	if active == nil {
		return incompleteVerificationRecord{}, false, nil
	}
	return *active, true, nil
}

// verificationOperationRecord 关联冻结命令提议与其既有操作终态。
type verificationOperationRecord struct {
	operationID         domain.OperationID
	proposal            verificationProposalPayload
	terminal            domain.Event
	approvalFingerprint string
	policyFingerprint   string
	policySummary       string
	policyAsked         bool
	approvalRequested   bool
	hasResult           bool
	authorized          bool
	policyAuthorized    bool
	started             bool
	policyDenied        bool
}

// verificationOperations 返回未完成验证中按 command_index 排列的命令记录。
func verificationOperations(events []domain.Event, verification incompleteVerificationRecord) ([]verificationOperationRecord, error) {
	records := make(map[domain.OperationID]*verificationOperationRecord)
	maxIndex := -1
	for index := verification.startIndex + 1; index < len(events); index++ {
		event := events[index]
		if event.OperationID == nil || event.TurnID == nil || *event.TurnID != verification.turnID {
			continue
		}
		operationID := *event.OperationID
		switch event.Type {
		case domain.EventVerificationCommandProposed:
			var payload verificationProposalPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode verification command proposal: %w", err)
			}
			if verification.verificationID != "" && payload.VerificationID != verification.verificationID {
				continue
			}
			if payload.CommandIndex < 0 {
				return nil, errors.New("verification command index must not be negative")
			}
			copyRecord := &verificationOperationRecord{operationID: operationID, proposal: payload}
			records[operationID] = copyRecord
			if payload.CommandIndex > maxIndex {
				maxIndex = payload.CommandIndex
			}
		case domain.EventPolicyDecided:
			record, found := records[operationID]
			if !found {
				continue
			}
			var payload policyDecisionPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode verification policy decision: %w", err)
			}
			record.policyFingerprint = payload.Fingerprint
			record.policySummary = payload.Summary
			switch payload.Effect {
			case string(policy.Allow):
				record.authorized = true
				record.policyAuthorized = true
			case string(policy.Ask):
				record.policyAsked = true
			case string(policy.Deny):
				record.policyDenied = true
			}
		case domain.EventApprovalRequested:
			if record, found := records[operationID]; found {
				record.approvalRequested = true
			}
		case domain.EventApprovalGranted:
			record, found := records[operationID]
			if !found {
				continue
			}
			var payload struct {
				Fingerprint string `json:"fingerprint"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode verification approval grant: %w", err)
			}
			record.approvalFingerprint = payload.Fingerprint
			record.authorized = true
		case domain.EventOperationStarted:
			if record, found := records[operationID]; found {
				record.started = true
			}
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
			if record, found := records[operationID]; found {
				record.terminal = event
				record.hasResult = true
			}
		}
	}
	if maxIndex < 0 {
		return nil, nil
	}
	ordered := make([]verificationOperationRecord, maxIndex+1)
	seen := make([]bool, maxIndex+1)
	for _, record := range records {
		commandIndex := record.proposal.CommandIndex
		if seen[commandIndex] {
			return nil, fmt.Errorf("verification command index %d was proposed more than once", commandIndex)
		}
		ordered[commandIndex] = *record
		seen[commandIndex] = true
	}
	for index, exists := range seen {
		if !exists {
			return nil, fmt.Errorf("verification command index %d is missing before a later proposal", index)
		}
	}
	return ordered, nil
}

// repairIncompleteVerification 从既有命令终态继续冻结验证；已执行命令绝不
// 重跑，失败终态只补 finished 与反馈，成功终态从下一命令继续。
func (loop *Loop) repairIncompleteVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (bool, error) {
	verification, found, err := latestIncompleteVerification(events)
	if err != nil {
		return false, err
	}
	if !found {
		return loop.repairMissingVerificationFeedback(ctx, projection, input, events)
	}
	if projection.CurrentTurn == nil || *projection.CurrentTurn != verification.turnID {
		return false, nil
	}
	operations, err := verificationOperations(events, verification)
	if err != nil {
		return false, err
	}
	if len(operations) != 0 {
		latest := operations[len(operations)-1]
		if latest.hasResult && latest.terminal.Type == domain.EventOperationCancelled {
			return false, nil
		}
	}
	verificationID := verification.verificationID
	planRef := verification.planRef
	if verificationID == "" && len(operations) != 0 {
		verificationID = operations[0].proposal.VerificationID
	}
	if planRef == nil && len(operations) != 0 {
		planRef = operations[0].proposal.PlanRef
	}
	commands, err := loop.verificationCommands(verificationID, planRef, verification.commandsFingerprint)
	if err != nil {
		return false, err
	}
	if len(operations) == 0 {
		if len(commands) != 0 {
			if err := loop.requireFrozenVerificationObservation(verification, projection); err != nil {
				return false, err
			}
			runErr := loop.runVerificationCommands(ctx, projection, input, verification.turnID, verificationID, planRef, verification.commandsFingerprint, 0)
			var failure *ports.VerificationFailureError
			if errors.As(runErr, &failure) {
				return true, nil
			}
			return true, runErr
		}
		commitContext, cancel := loop.commitContext(ctx)
		defer cancel()
		if err := loop.append(commitContext, projection, domain.EventDraft{
			RunID:         input.RunID,
			WorkspaceHash: input.WorkspaceHash,
			Type:          domain.EventVerificationFinished,
			TurnID:        &verification.turnID,
			Payload:       mustJSON(domain.VerificationFinishedPayload{VerificationID: verificationID, OK: false}),
		}); err != nil {
			return false, &verificationPersistenceError{cause: err}
		}
		return true, nil
	}
	latest := operations[len(operations)-1]
	if !latest.hasResult {
		if latest.started {
			return false, nil
		}
		if err := loop.requireFrozenVerificationObservation(verification, projection); err != nil {
			return false, err
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if latest.proposal.CommandIndex >= len(commands) {
			return false, errors.New("verification command index exceeds frozen plan")
		}
		command := commands[latest.proposal.CommandIndex]
		if latest.policyAsked && !latest.authorized {
			if latest.approvalRequested {
				return false, nil
			}
			decision := policy.Decision{Effect: policy.Ask, Fingerprint: latest.policyFingerprint, Summary: latest.policySummary}
			if err := loop.append(ctx, projection, approvalRequestedDraft(input, verification.turnID, latest.operationID, decision)); err != nil {
				return false, err
			}
			return true, &ports.WaitingApprovalError{RunID: input.RunID, OperationID: latest.operationID, Fingerprint: latest.policyFingerprint, Summary: latest.policySummary}
		}
		observation, err := loop.verificationObservation()
		if err != nil {
			return false, fmt.Errorf("observe workspace before resuming verification: %w", err)
		}
		planSHA := ""
		if planRef != nil {
			planSHA = planRef.SHA256
		}
		decision, err := loop.Gate.DecideVerificationWithObservation(command.Arguments, planSHA, latest.proposal.CommandIndex, loop.ConfigFingerprint, observation)
		if err != nil {
			return false, err
		}
		decision = freshApprovalDecision(*projection, decision)
		if !latest.authorized {
			if err := loop.append(ctx, projection, policyDecisionDraft(input, verification.turnID, latest.operationID, decision)); err != nil {
				return false, err
			}
			switch decision.Effect {
			case policy.Ask:
				if err := loop.append(ctx, projection, approvalRequestedDraft(input, verification.turnID, latest.operationID, decision)); err != nil {
					return false, err
				}
				return true, &ports.WaitingApprovalError{RunID: input.RunID, OperationID: latest.operationID, Fingerprint: decision.Fingerprint, Summary: decision.Summary}
			case policy.Deny:
				result := policyFailureResult(decision)
				feedback := "Verification command was denied by policy. Inspect the frozen command and policy before retrying."
				if err := loop.finishUnstartedVerification(ctx, projection, input, verification.turnID, latest.operationID, verificationID, result, feedback); err != nil {
					return false, err
				}
				return true, nil
			}
		} else {
			if !verificationAuthorizationMatches(latest, decision) {
				reason := "approval expired because the command, policy, or workspace observation changed; re-decision required"
				result := tools.Failure("approval_stale", reason)
				feedback := "Verification approval expired after workspace drift. Re-inspect the workspace and decide whether to repair or rerun verification."
				if err := loop.finishUnstartedVerification(ctx, projection, input, verification.turnID, latest.operationID, verificationID, result, feedback); err != nil {
					return false, err
				}
				return true, nil
			}
			if !latest.policyAuthorized {
				if latest.approvalFingerprint == "" {
					return false, errors.New("approved verification command has no persisted approval fingerprint")
				}
				approved := policy.Decision{Effect: policy.Allow, RuleID: "approved_once", Fingerprint: latest.approvalFingerprint}
				if err := loop.append(ctx, projection, policyDecisionDraft(input, verification.turnID, latest.operationID, approved)); err != nil {
					return false, err
				}
			}
		}
		runErr := loop.executeVerificationCommand(ctx, projection, input, verification.turnID, latest.operationID, command, observation)
		var failure *ports.VerificationFailureError
		if errors.As(runErr, &failure) {
			return true, nil
		}
		if runErr != nil {
			return true, runErr
		}
		runErr = loop.runVerificationCommands(ctx, projection, input, verification.turnID, verificationID, planRef, verification.commandsFingerprint, latest.proposal.CommandIndex+1)
		if errors.As(runErr, &failure) {
			return true, nil
		}
		return true, runErr
	}
	if latest.proposal.CommandIndex >= len(commands) {
		return false, errors.New("verification command index exceeds frozen plan")
	}
	result, succeeded, err := verificationTerminalResult(latest.terminal)
	if err != nil {
		return false, err
	}
	_, cancelled, err := verificationCancellationCause(latest.terminal)
	if err != nil {
		return false, err
	}
	if succeeded {
		if err := loop.requireFrozenVerificationObservation(verification, projection); err != nil {
			return false, err
		}
		nextIndex := latest.proposal.CommandIndex + 1
		if nextIndex < len(commands) {
			runErr := loop.runVerificationCommands(ctx, projection, input, verification.turnID, verificationID, planRef, verification.commandsFingerprint, nextIndex)
			var failure *ports.VerificationFailureError
			if errors.As(runErr, &failure) {
				return true, nil
			}
			return true, runErr
		}
		commitContext, cancel := loop.commitContext(ctx)
		defer cancel()
		if err := loop.append(commitContext, projection, domain.EventDraft{
			RunID:         input.RunID,
			WorkspaceHash: input.WorkspaceHash,
			Type:          domain.EventVerificationFinished,
			TurnID:        &verification.turnID,
			Payload:       mustJSON(domain.VerificationFinishedPayload{VerificationID: verificationID, OK: true}),
		}); err != nil {
			return false, &verificationPersistenceError{cause: err}
		}
		return true, nil
	}
	command := commands[latest.proposal.CommandIndex]
	feedback := verificationFailureFeedback(command.ID, result)
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventVerificationFinished,
		TurnID:        &verification.turnID,
		Payload: mustJSON(domain.VerificationFinishedPayload{
			VerificationID:    verificationID,
			OK:                false,
			ResultFingerprint: stableToolResultFingerprint(result),
		}),
	}); err != nil {
		return false, &verificationPersistenceError{cause: err}
	}
	if cancelled {
		return true, nil
	}
	if result.Code == "approval_denied" || latest.policyDenied {
		return true, nil
	}
	if result.Code == "approval_stale" {
		feedback = "Verification approval expired after workspace drift. Re-inspect the workspace and decide whether to repair or rerun verification."
	}
	operationID := *latest.terminal.OperationID
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventVerificationFeedbackAppended,
		TurnID:        &verification.turnID,
		OperationID:   &operationID,
		Payload: mustJSON(struct {
			Text string `json:"text"`
		}{Text: feedback}),
	}); err != nil {
		return false, &verificationPersistenceError{cause: err}
	}
	return true, nil
}

// verificationAuthorizationMatches 判断恢复时重算的策略结论是否仍与持久化授权
// 完全一致；人工审批必须仍是同一 ask 指纹，直接授权必须仍是同一 allow 指纹。
func verificationAuthorizationMatches(record verificationOperationRecord, decision policy.Decision) bool {
	if record.approvalFingerprint != "" {
		return decision.Effect == policy.Ask && decision.Fingerprint == record.approvalFingerprint
	}
	return record.policyAuthorized &&
		record.policyFingerprint != "" &&
		decision.Effect == policy.Allow &&
		decision.Fingerprint == record.policyFingerprint
}

// finishUnstartedVerification 将未进入 operation_started 的验证命令闭合为失败，
// 并同步关闭验证覆盖层；可选反馈仅用于需要模型重新决策的非取消场景。
func (loop *Loop) finishUnstartedVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, operationID domain.OperationID, verificationID string, result tools.Result, feedback string) error {
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventOperationFailed,
		TurnID:        &turnID,
		OperationID:   &operationID,
		Payload:       mustJSON(operationPayload{ToolName: "exec_command", Result: result}),
	}); err != nil {
		return &verificationPersistenceError{cause: err}
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventVerificationFinished,
		TurnID:        &turnID,
		Payload: mustJSON(domain.VerificationFinishedPayload{
			VerificationID:    verificationID,
			OK:                false,
			ResultFingerprint: stableToolResultFingerprint(result),
		}),
	}); err != nil {
		return &verificationPersistenceError{cause: err}
	}
	if feedback == "" {
		return nil
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventVerificationFeedbackAppended,
		TurnID:        &turnID,
		OperationID:   &operationID,
		Payload: mustJSON(struct {
			Text string `json:"text"`
		}{Text: feedback}),
	}); err != nil {
		return &verificationPersistenceError{cause: err}
	}
	return nil
}

// finishVerificationForCancellation 在运行取消兜底中安全关闭尚未完成的验证：
// 已授权但未启动的命令记为取消；已有命令终态时只关闭验证覆盖层，绝不执行
// 冻结计划中的后续命令。
func (loop *Loop) finishVerificationForCancellation(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (bool, error) {
	verification, found, err := latestIncompleteVerification(events)
	if err != nil {
		return false, err
	}
	if !found || projection.CurrentTurn == nil || *projection.CurrentTurn != verification.turnID {
		return false, nil
	}
	operations, err := verificationOperations(events, verification)
	if err != nil {
		return false, err
	}
	if len(operations) == 0 {
		return false, nil
	}
	latest := operations[len(operations)-1]
	verificationID := verification.verificationID
	if verificationID == "" {
		verificationID = latest.proposal.VerificationID
	}
	if latest.hasResult {
		result, _, err := verificationTerminalResult(latest.terminal)
		if err != nil {
			return false, err
		}
		commitContext, cancel := loop.commitContext(ctx)
		defer cancel()
		if err := loop.append(commitContext, projection, domain.EventDraft{
			RunID:         input.RunID,
			WorkspaceHash: input.WorkspaceHash,
			Type:          domain.EventVerificationFinished,
			TurnID:        &verification.turnID,
			Payload: mustJSON(domain.VerificationFinishedPayload{
				VerificationID:    verificationID,
				OK:                false,
				ResultFingerprint: stableToolResultFingerprint(result),
			}),
		}); err != nil {
			return false, &verificationPersistenceError{cause: err}
		}
		return true, nil
	}
	if latest.started || !latest.authorized {
		return false, nil
	}
	result := tools.Failure("user_interrupt", "verification command was cancelled before execution because the run was stopping")
	result.Data = mustJSON(struct {
		StopCause string `json:"stop_cause"`
	}{StopCause: "user_interrupt"})
	if err := loop.finishUnstartedVerification(ctx, projection, input, verification.turnID, latest.operationID, verificationID, result, ""); err != nil {
		return false, err
	}
	return true, nil
}

// requireCompletedVerificationObservation 在复用成功验证或补写完成前，
// 用该 finished 对应的持久化 started 核对当前工作区，绝不重跑旧命令。
func (loop *Loop) requireCompletedVerificationObservation(events []domain.Event, turnID domain.TurnID, projection *domain.RunProjection) error {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationFinished || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload domain.VerificationFinishedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode completed verification: %w", err)
		}
		if !payload.OK {
			return nil
		}
		verification, found, err := latestIncompleteVerification(events[:index])
		if err != nil {
			return err
		}
		if !found || verification.turnID != turnID || (payload.VerificationID != "" && payload.VerificationID != verification.verificationID) {
			return errors.New("completed verification has no matching persisted start")
		}
		return loop.requireFrozenVerificationObservation(verification, projection)
	}
	return nil
}

// requireFrozenVerificationObservation 在续跑或认可成功终态前，确认工作区与
// 验证开始时同一视图；失败终态的如实收束不调用它。
func (loop *Loop) requireFrozenVerificationObservation(verification incompleteVerificationRecord, projection *domain.RunProjection) error {
	if verification.lastMutationSeq != projection.LastMutationSeq {
		return errors.New("workspace mutation sequence changed since verification started")
	}
	if verification.workspaceObservation == "" {
		return errors.New("verification has no persisted workspace observation")
	}
	observation, err := loop.verificationObservation()
	if err != nil {
		return fmt.Errorf("observe workspace before resuming verification: %w", err)
	}
	if observation != verification.workspaceObservation {
		return errors.New("workspace changed since verification started; restore it before resuming verification")
	}
	return nil
}

// repairMissingVerificationFeedback 补写失败验证在 verification_finished 之后
// 遗漏的模型反馈；它只消费既有操作终态，不重新执行命令，也不把人工拒绝或
// 策略拒绝伪装为验证命令执行失败。
func (loop *Loop) repairMissingVerificationFeedback(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (bool, error) {
	if projection.CurrentTurn == nil {
		return false, nil
	}
	turnID := *projection.CurrentTurn
	finishedIndex := -1
	var finished domain.VerificationFinishedPayload
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationFinished || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		if err := json.Unmarshal(event.Payload, &finished); err != nil {
			return false, fmt.Errorf("decode verification_finished payload: %w", err)
		}
		finishedIndex = index
		break
	}
	if finishedIndex < 0 || finished.OK {
		return false, nil
	}
	for index := finishedIndex + 1; index < len(events); index++ {
		event := events[index]
		if event.Type == domain.EventVerificationFeedbackAppended && event.TurnID != nil && *event.TurnID == turnID {
			return false, nil
		}
	}
	var verification incompleteVerificationRecord
	foundStart := false
	for index := finishedIndex - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationStarted || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload struct {
			VerificationID      string        `json:"verification_id"`
			PlanRef             *artifact.Ref `json:"plan_ref"`
			CommandsFingerprint string        `json:"commands_fingerprint"`
			LastMutationSeq     uint64        `json:"last_mutation_seq"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return false, fmt.Errorf("decode verification_started payload: %w", err)
		}
		if finished.VerificationID != "" && payload.VerificationID != "" && finished.VerificationID != payload.VerificationID {
			continue
		}
		verification = incompleteVerificationRecord{
			turnID:              turnID,
			startIndex:          index,
			verificationID:      payload.VerificationID,
			planRef:             payload.PlanRef,
			commandsFingerprint: payload.CommandsFingerprint,
			lastMutationSeq:     payload.LastMutationSeq,
		}
		foundStart = true
		break
	}
	if !foundStart {
		return false, nil
	}
	operations, err := verificationOperations(events[:finishedIndex], verification)
	if err != nil {
		return false, err
	}
	if len(operations) == 0 {
		return false, nil
	}
	latest := operations[len(operations)-1]
	if !latest.hasResult || latest.terminal.OperationID == nil {
		return false, nil
	}
	result, succeeded, err := verificationTerminalResult(latest.terminal)
	if err != nil {
		return false, err
	}
	_, cancelled, err := verificationCancellationCause(latest.terminal)
	if err != nil {
		return false, err
	}
	if cancelled {
		return false, nil
	}
	if succeeded || result.Code == "approval_denied" || latest.policyDenied {
		return false, nil
	}
	verificationID := verification.verificationID
	planRef := verification.planRef
	if verificationID == "" {
		verificationID = latest.proposal.VerificationID
	}
	if planRef == nil {
		planRef = latest.proposal.PlanRef
	}
	command, err := loop.verificationCommand(verificationID, planRef, verification.commandsFingerprint, latest.proposal.CommandIndex)
	if err != nil {
		return false, err
	}
	feedback := fmt.Sprintf("Verification command %s failed (%s). Inspect the persisted result, repair the workspace, and run the configured verification again. Evidence: %s", command.ID, result.Code, result.Preview)
	if result.Code == "approval_stale" {
		feedback = "Verification approval expired after workspace drift. Re-inspect the workspace and decide whether to repair or rerun verification."
	}
	operationID := *latest.terminal.OperationID
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventVerificationFeedbackAppended,
		TurnID:        &turnID,
		OperationID:   &operationID,
		Payload: mustJSON(struct {
			Text string `json:"text"`
		}{Text: feedback}),
	}); err != nil {
		return false, &verificationPersistenceError{cause: err}
	}
	return true, nil
}

// verificationTerminalResult 将验证操作终态归一化为结果与成功标志。
func verificationTerminalResult(event domain.Event) (tools.Result, bool, error) {
	if event.Type == domain.EventOperationReconciled {
		var payload struct {
			Verdict string `json:"verdict"`
			Note    string `json:"note"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return tools.Result{}, false, fmt.Errorf("decode operation_reconciled payload: %w", err)
		}
		switch payload.Verdict {
		case "applied_success":
			result := tools.Result{OK: true, Code: "reconciled_applied_success", Preview: payload.Note}
			return result, true, nil
		case "applied_failure", "not_applied", "abort_run":
			result := tools.Result{OK: false, Code: "reconciled_" + payload.Verdict, Preview: payload.Note}
			return result, false, nil
		default:
			return tools.Result{}, false, fmt.Errorf("operation_reconciled has invalid verdict %q", payload.Verdict)
		}
	}
	result, err := toolResultFromOperationTerminal(event)
	if err != nil {
		return tools.Result{}, false, err
	}
	return result, event.Type == domain.EventOperationSucceeded && result.OK, nil
}

// verificationCancellationCause 从未启动验证的失败终态中提取结构化取消原因；
// 仅 user_interrupt 可接管运行终态，普通验证失败不能由文本或失败码推断为取消。
func verificationCancellationCause(event domain.Event) (string, bool, error) {
	if event.Type != domain.EventOperationFailed {
		return "", false, nil
	}
	var payload struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return "", false, fmt.Errorf("decode verification cancellation operation: %w", err)
	}
	if len(payload.Result) == 0 || string(payload.Result) == "null" {
		return "", false, nil
	}
	var result tools.Result
	if err := json.Unmarshal(payload.Result, &result); err != nil {
		return "", false, fmt.Errorf("decode verification cancellation result: %w", err)
	}
	if len(result.Data) == 0 || string(result.Data) == "null" {
		return "", false, nil
	}
	var data struct {
		StopCause string `json:"stop_cause"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return "", false, fmt.Errorf("decode verification cancellation data: %w", err)
	}
	if data.StopCause != "user_interrupt" {
		return "", false, nil
	}
	return data.StopCause, true, nil
}

// hasRecoverableTerminalGap 报告取消处理前必须优先补齐的既有终态证据。
func hasRecoverableTerminalGap(events []domain.Event) bool {
	repairs, err := PendingUnstartedOperationRepairs(events)
	if err != nil || len(repairs) != 0 {
		return true
	}
	type toolRecord struct {
		terminal bool
		result   bool
	}
	toolsByOperation := make(map[domain.OperationID]*toolRecord)
	for _, event := range events {
		if event.OperationID == nil {
			continue
		}
		operationID := *event.OperationID
		switch event.Type {
		case domain.EventToolCallProposed:
			toolsByOperation[operationID] = &toolRecord{}
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
			if record, found := toolsByOperation[operationID]; found {
				record.terminal = true
			}
		case domain.EventToolResultAppended:
			if record, found := toolsByOperation[operationID]; found {
				record.result = true
			}
		}
	}
	for _, record := range toolsByOperation {
		if record.terminal && !record.result {
			return true
		}
	}
	verification, found, err := latestIncompleteVerification(events)
	if err != nil || !found {
		return err != nil
	}
	operations, err := verificationOperations(events, verification)
	if err != nil {
		return true
	}
	for _, operation := range operations {
		if operation.authorized && !operation.started && !operation.hasResult {
			return true
		}
		if operation.hasResult && operation.terminal.Type != domain.EventOperationCancelled {
			return true
		}
	}
	return false
}

// persistedVerificationCommandsFingerprint 返回当前验证计划在 started 事件中
// 持久化的命令摘要；制品引用存在时允许旧事件缺省摘要。
func persistedVerificationCommandsFingerprint(events []domain.Event, turnID domain.TurnID, verificationID string) (string, error) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationStarted || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload struct {
			VerificationID      string `json:"verification_id"`
			CommandsFingerprint string `json:"commands_fingerprint"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return "", fmt.Errorf("decode verification_started payload: %w", err)
		}
		if verificationID != "" && payload.VerificationID != "" && payload.VerificationID != verificationID {
			continue
		}
		return payload.CommandsFingerprint, nil
	}
	return "", errors.New("verification proposal has no matching started event")
}

// equalVerificationArguments 对合法 JSON 参数做空白无关的字节比较，确保执行
// 的命令仍与已持久化提议一致。
func equalVerificationArguments(proposed json.RawMessage, command json.RawMessage) bool {
	var proposedCompact bytes.Buffer
	if err := json.Compact(&proposedCompact, proposed); err != nil {
		return false
	}
	var commandCompact bytes.Buffer
	if err := json.Compact(&commandCompact, command); err != nil {
		return false
	}
	return bytes.Equal(proposedCompact.Bytes(), commandCompact.Bytes())
}

// policyDecisionDraft 将策略决定编码为可追加的领域事件草稿。
func policyDecisionDraft(input RunInput, turnID domain.TurnID, operationID domain.OperationID, decision policy.Decision) domain.EventDraft {
	return domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventPolicyDecided,
		TurnID:        &turnID,
		OperationID:   &operationID,
		Payload: mustJSON(policyDecisionPayload{
			Effect:      string(decision.Effect),
			Rule:        decision.RuleID,
			Fingerprint: decision.Fingerprint,
			Summary:     decision.Summary,
			FailureCode: decision.FailureCode,
			FailureData: append(json.RawMessage(nil), decision.FailureData...),
		}),
	}
}

// approvalRequestedDraft 将待审批操作编码为可追加的领域事件草稿。
func approvalRequestedDraft(input RunInput, turnID domain.TurnID, operationID domain.OperationID, decision policy.Decision) domain.EventDraft {
	return domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalRequested, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Summary     string `json:"summary"`
		Rule        string `json:"rule"`
	}{Fingerprint: decision.Fingerprint, Summary: decision.Summary, Rule: decision.RuleID})}
}

// executeVerificationCommand 在策略授权后执行单条冻结验证命令并记录证据。
// beforeObservation 是调用方在审批决定时已计算的同一 verifier 视图观察值；
// 两次计算之间只发生事件追加（工作区已加锁且无工具执行），因此可安全复用，
// 避免同一无变更窗口内的重复全树扫描。
func (loop *Loop) executeVerificationCommand(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, operationID domain.OperationID, command VerificationCommand, beforeObservation string) error {
	if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
		return err
	}
	if loop.Gate == nil {
		return errors.New("policy gate is required for verification")
	}
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return fmt.Errorf("load verification proposal before command execution: %w", err)
	}
	proposal, found := proposedVerificationCommand(events, operationID)
	if !found {
		return errors.New("verification operation has no replayable proposal")
	}
	if !equalVerificationArguments(proposal.Arguments, command.Arguments) {
		return errors.New("verification command arguments do not match the persisted proposal")
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: "exec_command", SideEffectClass: string(tools.SideEffectProcess), MutationBarrier: false})}); err != nil {
		return err
	}
	result := redactToolResult(loop.Tools.Execute(ctx, "exec_command", command.Arguments))
	if result.Code == tools.CodeTerminationUnconfirmed {
		return &ports.WaitingReconcileError{RunID: input.RunID, OperationID: operationID, Reason: result.Preview}
	}
	eventType := domain.EventOperationSucceeded
	cancelled := result.Code == "cancelled" && ctx.Err() != nil
	if cancelled {
		eventType = domain.EventOperationCancelled
	} else if !result.OK {
		eventType = domain.EventOperationFailed
	}
	if result.OK {
		afterObservation, observationErr := loop.verificationObservation()
		if observationErr != nil {
			return fmt.Errorf("observe workspace after verification: %w", observationErr)
		}
		if afterObservation != beforeObservation {
			result = tools.Failure("verification_mutated_workspace", "verification command modified the tool-visible workspace; repair or rerun verification before completion")
			eventType = domain.EventOperationFailed
		}
	}
	resultContext, cancel := loop.commitContext(ctx)
	defer cancel()
	payload := loop.resultPayload("exec_command", result)
	if err := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(payload)}); err != nil {
		return err
	}
	if cancelled {
		return ctx.Err()
	}
	if result.OK {
		return nil
	}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{VerificationID: proposal.VerificationID, OK: false, ResultFingerprint: stableToolResultFingerprint(result)})}); err != nil {
		return &verificationPersistenceError{cause: err}
	}
	feedback := fmt.Sprintf("Verification command %s failed (%s). Inspect the result, repair the workspace, and run the configured verification again. Evidence: %s", command.ID, result.Code, result.Preview)
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFeedbackAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Text string `json:"text"`
	}{Text: feedback})}); err != nil {
		return &verificationPersistenceError{cause: err}
	}
	if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
		return err
	}
	return &ports.VerificationFailureError{CommandID: command.ID, Feedback: feedback}
}

// verificationObservation 返回 verifier 视图的稳定摘要，用于绑定命令审批。
func (loop *Loop) verificationObservation() (string, error) {
	if loop.VerificationWorkspace.Root != "" {
		return loop.VerificationWorkspace.Observation()
	}
	if loop.Gate == nil {
		return "", errors.New("policy gate is required for workspace observation")
	}
	return loop.Gate.Patch.Workspace.Observation()
}

// finishAutomaticVerification 在同一模型响应的所有工具调用成功结束且有补丁写入后，
// 执行冻结 verifier；通过时直接完成，失败时保留既有证据并返回给下一模型轮次。
func (loop *Loop) finishAutomaticVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) (Result, string, error) {
	if err := loop.runVerification(ctx, projection, input, turnID); err != nil {
		var persistence *verificationPersistenceError
		if errors.As(err, &persistence) {
			return Result{RunID: input.RunID}, "", err
		}
		if errors.Is(err, ports.ErrWaitingReconcile) {
			return Result{RunID: input.RunID}, "", err
		}
		if ctx.Err() != nil {
			result, cancellationErr := loop.persistCancellation(ctx, projection, input, &turnID)
			return result, "", cancellationErr
		}
		var waiting *ports.WaitingApprovalError
		if errors.As(err, &waiting) {
			return Result{RunID: input.RunID}, "", waiting
		}
		var budget *ports.BudgetExceededError
		if errors.As(err, &budget) {
			result, budgetErr := loop.persistBudgetExceeded(ctx, projection, input, budget.Dimension)
			return result, "", budgetErr
		}
		var guard *ports.LoopGuardExceededError
		if errors.As(err, &guard) {
			return Result{RunID: input.RunID}, "", loop.persistLoopGuardExceeded(ctx, projection, input, guard.Reason)
		}
		var verificationFailure *ports.VerificationFailureError
		if errors.As(err, &verificationFailure) {
			return Result{RunID: input.RunID}, verificationFailure.Feedback, nil
		}
		result, failureErr := loop.failCurrentTurn(ctx, projection, input, turnID, err)
		return result, "", failureErr
	}
	result, completionErr := loop.completeAutomaticVerification(ctx, projection, input, turnID)
	return result, "", completionErr
}

// completeAutomaticVerification 以明确标记的运行时结论关闭已通过自动验证的运行，
// 不将空工具调用响应伪装为模型最终文本。
func (loop *Loop) completeAutomaticVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) (Result, error) {
	response := loop.completionResponse(*projection)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: response})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	return Result{RunID: input.RunID, Response: response}, nil
}

// completionResponse 返回已持久化的模型最终消息；自动验证没有模型最终文本时，
// 返回可辨识的 Cairn 运行时结论。
func (loop *Loop) completionResponse(projection domain.RunProjection) string {
	if strings.TrimSpace(projection.LastAssistantMessage) != "" {
		return projection.LastAssistantMessage
	}
	return automaticVerificationResponse
}
