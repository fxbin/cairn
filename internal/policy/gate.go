// Package policy decides whether a proposed side effect is allowed, denied, or
// requires a single human approval.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
)

type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
	Ask   Effect = "ask"
)

type Gate struct {
	WriteMode      Effect
	ExecMode       Effect
	ProtectedPaths []string
	Patch          tools.ApplyPatch
}

type Decision struct {
	Effect               Effect
	RuleID               string
	Fingerprint          string
	Summary              string
	WorkspaceObservation string
}

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

func (gate *Gate) Decide(toolName string, raw json.RawMessage) (Decision, error) {
	switch toolName {
	case "list_files", "read_file", "search_text":
		return Decision{Effect: Allow, RuleID: "read_only", Fingerprint: fingerprint(toolName, raw), Summary: "read-only workspace access"}, nil
	case "apply_patch":
		var proposed struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(raw, &proposed); err != nil {
			return Decision{Effect: Deny, RuleID: "invalid_patch", Summary: err.Error()}, nil
		}
		for _, protected := range gate.ProtectedPaths {
			if matchesProtected(proposed.Path, protected) {
				return Decision{Effect: Deny, RuleID: "protected_path", Fingerprint: fingerprint("apply_patch", raw), Summary: "protected path: " + proposed.Path}, nil
			}
		}
		plan, err := gate.Patch.Plan(raw)
		if err != nil {
			return Decision{Effect: Deny, RuleID: "invalid_patch", Summary: err.Error()}, nil
		}
		return Decision{Effect: gate.WriteMode, RuleID: "workspace_write", Fingerprint: patchFingerprint(plan), Summary: fmt.Sprintf("%s %s (%s → %s)", plan.Mode, plan.Path, plan.BeforeHash, plan.AfterHash)}, nil
	case "exec_command":
		proposed, err := tools.ParseExecArguments(raw)
		if err != nil {
			return Decision{Effect: Deny, RuleID: "invalid_command", Summary: "invalid exec_command arguments"}, nil
		}
		_, relativeCWD, err := gate.Patch.Workspace.Resolve(proposed.CWD)
		if err != nil {
			return Decision{Effect: Deny, RuleID: "invalid_command", Summary: "invalid exec_command cwd"}, nil
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
			Summary:              "execute " + strings.Join(proposed.Argv, " "),
			WorkspaceObservation: observation,
		}, nil
	default:
		return Decision{Effect: Deny, RuleID: "unknown_tool", Fingerprint: fingerprint(toolName, raw), Summary: "tool is not allowed"}, nil
	}
}

// DecideVerification derives a distinct approval fingerprint for a fixed
// verification plan. A command that happens to have the same argv as a model
// tool cannot consume or inherit this approval.
func (gate *Gate) DecideVerification(raw json.RawMessage, planRef string, commandIndex int, configFingerprint string) (Decision, error) {
	decision, err := gate.Decide("exec_command", raw)
	if err != nil || decision.Effect == Deny {
		return decision, err
	}
	return gate.DecideVerificationWithObservation(raw, planRef, commandIndex, configFingerprint, decision.WorkspaceObservation)
}

// DecideVerificationWithObservation binds a frozen verification operation to
// the verifier's whole-workspace observation. Model command approvals retain
// their tool-visible observation; verification integrity must instead observe
// every non-volatile repository path.
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

func patchFingerprint(plan tools.PatchPlan) string {
	return fingerprint("apply_patch", struct {
		Version string `json:"version"`
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Before  string `json:"before"`
		After   string `json:"after"`
	}{Version: "v1", Path: plan.Path, Mode: plan.Mode, Before: plan.BeforeHash, After: plan.AfterHash})
}

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
