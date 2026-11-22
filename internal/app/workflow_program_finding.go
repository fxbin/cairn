// [INPUT]: 依赖冻结验收计划、Program Journal 与命令结果制品。
// [OUTPUT]: 仅为已核验的必需检查失败生成确认 Finding。
// [POS]: 为报告和返工提供证据。读取历史，不授权执行。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件核验程序检查失败的来源及终态。
// Author: fxbin
package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/runtime/artifact"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

const (
	workflowProgramFailureCategory = "program_check_failed"
	workflowProgramFailureReviewer = "workflow-program"
	// 程序可各输出一份 stdout 和 stderr。读取上限包含最坏 JSON 转义余量。
	workflowProgramReceiptMaxBytes = 16 * contract.ExecMaxOutputBytesLimit
	// 124 及以上包含常见超时、容器启动、命令调用和信号退出码。
	workflowProgramReservedExitCode = 124
)

// workflowFailedCommand 保存同一验证中最近提议且已执行的失败操作。
type workflowFailedCommand struct {
	VerificationID string          `json:"verification_id"`
	CommandIndex   int             `json:"command_index"`
	Arguments      json.RawMessage `json:"arguments"`
	PlanRef        *contract.Ref   `json:"plan_ref"`
	terminal       domain.Event
}

// workflowProgramFailureFindings 将实际非零退出绑定到冻结命令及工作区版本。
// 调用方提供已重放的 Program Journal。观察不足与不可定论执行不产生确认。
func (execution workflowExecution) workflowProgramFailureFindings(run domain.RunProjection, events []domain.Event, input workflowVerificationInputs) ([]contract.Finding, error) {
	if run.State != domain.RunCompleted || run.VerificationSucceeded || run.LastVerificationSeq < 2 ||
		run.LastVerificationSeq >= uint64(len(events)) || run.ID != input.ProgramRunID || run.WorkspaceHash != execution.workspaceHash ||
		input.WorkerRunID == "" || input.WorkerRunID == run.ID || input.WorkspaceVersion == "" {
		return nil, nil
	}
	terminal := events[len(events)-1]
	finishIndex := int(run.LastVerificationSeq - 1)
	event := events[finishIndex]
	var finish domain.VerificationFinishedPayload
	if terminal.Type != domain.EventRunCompleted || event.Type != domain.EventVerificationFinished || event.TurnID == nil ||
		json.Unmarshal(event.Payload, &finish) != nil || finish.OK || finish.VerificationID == "" || finish.ResultFingerprint == "" {
		return nil, nil
	}
	version, err := workflowObservedRunVersion(run, events)
	if err != nil || version != input.WorkspaceVersion {
		return nil, nil
	}
	start, found := verificationStartedForFinish(events, finishIndex, *event.TurnID, finish.VerificationID)
	if !found || start.planRef == nil {
		return nil, nil
	}
	command, found := workflowFailedCommandOf(events[:finishIndex], *event.TurnID)
	if !found || command.VerificationID != finish.VerificationID ||
		command.PlanRef == nil || command.PlanRef.SHA256 != start.planRef.SHA256 {
		return nil, nil
	}
	store := artifact.Store{Root: manifestArtifactRoot(execution.stateDir, run.WorkspaceHash)}
	checkID, arguments, err := workflowBoundFailedCheck(&store, run, start, input.Acceptance, command)
	if err != nil || checkID == "" {
		return nil, err
	}
	ref, exit, err := workflowProgramExitReceipt(&store, command.terminal, arguments)
	if err != nil || ref == nil {
		return nil, err
	}
	return workflowConfirmedCheckFindings(input, checkID, exit, *ref)
}

