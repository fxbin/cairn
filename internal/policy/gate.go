// Package policy 决定提议的副作用是允许、拒绝还是需要一次人工审批。
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
)

// Effect 是策略判定的三种可能结果。
type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
	Ask   Effect = "ask"
)

// Gate 保存副作用策略、受保护路径和 patch 规划器。
type Gate struct {
	WriteMode      Effect
	ExecMode       Effect
	ProtectedPaths []string
	Patch          tools.ApplyPatch
	MCPRegistry    *tools.Registry
}

// Decision 是可持久化的策略结论及其审批绑定信息。
type Decision struct {
	MCPIdentity          string
	Effect               Effect
	RuleID               string
	Fingerprint          string
	Summary              string
	WorkspaceObservation string
	FailureCode          string
	FailureData          json.RawMessage
}

// New 解析策略模式并复制受保护路径，构造副作用决策门。
func New(writeMode, execMode string, protected []string, patch tools.ApplyPatch) (*Gate, error) {
	write, err := parseEffect(writeMode, Ask)
	if err != nil {
		return nil, err
	}
	exec, err := parseEffect(execMode, Ask)
	if err != nil {
		return nil, err
	}
	return &Gate{WriteMode: write, ExecMode: exec, ProtectedPaths: append([]string(nil), protected...), Patch: patch}, nil
}