// workflowFailedCommandOf 核对同轮最新提议、授权、实际启动与直接失败事实。
func workflowFailedCommandOf(events []domain.Event, turnID domain.TurnID) (workflowFailedCommand, bool) {
	var command workflowFailedCommand
	var started, allowed bool
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		switch event.Type {
		case domain.EventVerificationStarted:
			return workflowFailedCommand{}, false
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
			if command.terminal.OperationID != nil {
				continue
			}
			if event.Type != domain.EventOperationFailed || event.OperationID == nil {
				return workflowFailedCommand{}, false
			}
			command.terminal = event
		case domain.EventOperationStarted:
			if !workflowSameProgramOperation(event, command.terminal) {
				return workflowFailedCommand{}, false
			}
			var body struct {
				ToolName        string `json:"tool_name"`
				SideEffectClass string `json:"side_effect_class"`
				MutationBarrier bool   `json:"mutation_barrier"`
			}
			if started || json.Unmarshal(event.Payload, &body) != nil || body.ToolName != "exec_command" ||
				body.SideEffectClass != string(contract.SideEffectProcess) || body.MutationBarrier {
				return workflowFailedCommand{}, false
			}
			started = true
		case domain.EventPolicyDecided:
			if !started || !workflowSameProgramOperation(event, command.terminal) || allowed {
				continue
			}
			var body struct {
				Effect      string `json:"effect"`
				Fingerprint string `json:"fingerprint"`
			}
			if json.Unmarshal(event.Payload, &body) != nil || body.Effect != "allow" || body.Fingerprint == "" {
				return workflowFailedCommand{}, false
			}
			allowed = true
		case domain.EventVerificationCommandProposed:
			if !started || !allowed || !workflowSameProgramOperation(event, command.terminal) ||
				json.Unmarshal(event.Payload, &command) != nil {
				return workflowFailedCommand{}, false
			}
			return command, true
		}
	}
	return workflowFailedCommand{}, false
}

// workflowSameProgramOperation 判断两条事件是否绑定同一操作。
func workflowSameProgramOperation(left, right domain.Event) bool {
	return left.OperationID != nil && right.OperationID != nil && *left.OperationID == *right.OperationID
}

// workflowBoundFailedCheck 按验证制品解析指定检查，并比对提议实参。
func workflowBoundFailedCheck(store *artifact.Store, run domain.RunProjection, start verificationStartRecord, acceptance contract.AcceptanceContract, command workflowFailedCommand) (string, contract.ExecArguments, error) {
	var arguments contract.ExecArguments
	content, err := workflowReadProgramReceipt(store, *start.planRef)
	if err != nil {
		return "", arguments, err
	}
	var plan contract.VerificationPlanSpec
	if json.Unmarshal(content, &plan) != nil || plan.Version != contract.VerificationPlanVersion ||
		plan.VerificationID != start.verificationID || plan.ConfigFingerprint != run.ConfigFingerprint ||
		plan.CommandsFingerprint != start.commandsFingerprint || plan.LastMutationSeq != start.lastMutationSeq ||
		plan.Acceptance == nil || plan.Acceptance.Validate() != nil || acceptance.Validate() != nil ||
		plan.Acceptance.Digest != acceptance.Digest || command.CommandIndex < 0 || command.CommandIndex >= len(plan.Commands) {
		return "", arguments, fmt.Errorf("%w: frozen program verification plan does not match its journal", ErrStateCorrupt)
	}
	fingerprint, err := contract.FingerprintVerificationCommands(plan.Commands)
	if err != nil || fingerprint != plan.CommandsFingerprint {
		return "", arguments, fmt.Errorf("%w: invalid program verification command fingerprint", ErrStateCorrupt)
	}
	check := plan.Commands[command.CommandIndex]
	if !contract.ValidAcceptanceIdentifier(check.ID) || !workflowReceiptJSONEqual(check.Arguments, command.Arguments) {
		return "", arguments, fmt.Errorf("%w: proposed program check differs from its frozen command", ErrStateCorrupt)
	}
	arguments, err = contract.ParseExecArguments(check.Arguments)
	if err != nil {
		return "", arguments, fmt.Errorf("%w: invalid frozen program check arguments", ErrStateCorrupt)
	}
	return check.ID, arguments, nil
}

// workflowProgramExitReceipt 只认可有直接制品证明、无信号且正常退出的检查失败。
func workflowProgramExitReceipt(store *artifact.Store, event domain.Event, command contract.ExecArguments) (*contract.Ref, int, error) {
	var operation struct {
		ToolName  string          `json:"tool_name"`
		Result    json.RawMessage `json:"result"`
		ResultRef *contract.Ref   `json:"result_ref"`
	}
	if json.Unmarshal(event.Payload, &operation) != nil || operation.ToolName != "exec_command" || operation.ResultRef == nil {
		return nil, 0, nil
	}
	var result contract.ToolResult
	var exit struct {
		Argv     []string          `json:"argv"`
		Backend  tools.ExecBackend `json:"backend"`
		CWD      string            `json:"cwd"`
		ExitCode int               `json:"exit_code"`
		Signal   string            `json:"signal"`
		Stderr   string            `json:"stderr"`
	}
	if json.Unmarshal(operation.Result, &result) != nil || result.OK || result.Code != "nonzero_exit" ||
		json.Unmarshal(result.Data, &exit) != nil || exit.Signal != "" ||
		!workflowCheckReachedExit(exit.Backend, exit.Stderr) || exit.ExitCode < 1 || exit.ExitCode >= workflowProgramReservedExitCode {
		return nil, 0, nil
	}
	if !slices.Equal(exit.Argv, command.Argv) || exit.CWD != filepath.ToSlash(command.CWD) {
		return nil, 0, fmt.Errorf("%w: program check result differs from its frozen arguments", ErrStateCorrupt)
	}
	content, err := workflowReadProgramReceipt(store, *operation.ResultRef)
	if err != nil {
		return nil, 0, err
	}
	if !workflowReceiptJSONEqual(operation.Result, content) {
		return nil, 0, fmt.Errorf("%w: program result artifact differs from its journal event", ErrStateCorrupt)
	}
	ref := *operation.ResultRef
	ref.TruncatedPreview = ""
	return &ref, exit.ExitCode, nil
}

// workflowCheckReachedExit 排除包装器与容器启动失败。
func workflowCheckReachedExit(backend tools.ExecBackend, stderr string) bool {
	switch backend {
	case tools.ExecBackendHost:
		return true
	case tools.ExecBackendSeatbeltHost, tools.ExecBackendDocker:
		for line := range strings.SplitSeq(stderr, "\n") {
			text := strings.TrimSpace(line)
			if strings.HasPrefix(text, "bwrap:") || strings.HasPrefix(text, "sandbox-exec:") ||
				strings.HasPrefix(text, "docker:") || strings.HasPrefix(text, "Error response from daemon:") {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// workflowReadProgramReceipt 有界读取并校验计划或结果制品。
func workflowReadProgramReceipt(store *artifact.Store, ref contract.Ref) ([]byte, error) {
	content, err := store.OpenBounded(ref, workflowProgramReceiptMaxBytes)
	if err != nil || ref.MediaType != "application/json" || ref.Bytes != int64(len(content)) {
		return nil, fmt.Errorf("%w: invalid program verification artifact", ErrStateCorrupt)
	}
	return content, nil
}

// workflowReceiptJSONEqual 按规范 JSON 比较已持久化的制品与事件。
func workflowReceiptJSONEqual(left, right json.RawMessage) bool {
	first, err := canonicaljson.Marshal(left)
	if err != nil {
		return false
	}
	second, err := canonicaljson.Marshal(right)
	return err == nil && bytes.Equal(first, second)
}

// workflowConfirmedCheckFindings 将一次已核验失败投影到绑定该检查的必需标准。
func workflowConfirmedCheckFindings(input workflowVerificationInputs, checkID string, exit int, ref contract.Ref) ([]contract.Finding, error) {
	var findings []contract.Finding
	for _, criterion := range input.Acceptance.Criteria {
		if !criterion.Required || !slices.Contains(criterion.Checks, checkID) {
			continue
		}
		digest := sha256.Sum256([]byte(string(input.ProgramRunID) + "\x00" + checkID + "\x00" + criterion.ID))
		finding := contract.Finding{
			Version: contract.FindingVersion, ID: "program-" + hex.EncodeToString(digest[:]),
			TargetID: string(input.WorkerRunID), CriterionID: criterion.ID, Category: workflowProgramFailureCategory,
			Severity: contract.SeverityMedium, Status: contract.FindingConfirmed,
			Description:  fmt.Sprintf("验收检查 %s 以状态 %d 退出", checkID, exit),
			EvidenceRefs: []contract.Ref{ref},
			Reviews: []contract.FindingReview{{
				ReviewerID: workflowProgramFailureReviewer, Outcome: contract.FindingConfirmed,
				Rationale: "冻结检查确已启动。退出状态与结果制品一致。", EvidenceRefs: []contract.Ref{ref},
			}},
		}
		if err := finding.Validate(); err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, nil
}