// Decide 对模型提议的固定工具调用作出策略判定，并为审批生成绑定指纹。
// 注意：ProtectedPaths 是防篡改边界而非保密边界——只读工具对受保护路径恒
// allow 是有意设计（敏感文件已由工作区层 sensitivePath/ignore 拦截），模型
// 需要读取（如受写保护的配置文件）才能完成任务的场景不因此被阻断。
func (gate *Gate) Decide(toolName string, raw json.RawMessage) (Decision, error) {
	// 显式拒绝标记下拒绝写/执行（向后兼容：无标记视为信任）。
	if toolName == "exec_command" || toolName == "apply_patch" {
		if err := tools.GuardWrite(gate.Patch.Workspace.Root, toolName); err != nil {
			return Decision{
				Effect:      Deny,
				RuleID:      "untrusted_workspace",
				Fingerprint: fingerprint(toolName, raw),
				Summary:     err.Error(),
			}, nil
		}
	}
	// 危险命令在任何模式下都拒绝，并给出重写提示。
	if toolName == "exec_command" {
		var proposed struct {
			Argv []string `json:"argv"`
		}
		if err := json.Unmarshal(raw, &proposed); err == nil {
			argv := strings.Join(proposed.Argv, " ")
			if dangerous := DetectDangerousCommand(toolName, argv); dangerous != nil {
				return Decision{
					Effect:      Deny,
					RuleID:      "dangerous_command",
					Fingerprint: fingerprint(toolName, raw),
					Summary:     dangerous.Reason + "；" + dangerous.RewriteHint,
				}, nil
			}
		}
	}
	switch toolName {
	case "list_files", "read_file", "search_text", "skill":
		return Decision{Effect: Allow, RuleID: "read_only", Fingerprint: fingerprint(toolName, raw), Summary: "read-only workspace access"}, nil
	case "apply_patch":
		var proposed struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(raw, &proposed); err != nil {
			return invalidPatchDecision(raw, &tools.PatchArgumentsError{Message: err.Error()}), nil
		}
		if proposed.Path == "" {
			return invalidPatchDecision(raw, &tools.PatchArgumentsError{Message: "path is required"}), nil
		}
		for _, protected := range gate.ProtectedPaths {
			if matchesProtected(proposed.Path, protected) {
				return Decision{Effect: Deny, RuleID: "protected_path", Fingerprint: fingerprint("apply_patch", raw), Summary: "protected path: " + proposed.Path}, nil
			}
		}
		_, relative, err := gate.Patch.Workspace.Resolve(proposed.Path)
		if err != nil {
			return invalidPatchDecision(raw, err), nil
		}
		if gate.protectsResolvedPath(proposed.Path, relative) {
			return Decision{Effect: Deny, RuleID: "protected_path", Fingerprint: fingerprint("apply_patch", raw), Summary: "protected path: " + proposed.Path}, nil
		}
		plan, err := gate.Patch.Plan(raw)
		if err != nil {
			return invalidPatchDecision(raw, err), nil
		}
		if plan.Path != relative && gate.protectsResolvedPath(proposed.Path, plan.Path) {
			return Decision{Effect: Deny, RuleID: "protected_path", Fingerprint: fingerprint("apply_patch", raw), Summary: "protected path: " + proposed.Path}, nil
		}
		return Decision{Effect: gate.WriteMode, RuleID: "workspace_write", Fingerprint: patchFingerprint(plan), Summary: fmt.Sprintf("%s %s (%s → %s)", plan.Mode, plan.Path, plan.BeforeHash, plan.AfterHash)}, nil
	case "exec_command":
		proposed, err := tools.ParseExecArguments(raw)
		if err != nil {
			return Decision{Effect: Deny, RuleID: "invalid_command", Fingerprint: fingerprint("exec_command", raw), Summary: "invalid exec_command arguments"}, nil
		}
		_, relativeCWD, err := gate.Patch.Workspace.Resolve(proposed.CWD)
		if err != nil {
			return Decision{Effect: Deny, RuleID: "invalid_command", Fingerprint: fingerprint("exec_command", raw), Summary: "invalid exec_command cwd"}, nil
		}
		observation, err := gate.Patch.Workspace.Observation()
		if err != nil {
			return Decision{}, fmt.Errorf("observe workspace for command approval: %w", err)
		}
		proposed.CWD = relativeCWD
		return Decision{
			Effect:               gate.ExecMode,
			RuleID:               "process_execution",
			Fingerprint:          execFingerprint(proposed, observation),
			Summary:              "execute " + quoteArgv(proposed.Argv),
			WorkspaceObservation: observation,
		}, nil
	default:
		if gate.MCPRegistry != nil {
			proposal, found, err := gate.MCPRegistry.MCPProposal(toolName, raw)
			if err != nil {
				return Decision{Effect: Deny, RuleID: "invalid_mcp_call", Fingerprint: fingerprint(toolName, raw), Summary: "invalid or unavailable MCP tool"}, nil
			}
			if found {
				return gate.decideMCP(proposal)
			}
		}
		return Decision{Effect: Deny, RuleID: "unknown_tool", Fingerprint: fingerprint(toolName, raw), Summary: "tool is not allowed"}, nil
	}
}

// decideMCP 将本地服务器权限、目录摘要和实参绑定到现有审批链；注解不授予权限。
func (gate *Gate) decideMCP(proposal tools.MCPProposal) (Decision, error) {
	effect, err := parseEffect(proposal.Effect, Ask)
	if err != nil {
		return Decision{}, err
	}
	if proposal.Stdio {
		if gate.ExecMode == Deny {
			effect = Deny
		} else if gate.ExecMode == Ask && effect == Allow {
			effect = Ask
		}
	}
	observation := ""
	if proposal.Stdio {
		observation, err = gate.Patch.Workspace.Observation()
		if err != nil {
			return Decision{}, errors.New("observe workspace for MCP approval failed")
		}
	}
	return Decision{MCPIdentity: proposal.EnvironmentDigest, Effect: effect, RuleID: "mcp_tool", Summary: proposal.Summary, WorkspaceObservation: observation, Fingerprint: fingerprint("mcp_call", struct {
		Proposal             tools.MCPProposal `json:"proposal"`
		WorkspaceObservation string            `json:"workspace_observation,omitempty"`
	}{Proposal: proposal, WorkspaceObservation: observation})}, nil
}

// invalidPatchDecision 保持规划失败的拒绝和操作指纹，同时把参数、精确匹配及
// 过期摘要分别映射为可恢复反馈；未知错误与权限边界不因此得到授权。
func invalidPatchDecision(raw json.RawMessage, cause error) Decision {
	decision := Decision{Effect: Deny, RuleID: "invalid_patch", Fingerprint: fingerprint("apply_patch", raw), Summary: cause.Error()}
	feedback := tools.PatchFailureResult(cause)
	if feedback.Code == tools.CodeInvalidPatchArguments || feedback.Code == tools.CodePatchTextMismatch {
		decision.FailureCode = feedback.Code
		decision.FailureData = feedback.Data
		decision.Summary = feedback.Preview
		return decision
	}
	var stale *tools.PatchPreconditionError
	if !errors.As(cause, &stale) {
		return decision
	}
	data, err := json.Marshal(struct {
		Path          string `json:"path"`
		CurrentSHA256 string `json:"current_sha256"`
		NextAction    string `json:"next_action"`
	}{
		Path:          stale.Path,
		CurrentSHA256: stale.ActualSHA256,
		NextAction:    "call read_file for this path, then copy its sha256 into a new apply_patch request",
	})
	if err != nil {
		return decision
	}
	decision.RuleID = "stale_patch_precondition"
	decision.FailureCode = tools.CodeStalePrecondition
	decision.FailureData = data
	decision.Summary = "patch precondition is stale for " + stale.Path + "; call read_file before retrying apply_patch"
	return decision
}

// DecideVerification 为冻结验证计划生成独立审批指纹；即使 argv 相同，模型工具
// 命令也不能消费或继承该审批。
func (gate *Gate) DecideVerification(raw json.RawMessage, planRef string, commandIndex int, configFingerprint string) (Decision, error) {
	decision, err := gate.Decide("exec_command", raw)
	if err != nil || decision.Effect == Deny {
		return decision, err
	}
	return gate.DecideVerificationWithObservation(raw, planRef, commandIndex, configFingerprint, decision.WorkspaceObservation)
}

// DecideVerificationWithObservation 将冻结验证操作绑定到 verifier 的全工作区
// 观察值；模型命令仍使用工具视图，而验证完整性必须覆盖每个非 volatile 路径。
func (gate *Gate) DecideVerificationWithObservation(raw json.RawMessage, planRef string, commandIndex int, configFingerprint, observation string) (Decision, error) {
	decision, err := gate.Decide("exec_command", raw)
	if err != nil || decision.Effect == Deny {
		return decision, err
	}
	if observation == "" {
		return Decision{}, fmt.Errorf("verification workspace observation is required")
	}
	decision.RuleID = "verification_process_execution"
	decision.Fingerprint = fingerprint("verification_command", struct {
		Version              string `json:"version"`
		PlanRef              string `json:"plan_ref"`
		CommandIndex         int    `json:"command_index"`
		ConfigFingerprint    string `json:"config_fingerprint"`
		CommandFingerprint   string `json:"command_fingerprint"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Version: "v1", PlanRef: planRef, CommandIndex: commandIndex, ConfigFingerprint: configFingerprint, CommandFingerprint: decision.Fingerprint, WorkspaceObservation: observation})
	decision.WorkspaceObservation = observation
	decision.Summary = fmt.Sprintf("verify command %d: %s", commandIndex+1, decision.Summary)
	return decision, nil
}

// parseEffect 解析可选策略字符串，并在缺省时返回指定的保守默认值。
func parseEffect(value string, fallback Effect) (Effect, error) {
	if value == "" {
		return fallback, nil
	}
	effect := Effect(value)
	if effect != Allow && effect != Deny && effect != Ask {
		return "", fmt.Errorf("invalid policy effect %q", value)
	}
	return effect, nil
}

// patchFingerprint 为确定性 patch 计划计算审批绑定摘要。
func patchFingerprint(plan tools.PatchPlan) string {
	return fingerprint("apply_patch", struct {
		Version string `json:"version"`
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Before  string `json:"before"`
		After   string `json:"after"`
	}{Version: "v1", Path: plan.Path, Mode: plan.Mode, Before: plan.BeforeHash, After: plan.AfterHash})
}

// execFingerprint 将命令实参和执行前工作区观察值绑定为审批摘要。
func execFingerprint(arguments tools.ExecArguments, observation string) string {
	return fingerprint("exec_command", struct {
		Version              string            `json:"version"`
		Argv                 []string          `json:"argv"`
		CWD                  string            `json:"cwd"`
		TimeoutMS            int               `json:"timeout_ms"`
		MaxOutputBytes       int               `json:"max_output_bytes"`
		Env                  map[string]string `json:"env,omitempty"`
		WorkspaceObservation string            `json:"workspace_observation"`
	}{
		Version: "v1", Argv: arguments.Argv, CWD: arguments.CWD,
		TimeoutMS: arguments.TimeoutMS, MaxOutputBytes: arguments.MaxOutputBytes,
		Env: arguments.Env, WorkspaceObservation: observation,
	})
}

// fingerprint 对命名 JSON 载荷计算稳定 SHA-256 引用。
func fingerprint(name string, value any) string {
	encoded, err := json.Marshal(struct {
		Name  string `json:"name"`
		Value any    `json:"value"`
	}{Name: name, Value: value})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// protectsResolvedPath 同时检查清理后的逻辑路径和 Workspace.Resolve 的真实
// 目标；字面受保护路径自身为内部链接时也保护其目标，glob 保留原匹配语义。
func (gate *Gate) protectsResolvedPath(logicalPath, resolvedPath string) bool {
	logicalPath = path.Clean(strings.ReplaceAll(logicalPath, "\\", "/"))
	for _, protected := range gate.ProtectedPaths {
		if matchesProtected(logicalPath, protected) || matchesProtected(resolvedPath, protected) {
			return true
		}
		literal := strings.Trim(strings.ReplaceAll(protected, "\\", "/"), "/")
		if literal == "" || strings.ContainsAny(literal, "*?[") {
			continue
		}
		_, resolvedProtected, err := gate.Patch.Workspace.Resolve(literal)
		// 根目录的规范相对路径为 "."，其后代不会携带 "./" 前缀；根目标覆盖整个工作区。
		if err == nil && (resolvedProtected == "." || resolvedPath == resolvedProtected || strings.HasPrefix(resolvedPath, resolvedProtected+"/")) {
			return true
		}
	}
	return false
}

// matchesProtected 判断候选路径是否命中受保护路径、glob 或其子路径。
func matchesProtected(candidate, protected string) bool {
	protected = strings.Trim(strings.ReplaceAll(protected, "\\", "/"), "/")
	candidate = strings.Trim(strings.ReplaceAll(candidate, "\\", "/"), "/")
	if protected == "" {
		return false
	}
	matched, err := path.Match(protected, candidate)
	if err == nil && matched {
		return true
	}
	base := path.Base(candidate)
	if strings.HasPrefix(protected, "**/") {
		matched, err = path.Match(strings.TrimPrefix(protected, "**/"), base)
		if err == nil && matched {
			return true
		}
	}
	if !strings.Contains(protected, "/") {
		matched, err = path.Match(protected, base)
		if err == nil && matched {
			return true
		}
	}
	return candidate == protected || strings.HasPrefix(candidate, protected+"/")
}

// quoteArgv 逐参数加引号渲染命令行，使含空格、分号等字符的实参无法在审批
// 摘要中伪装成多条命令或纯展示文本；每个参数的边界对审批人可见。
func quoteArgv(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, argument := range argv {
		quoted = append(quoted, strconv.Quote(argument))
	}
	return "[" + strings.Join(quoted, " ") + "]"
}
