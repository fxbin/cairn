// memory 系列子命令。
// Author: fxbin
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/benchmark"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

func (command Command) memory(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "memory requires capture, recall, explain, inspect, revoke, consolidate, reject, candidate, promotion, or telemetry")
		return ExitUsage
	}
	switch arguments[0] {
	case "capture":
		return command.memoryCapture(ctx, arguments[1:])
	case "recall":
		return command.memoryRecall(ctx, arguments[1:])
	case "explain":
		return command.memoryExplain(ctx, arguments[1:])
	case "inspect":
		return command.memoryInspectDispatch(ctx, arguments[1:])
	case "revoke":
		return command.memoryRevoke(ctx, arguments[1:])
	case "consolidate":
		return command.memoryConsolidate(ctx, arguments[1:])
	case "reject":
		return command.memoryRejectDerived(ctx, arguments[1:])
	case "candidate":
		return command.memoryCandidate(ctx, arguments[1:])
	case "promotion":
		return command.memoryPromotion(ctx, arguments[1:])
	case "telemetry":
		return command.memoryTelemetry(ctx, arguments[1:])
	default:
		fmt.Fprintln(command.Stderr, "memory requires capture, recall, explain, inspect, revoke, consolidate, reject, candidate, promotion, or telemetry")
		return ExitUsage
	}
}

// memoryCandidate 分派演化候选的固定动作：创建必须由人工显式提供闭合变体
// 参数；评测驱动沙箱执行器并要求候选已就绪；拒绝使用封闭理由；作废与过期
// 为显式终态迁移；解释只读。不存在任何自动生成候选或晋升入口。
func (command Command) memoryCandidate(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "memory candidate requires create, evaluate, evidence, promote, reject, invalidate, expire, or inspect")
		return ExitUsage
	}
	switch arguments[0] {
	case "create":
		return command.memoryCandidateCreate(ctx, arguments[1:])
	case "evaluate":
		return command.memoryCandidateEvaluate(ctx, arguments[1:])
	case "evidence":
		return command.memoryCandidateEvidence(ctx, arguments[1:])
	case "promote":
		return command.memoryCandidatePromote(ctx, arguments[1:])
	case "reject":
		return command.memoryCandidateReject(ctx, arguments[1:])
	case "invalidate":
		return command.memoryCandidateInvalidate(ctx, arguments[1:])
	case "expire":
		return command.memoryCandidateExpire(ctx, arguments[1:])
	case "inspect":
		return command.memoryCandidateInspect(ctx, arguments[1:])
	default:
		fmt.Fprintln(command.Stderr, "memory candidate requires create, evaluate, evidence, promote, reject, invalidate, expire, or inspect")
		return ExitUsage
	}
}

// memoryCandidateCreate 从人工提供的闭合变体参数创建 draft 演化候选；
// 来源派生版本复合键与基线指纹为必填，不接受任务正文。
func (command Command) memoryCandidateCreate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate create", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	sourceDerived := flags.String("source-derived", "", "source derived candidate ID (required)")
	sourceConsolidation := flags.String("source-consolidation", "", "source consolidation ID (required)")
	baselineFingerprint := flags.String("baseline-fingerprint", "", "baseline configuration fingerprint sha256 (required)")
	evalSetFingerprint := flags.String("eval-set-fingerprint", "", "frozen eval set fingerprint; providing it creates the candidate directly in ready state")
	noteDigest := flags.String("note-digest", "", "optional sha256 digest of an operator note artifact")
	maxPromptTokens := flags.Int("max-prompt-tokens", 0, "variant max prompt tokens (required)")
	maxSummaryBytes := flags.Int("max-summary-bytes", 0, "variant max summary bytes (required)")
	inventoryPercent := flags.Int("inventory-percent", 0, "variant inventory budget percent (required)")
	reservePercent := flags.Int("reserve-percent", 0, "variant threshold reserve percent (required)")
	ttlText := flags.String("ttl", "", "candidate retention duration (default: 30d, max: 90d)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 || *sourceDerived == "" || *sourceConsolidation == "" || *baselineFingerprint == "" || *maxPromptTokens == 0 || *maxSummaryBytes == 0 || *inventoryPercent == 0 || *reservePercent == 0 {
		fmt.Fprintln(command.Stderr, "memory candidate create requires source, baseline fingerprint and all variant parameters")
		return ExitUsage
	}
	var ttl time.Duration
	var err error
	if *ttlText != "" {
		ttl, err = time.ParseDuration(*ttlText)
		if err != nil {
			fmt.Fprintln(command.Stderr, "memory candidate create --ttl must be a duration")
			return ExitUsage
		}
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.CreateEvolutionCandidate(ctx, app.MemoryEvolutionCreateOptions{
		Workspace:             *workspace,
		StateDir:              *stateDir,
		Config:                configuration,
		SourceDerivedID:       *sourceDerived,
		SourceConsolidationID: *sourceConsolidation,
		BaselineFingerprint:   *baselineFingerprint,
		EvalSetFingerprint:    *evalSetFingerprint,
		NoteDigest:            *noteDigest,
		Variant: memory.CompactionParamsVariant{
			SchemaVersion:           memory.EvolutionSchemaVersion,
			MaxPromptTokens:         *maxPromptTokens,
			MaxSummaryBytes:         *maxSummaryBytes,
			InventoryBudgetPercent:  *inventoryPercent,
			ThresholdReservePercent: *reservePercent,
		},
		TTL: ttl,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate create failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory candidate create", candidate)
	}
	fmt.Fprintf(command.Stdout, "candidate: %s status=%s target=%s variant=%s source=%s/%s\n", candidate.ID, candidate.Status, candidate.TargetKind, candidate.VariantFingerprint, candidate.Source.DerivedID, candidate.Source.ConsolidationID)
	return ExitOK
}

// memoryCandidateEvaluate 驱动沙箱执行器对已就绪候选发起受控评测：评测集、
// 租约与凭据策略都来自 Git 忽略的本地冻结文件；变体参数必须人工重述并经候
// 选登记的规格摘要复核。--dry-run 只跑离线预筛，不开事务也不调用模型。
func (command Command) memoryCandidateEvaluate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate evaluate", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	candidateID := flags.String("candidate", "", "evolution candidate ID (required)")
	localConfigPath := flags.String("local-config", "", "Git-ignored local evaluation configuration (required)")
	evalSetPath := flags.String("eval-set", "", "frozen eval set JSON file (required)")
	leasePath := flags.String("lease", "", "credential lease description JSON file (required)")
	policyPath := flags.String("policy", "", "frozen credential policy JSON file (required)")
	maxPromptTokens := flags.Int("max-prompt-tokens", 0, "variant max prompt tokens, must match the registration (required)")
	maxSummaryBytes := flags.Int("max-summary-bytes", 0, "variant max summary bytes (required)")
	inventoryPercent := flags.Int("inventory-percent", 0, "variant inventory budget percent (required)")
	reservePercent := flags.Int("reserve-percent", 0, "variant threshold reserve percent (required)")
	workspace := flags.String("workspace", "", "workspace the candidate was created against (default: current directory)")
	stateDir := flags.String("state-dir", "", "candidate state directory")
	configPath := flags.String("config", "", "configuration path used when the candidate was created")
	cairnRoot := flags.String("cairn-root", "", "Cairn repository used for asset boundary checks")
	dryRun := flags.Bool("dry-run", false, "run offline prescreen only: no transaction, no model calls")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if *candidateID == "" || *localConfigPath == "" || *evalSetPath == "" || *leasePath == "" || *policyPath == "" ||
		flags.NArg() != 0 || *maxPromptTokens == 0 || *maxSummaryBytes == 0 || *inventoryPercent == 0 || *reservePercent == 0 {
		fmt.Fprintln(command.Stderr, "memory candidate evaluate requires --candidate, --local-config, --eval-set, --lease, --policy and all variant parameters; create the candidate first if it does not exist")
		return ExitUsage
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(command.Stderr, "resolve current directory:", err)
		return ExitFailed
	}
	if *cairnRoot == "" {
		*cairnRoot = root
	}
	localConfiguration, err := benchmark.LoadLocalConfig(*localConfigPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "local benchmark configuration error:", err)
		return ExitUsage
	}
	evalAssets := append([]string{*localConfigPath}, localConfiguration.AssetPaths()...)
	evalAssets = append(evalAssets, *evalSetPath, *leasePath, *policyPath)
	if err := benchmark.EnsureLocalAssetsIgnored(*cairnRoot, evalAssets...); err != nil {
		fmt.Fprintln(command.Stderr, "evaluation asset boundary error:", err)
		return ExitUsage
	}
	memoryConfiguration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	baselineConfiguration, err := config.Load(localConfiguration.Paths.AgentConfig, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "frozen agent configuration error:", err)
		return ExitUsage
	}
	variant := memory.CompactionParamsVariant{
		SchemaVersion:           memory.EvolutionSchemaVersion,
		MaxPromptTokens:         *maxPromptTokens,
		MaxSummaryBytes:         *maxSummaryBytes,
		InventoryBudgetPercent:  *inventoryPercent,
		ThresholdReservePercent: *reservePercent,
	}
	specDigest, err := memory.VariantSpecDigest(variant)
	if err != nil {
		fmt.Fprintln(command.Stderr, "variant parameters rejected:", err)
		return ExitUsage
	}
	inspected, err := command.Service.InspectEvolutionCandidate(ctx, app.MemoryEvolutionInspectOptions{CandidateID: *candidateID, Workspace: *workspace, StateDir: *stateDir, Config: memoryConfiguration})
	if err != nil {
		fmt.Fprintf(command.Stderr, "memory candidate evaluate failed: %v; create the candidate first with memory candidate create\n", err)
		return exitFor(err)
	}
	if inspected.Candidate.Status != memory.CandidateStateReady && inspected.Candidate.Status != memory.CandidateStateEvaluating {
		fmt.Fprintf(command.Stderr, "memory candidate evaluate requires a ready or evaluating candidate, got %s; create it first or resolve its current state\n", inspected.Candidate.Status)
		return ExitFailed
	}
	if inspected.Candidate.VariantSpecSHA256 != specDigest {
		fmt.Fprintln(command.Stderr, "restated variant parameters diverge from the registered candidate specification")
		return ExitFailed
	}
	evalSet, err := sandboxeval.LoadEvalSet(*evalSetPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "frozen eval set error:", err)
		return ExitUsage
	}
	lease, err := sandboxeval.LoadCredentialLease(*leasePath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "credential lease error:", err)
		return ExitUsage
	}
	policy, err := sandboxeval.LoadCredentialPolicy(*policyPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "credential policy error:", err)
		return ExitUsage
	}
	pack, err := benchmark.LoadTaskPack(localConfiguration.Paths.Manifest)
	if err != nil {
		fmt.Fprintln(command.Stderr, "benchmark manifest error:", err)
		return ExitUsage
	}
	prepareDuration, err := time.ParseDuration(localConfiguration.Execution.PrepareTimeout)
	if err != nil || prepareDuration <= 0 {
		fmt.Fprintln(command.Stderr, "local benchmark prepare_timeout must be a positive duration")
		return ExitUsage
	}
	executor := benchmark.SandboxExecutor{Runner: benchmark.Runner{Service: command.Service}}
	outcome, err := executor.Run(ctx, benchmark.SandboxExecutorOptions{
		Pack:            pack,
		EvalSet:         evalSet,
		Lease:           lease,
		Policy:          policy,
		CandidateID:     *candidateID,
		MemoryWorkspace: *workspace,
		MemoryStateDir:  *stateDir,
		MemoryConfig:    memoryConfiguration,
		BaselineConfig:  baselineConfiguration,
		Variant:         variant,
		StateRoot:       localConfiguration.Paths.StateDir,
		WorkspacesRoot:  localConfiguration.Paths.WorkspacesDir,
		PrivateRoot:     localConfiguration.Paths.PrivateDir,
		CairnRoot:       *cairnRoot,
		PrepareTimeout:  prepareDuration,
		PrepareProxy:    localConfiguration.Execution.PrepareProxy,
		DryRun:          *dryRun,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate evaluate failed:", err)
		return ExitFailed
	}
	if *jsonOutput {
		projection := struct {
			DryRun        bool                   `json:"dry_run"`
			EvaluationID  string                 `json:"evaluation_id,omitempty"`
			CandidateID   string                 `json:"candidate_id,omitempty"`
			Conclusion    sandboxeval.Conclusion `json:"conclusion"`
			ResultDigest  string                 `json:"result_digest,omitempty"`
			RunsCompleted int                    `json:"runs_completed"`
			StopReason    string                 `json:"stop_reason,omitempty"`
			StopDetail    string                 `json:"stop_detail,omitempty"`
		}{
			DryRun:        outcome.DryRun,
			EvaluationID:  outcome.Evaluation.ID,
			CandidateID:   outcome.Evaluation.CandidateID,
			Conclusion:    outcome.Conclusion,
			ResultDigest:  outcome.ResultDigest,
			RunsCompleted: outcome.RunsCompleted,
			StopReason:    outcome.StopReason,
			StopDetail:    outcome.StopDetail,
		}
		if err := writeJSONChecked(command.Stdout, projection); err != nil {
			markOutputErrorReported(command.Stdout)
			fmt.Fprintln(command.Stderr, "memory candidate evaluate output failed:", err)
			return ExitFailed
		}
	} else if outcome.DryRun {
		fmt.Fprintln(command.Stdout, "evaluate dry-run: prescreen passed; no transaction opened, no model called")
	} else {
		conclusion := outcome.Conclusion
		if outcome.StopDetail != "" {
			fmt.Fprintf(command.Stdout, "evaluate: %s status=%s trigger=%s stop_detail=%s runs=%d result=%s\n", outcome.Evaluation.ID, conclusion.Status, conclusion.Trigger, outcome.StopDetail, outcome.RunsCompleted, outcome.ResultDigest)
		} else {
			fmt.Fprintf(command.Stdout, "evaluate: %s status=%s trigger=%s runs=%d result=%s\n", outcome.Evaluation.ID, conclusion.Status, conclusion.Trigger, outcome.RunsCompleted, outcome.ResultDigest)
		}
	}
	if outcome.DryRun || outcome.Conclusion.Status == sandboxeval.ConclusionPassed {
		return ExitOK
	}
	return ExitFailed
}

// memoryCandidateEvidence 只读验证候选的真实双臂套件证据，并输出可机器复算的
// 质量、token 成本、墙钟和重复探索摘要；只有已验证的 passed 结论返回成功。
func (command Command) memoryCandidateEvidence(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate evidence", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	candidateID := flags.String("candidate", "", "evolution candidate ID (required)")
	evalSetPath := flags.String("eval-set", "", "frozen eval set JSON file (required)")
	suiteStateRoot := flags.String("suite-state-dir", "", "state root hosting the evaluations directory (required)")
	workspace := flags.String("workspace", "", "workspace the candidate was created against (default: current directory)")
	stateDir := flags.String("state-dir", "", "candidate state directory")
	configPath := flags.String("config", "", "configuration path used when the candidate was created")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if *candidateID == "" || *evalSetPath == "" || *suiteStateRoot == "" || flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory candidate evidence requires --candidate, --eval-set and --suite-state-dir")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	inspected, err := command.Service.InspectEvolutionCandidate(ctx, app.MemoryEvolutionInspectOptions{CandidateID: *candidateID, Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate evidence failed:", err)
		return exitFor(err)
	}
	if inspected.Evaluation == nil {
		fmt.Fprintln(command.Stderr, "memory candidate evidence failed: candidate has no evaluation")
		return ExitFailed
	}
	evalSet, err := sandboxeval.LoadEvalSet(*evalSetPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "frozen eval set error:", err)
		return ExitUsage
	}
	evidence, err := benchmark.VerifySandboxBenefitEvidence(*suiteStateRoot, *inspected.Evaluation, evalSet)
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate evidence failed:", err)
		if errors.Is(err, benchmark.ErrSandboxEvidenceCorrupt) {
			return ExitCorrupt
		}
		return ExitFailed
	}
	if *jsonOutput {
		if err := writeJSONChecked(command.Stdout, evidence); err != nil {
			markOutputErrorReported(command.Stdout)
			fmt.Fprintln(command.Stderr, "memory candidate evidence output failed:", err)
			return ExitFailed
		}
	} else {
		fmt.Fprintf(command.Stdout, "evidence: verified=%t status=%s trigger=%s runs=%d/%d result=%s\n", evidence.Verified, evidence.Status, evidence.Trigger, evidence.CountedRuns, evidence.ExpectedRuns, evidence.SuiteResultDigest)
		fmt.Fprintf(command.Stdout, "baseline: success=%d/%d input_tokens=%d output_tokens=%d cost_units=%d median_wall_ms=%d repeated_explorations=%d security_incidents=%d doctor_replay_broken=%d tool_pairing_breaks=%d\n", evidence.Baseline.Successes, evidence.Baseline.Runs, evidence.Baseline.InputTokens, evidence.Baseline.OutputTokens, evidence.Baseline.CostUnits, evidence.Baseline.MedianWallMS, evidence.Baseline.RepeatedExplorations, evidence.Baseline.SecurityIncidents, evidence.Baseline.DoctorReplayBroken, evidence.Baseline.ToolPairingBreaks)
		fmt.Fprintf(command.Stdout, "variant: success=%d/%d input_tokens=%d output_tokens=%d cost_units=%d median_wall_ms=%d repeated_explorations=%d security_incidents=%d doctor_replay_broken=%d tool_pairing_breaks=%d\n", evidence.Variant.Successes, evidence.Variant.Runs, evidence.Variant.InputTokens, evidence.Variant.OutputTokens, evidence.Variant.CostUnits, evidence.Variant.MedianWallMS, evidence.Variant.RepeatedExplorations, evidence.Variant.SecurityIncidents, evidence.Variant.DoctorReplayBroken, evidence.Variant.ToolPairingBreaks)
	}
	if evidence.Status != sandboxeval.ConclusionPassed {
		return ExitFailed
	}
	return ExitOK
}

// memoryCandidateReject 以封闭理由拒绝演化候选并落入冷却。
func (command Command) memoryCandidateReject(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate reject", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	reason := flags.String("reason", "", "rejection reason from the closed lexicon (required)")
	cooldownText := flags.String("cooldown", "", "cooldown duration (default: 7d, max: 90d)")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 || *reason == "" {
		fmt.Fprintln(command.Stderr, "memory candidate reject requires a candidate ID and --reason")
		return ExitUsage
	}
	var cooldown time.Duration
	var err error
	if *cooldownText != "" {
		cooldown, err = time.ParseDuration(*cooldownText)
		if err != nil {
			fmt.Fprintln(command.Stderr, "memory candidate reject --cooldown must be a duration")
			return ExitUsage
		}
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.RejectEvolutionCandidate(ctx, app.MemoryEvolutionActionOptions{CandidateID: flags.Arg(0), Reason: *reason, Workspace: *workspace, StateDir: *stateDir, Config: configuration, Cooldown: cooldown})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate reject failed:", err)
		return exitFor(err)
	}
	fmt.Fprintf(command.Stdout, "rejected: %s status=%s\n", candidate.ID, candidate.Status)
	return ExitOK
}

// memoryCandidateInvalidate 显式作废来源失效的演化候选。
func (command Command) memoryCandidateInvalidate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate invalidate", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory candidate invalidate requires a candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.InvalidateEvolutionCandidate(ctx, app.MemoryEvolutionActionOptions{CandidateID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate invalidate failed:", err)
		return exitFor(err)
	}
	fmt.Fprintf(command.Stdout, "invalidated: %s status=%s\n", candidate.ID, candidate.Status)
	return ExitOK
}

// memoryCandidateExpire 显式把保留期耗尽的演化候选置为 expired 终态。
func (command Command) memoryCandidateExpire(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate expire", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory candidate expire requires a candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.ExpireEvolutionCandidate(ctx, app.MemoryEvolutionActionOptions{CandidateID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate expire failed:", err)
		return exitFor(err)
	}
	fmt.Fprintf(command.Stdout, "expired: %s status=%s\n", candidate.ID, candidate.Status)
	return ExitOK
}

// memoryCandidateInspect 输出演化候选与审计历史的只读解释。
func (command Command) memoryCandidateInspect(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate inspect", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeMemoryPositionals(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory candidate inspect requires a candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	inspected, err := command.Service.InspectEvolutionCandidate(ctx, app.MemoryEvolutionInspectOptions{CandidateID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate inspect failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory candidate inspect", inspected)
	}
	candidate := inspected.Candidate
	fmt.Fprintf(command.Stdout, "candidate: %s status=%s target=%s\n", candidate.ID, candidate.Status, candidate.TargetKind)
	fmt.Fprintf(command.Stdout, "source: %s/%s\n", candidate.Source.DerivedID, candidate.Source.ConsolidationID)
	fmt.Fprintf(command.Stdout, "variant: %s spec=%s\n", candidate.VariantFingerprint, candidate.VariantSpecSHA256)
	fmt.Fprintf(command.Stdout, "baseline: %s\n", candidate.BaselineFingerprint)
	fmt.Fprintf(command.Stdout, "retention: %s -> %s\n", candidate.CreatedAt.Format(time.RFC3339), candidate.ExpiresAt.Format(time.RFC3339))
	for _, entry := range inspected.Audit {
		reason := ""
		if entry.Reason != "" {
			reason = " reason=" + entry.Reason
		}
		fmt.Fprintf(command.Stdout, "audit: %s%s at %s\n", entry.Action, reason, entry.OccurredAt.Format(time.RFC3339))
	}
	if evaluation := inspected.Evaluation; evaluation != nil {
		finished := "-"
		if !evaluation.FinishedAt.IsZero() {
			finished = evaluation.FinishedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(command.Stdout, "evaluation: %s status=%s evidence=%s started=%s finished=%s\n",
			evaluation.ID, evaluation.Status, evidenceRef(evaluation.SuiteResultDigest), evaluation.StartedAt.Format(time.RFC3339), finished)
	}
	return ExitOK
}

// evidenceRef 把证据摘要引用收敛为展示形态：空值显示 pending。
func evidenceRef(digest string) string {
	if digest == "" {
		return "pending"
	}
	return digest
}

// memoryConsolidate 在工作区锁内执行一次事务化巩固，并把提交的计划、派生候选与
// 被替代版本以人类或 NDJSON 视图输出；不接入运行时召回。
func (command Command) memoryConsolidate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory consolidate", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	ttlText := flags.String("ttl", "", "derived candidate retention duration (default: 30d, max: 90d)")
	minSupport := flags.Int("min-support", 0, "minimum evidence support for pattern and hypothesis (default: 2, cannot be lowered)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory consolidate does not accept positional arguments")
		return ExitUsage
	}
	var ttl time.Duration
	var err error
	if *ttlText != "" {
		ttl, err = time.ParseDuration(*ttlText)
		if err != nil {
			fmt.Fprintln(command.Stderr, "memory consolidate --ttl must be a duration")
			return ExitUsage
		}
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	results, err := command.Service.ConsolidateMemory(ctx, app.MemoryConsolidateOptions{Workspace: *workspace, StateDir: *stateDir, Config: configuration, TTL: ttl, MinPatternSupport: *minSupport, MinHypothesisSupport: *minSupport})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory consolidate failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory consolidate", results)
	}
	if len(results) == 0 {
		fmt.Fprintln(command.Stdout, "consolidation: no derivable candidates from current inputs")
		return ExitOK
	}
	for _, result := range results {
		fmt.Fprintf(command.Stdout, "consolidation: %s strategy=%s inputs=%d\n", result.Plan.ConsolidationID, result.Plan.Strategy, len(result.Plan.InputIDs))
		for _, derived := range result.Plan.Derived {
			fmt.Fprintf(command.Stdout, "derived: %s kind=%s support=%d status=%s\n", derived.ID, derived.Kind, len(derived.Evidence), derived.Status)
		}
		for _, superseded := range result.SupersededVersion {
			fmt.Fprintf(command.Stdout, "superseded: %s consolidation=%s\n", superseded.ID, superseded.ConsolidationID)
		}
	}
	return ExitOK
}

// memoryRejectDerived 显式拒绝当前 active 的派生候选版本；理由只能是封闭词表。
func (command Command) memoryRejectDerived(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory reject", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	reason := flags.String("reason", "", "rejection reason: insufficient_evidence or human_request (required)")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 || *reason == "" {
		fmt.Fprintln(command.Stderr, "memory reject requires a derived candidate ID and --reason")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	key, err := command.Service.RejectDerivedMemory(ctx, app.MemoryRejectDerivedOptions{DerivedID: flags.Arg(0), Reason: *reason, Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory reject failed:", err)
		return exitFor(err)
	}
	fmt.Fprintf(command.Stdout, "rejected: %s consolidation=%s\n", key.ID, key.ConsolidationID)
	return ExitOK
}

// memoryExplain 显式执行 M2-A 的 sqlite_exact 计划和一次性证据账本投影。它仅接收
// 摘要、枚举和 UTC 时间范围，绝不接收任务正文或把结果注入 Agent Loop。
func (command Command) memoryExplain(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory explain", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	taskDigest := flags.String("task-digest", "", "exact sha256 task digest")
	trustedVerification := flags.String("trusted-verification", "any", "any, required, or not_required")
	createdFromText := flags.String("created-from", "", "UTC RFC3339 start (inclusive)")
	createdUntilText := flags.String("created-until", "", "UTC RFC3339 end (exclusive)")
	limit := flags.Int("limit", 0, "candidates: compact default/max 10; trace default 20, max 100")
	precision := flags.String("precision", "compact", "compact or trace")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	var outcomes, failureClasses stringList
	flags.Var(&outcomes, "outcome", "completed, failed, or cancelled; repeatable")
	flags.Var(&failureClasses, "failure-class", "controlled failure class; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory explain does not accept positional arguments")
		return ExitUsage
	}
	createdFrom, parseErr := parseUTCTimestamp(*createdFromText)
	if parseErr != nil {
		fmt.Fprintln(command.Stderr, "memory explain --created-from must be UTC RFC3339")
		return ExitUsage
	}
	createdUntil, parseErr := parseUTCTimestamp(*createdUntilText)
	if parseErr != nil {
		fmt.Fprintln(command.Stderr, "memory explain --created-until must be UTC RFC3339")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	ledger, err := command.Service.ExplainMemoryRecall(ctx, app.MemoryExplainOptions{
		Query: memory.RecallQuery{
			TaskPromptDigest:    *taskDigest,
			Outcomes:            outcomes,
			FailureClasses:      failureClasses,
			TrustedVerification: *trustedVerification,
			CreatedFrom:         createdFrom,
			CreatedUntil:        createdUntil,
			Limit:               *limit,
			Precision:           *precision,
		},
		Workspace: *workspace,
		StateDir:  *stateDir,
		Config:    configuration,
	})
	if err != nil && ledger.SchemaVersion == 0 {
		fmt.Fprintln(command.Stderr, "memory explain query is invalid")
		return ExitUsage
	}
	if *jsonOutput {
		if code := command.writeJSONOutput("memory explain", struct {
			Type   string                `json:"type"`
			Ledger memory.EvidenceLedger `json:"ledger"`
		}{Type: "memory_explain", Ledger: ledger}); code != ExitOK {
			return code
		}
	} else {
		fmt.Fprintf(command.Stdout, "memory explain: strategy=%s status=%s candidates=%d conflicts=%d\n", ledger.Plan.Strategy, ledger.Status, len(ledger.Entries), len(ledger.Conflicts))
	}
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory explain source invalid")
		return exitFor(err)
	}
	return ExitOK
}

// parseUTCTimestamp 解析 CLI 时间边界；空值表示未提供，非 UTC 表示拒绝而非隐式换算。
func parseUTCTimestamp(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC {
		return nil, errors.New("timestamp is not UTC RFC3339")
	}
	return &parsed, nil
}

// memoryCapture 解析固定 Run 身份与 TTL，从已校验终态 Journal 写入结构化候选。
func (command Command) memoryCapture(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory capture", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	runID := flags.String("run", "", "terminal run ID (required)")
	ttlText := flags.String("ttl", "", "memory retention duration (default: 30d, max: 90d)")
	supersedesID := flags.String("supersedes", "", "active memory candidate ID to atomically supersede")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 || *runID == "" {
		fmt.Fprintln(command.Stderr, "memory capture requires --run and no positional arguments")
		return ExitUsage
	}
	var ttl time.Duration
	var err error
	if *ttlText != "" {
		ttl, err = time.ParseDuration(*ttlText)
		if err != nil {
			fmt.Fprintln(command.Stderr, "memory capture --ttl must be a duration")
			return ExitUsage
		}
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.CaptureMemory(ctx, app.MemoryCaptureOptions{RunID: domain.RunID(*runID), Workspace: *workspace, StateDir: *stateDir, Config: configuration, TTL: ttl, SupersedesID: *supersedesID})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory capture failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory capture", struct {
			Type      string `json:"type"`
			Candidate any    `json:"candidate"`
		}{Type: "memory_capture", Candidate: candidate})
	}
	fmt.Fprintf(command.Stdout, "memory candidate: %s kind=%s outcome=%s status=%s expires=%s\n", candidate.ID, candidate.Kind, candidate.Outcome, candidate.Status, candidate.ExpiresAt.UTC().Format(time.RFC3339Nano))
	return ExitOK
}

// memoryRecall 只读检索当前工作区的 active、未过期 run_outcome 候选。
func (command Command) memoryRecall(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory recall", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	limit := flags.Int("limit", 0, "maximum recalled candidates (default: 20, max: 100)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory recall does not accept positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidates, err := command.Service.RecallMemory(ctx, app.MemoryRecallOptions{Workspace: *workspace, StateDir: *stateDir, Config: configuration, Limit: *limit})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory recall failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory recall", struct {
			Type       string `json:"type"`
			Candidates any    `json:"candidates"`
		}{Type: "memory_recall", Candidates: candidates})
	}
	for _, candidate := range candidates {
		fmt.Fprintf(command.Stdout, "memory candidate: %s outcome=%s status=%s created=%s expires=%s\n", candidate.ID, candidate.Outcome, candidate.Status, candidate.CreatedAt.UTC().Format(time.RFC3339Nano), candidate.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	return ExitOK
}

// memoryInspect 只读显示当前工作区一个候选及其无自由文本的审计历史。
func (command Command) memoryInspectDispatch(ctx context.Context, arguments []string) int {
	for _, argument := range normalizeMemoryPositionals(arguments) {
		if memory.IsDerivedID(argument) {
			return command.memoryInspectDerived(ctx, arguments)
		}
	}
	return command.memoryInspect(ctx, arguments)
}

// memoryInspectDerived 输出派生候选最新版本的候选事实与审计历史；人类与
// NDJSON 视图共享同一只读投影。
func (command Command) memoryInspectDerived(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory inspect derived", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeMemoryPositionals(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory inspect requires a derived candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	inspected, err := command.Service.InspectDerivedMemory(ctx, app.MemoryInspectDerivedOptions{DerivedID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory inspect failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory inspect", inspected)
	}
	candidate := inspected.Candidate
	fmt.Fprintf(command.Stdout, "derived: %s kind=%s status=%s current=%t eligible=%t\n", candidate.ID, candidate.Kind, candidate.Status, inspected.Current, inspected.Eligible)
	fmt.Fprintf(command.Stdout, "consolidation: %s\n", candidate.ConsolidationID)
	fmt.Fprintf(command.Stdout, "scope: %s\n", candidate.ScopeDigest)
	fmt.Fprintf(command.Stdout, "payload: %s\n", string(candidate.Payload))
	fmt.Fprintf(command.Stdout, "retention: %s -> %s\n", candidate.CreatedAt.Format(time.RFC3339), candidate.ExpiresAt.Format(time.RFC3339))
	for _, evidence := range candidate.Evidence {
		fmt.Fprintf(command.Stdout, "evidence: %s\n", evidence.CandidateID)
	}
	for _, entry := range inspected.Audit {
		reason := ""
		if entry.Reason != "" {
			reason = " reason=" + entry.Reason
		}
		fmt.Fprintf(command.Stdout, "audit: %s%s at %s\n", entry.Action, reason, entry.OccurredAt.Format(time.RFC3339))
	}
	return ExitOK
}

func (command Command) memoryInspect(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory inspect", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeMemoryPositionals(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory inspect requires a candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	result, err := command.Service.InspectMemory(ctx, app.MemoryInspectOptions{CandidateID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory inspect failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory inspect", struct {
			Type   string `json:"type"`
			Memory any    `json:"memory"`
		}{Type: "memory_inspect", Memory: result})
	}
	candidate := result.Candidate
	fmt.Fprintf(command.Stdout, "memory candidate: %s kind=%s outcome=%s failure=%s trusted_verification=%t status=%s\n", candidate.ID, candidate.Kind, candidate.Outcome, candidate.FailureClass, candidate.TrustedVerification, candidate.Status)
	for _, entry := range result.Audit {
		fmt.Fprintf(command.Stdout, "audit: %s reason=%s at=%s\n", entry.Action, entry.Reason, entry.OccurredAt.UTC().Format(time.RFC3339Nano))
	}
	return ExitOK
}

// memoryRevoke 原子撤销当前工作区的 active 候选，并要求封闭的、无自由文本理由。
func (command Command) memoryRevoke(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory revoke", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	reason := flags.String("reason", "", "evidence_invalid or human_request (required)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeMemoryPositionals(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 || (*reason != "evidence_invalid" && *reason != "human_request") {
		fmt.Fprintln(command.Stderr, "memory revoke requires a candidate ID and --reason evidence_invalid|human_request")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.RevokeMemory(ctx, app.MemoryRevokeOptions{CandidateID: flags.Arg(0), Reason: *reason, Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory revoke failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory revoke", struct {
			Type      string `json:"type"`
			Candidate any    `json:"candidate"`
		}{Type: "memory_revoke", Candidate: candidate})
	}
	fmt.Fprintf(command.Stdout, "memory candidate: %s status=%s\n", candidate.ID, candidate.Status)
	return ExitOK
}

// resume 解析恢复、审批、对账或中止参数，并调用相应应用用例。
func normalizeMemoryPositionals(arguments []string) []string {
	return normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {}, "--reason": {},
	})
}

func (command Command) memoryPromotion(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "memory promotion requires activate, rollback or list")
		return ExitUsage
	}
	switch arguments[0] {
	case "activate":
		return command.memoryPromotionActivate(ctx, arguments[1:])
	case "rollback":
		return command.memoryPromotionRollback(ctx, arguments[1:])
	case "list":
		return command.memoryPromotionList(ctx, arguments[1:])
	default:
		fmt.Fprintln(command.Stderr, "memory promotion requires activate, rollback or list")
		return ExitUsage
	}
}

// loadSuiteResultForPromotion 定位候选的评测目录、按存储摘要校验套件结果
// 字节并解码正文；摘要不一致即失败关闭，杜绝在漂移证据上晋升。
func loadSuiteResultForPromotion(evaluation memory.Evaluation, stateRoot string) (benchmark.SandboxSuiteResult, error) {
	if evaluation.Status != memory.EvaluationStatusPassed || evaluation.SuiteResultDigest == "" {
		return benchmark.SandboxSuiteResult{}, errors.New("candidate lacks a concluded passed evaluation digest")
	}
	resultPath := filepath.Join(stateRoot, "evaluations", strings.ReplaceAll(evaluation.ID, ":", "_"), "suite-result.json")
	content, err := os.ReadFile(resultPath)
	if err != nil {
		return benchmark.SandboxSuiteResult{}, fmt.Errorf("read suite result: %w", err)
	}
	digest := sha256.Sum256(content)
	if "sha256:"+hex.EncodeToString(digest[:]) != evaluation.SuiteResultDigest {
		return benchmark.SandboxSuiteResult{}, errors.New("suite result bytes diverge from the stored digest")
	}
	var result benchmark.SandboxSuiteResult
	if err := json.Unmarshal(content, &result); err != nil {
		return benchmark.SandboxSuiteResult{}, fmt.Errorf("decode suite result: %w", err)
	}
	if result.EvaluationID != evaluation.ID || result.CandidateID != evaluation.CandidateID || result.Status != memory.EvaluationStatusPassed {
		return benchmark.SandboxSuiteResult{}, errors.New("suite result does not match the passed evaluation identity")
	}
	if err := benchmark.RequireTrustedSandboxSuiteVersion(result); err != nil {
		return benchmark.SandboxSuiteResult{}, err
	}
	return result, nil
}

// memoryCandidatePromote 对评测通过的候选执行人工晋升：定位评测行并按存
// 储摘要校验本地套件结果正文，提取闭合指标摘要后经审批指纹触发版本账本
// 事务。指纹不匹配时回显期望值供操作者重试。
func (command Command) memoryCandidatePromote(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate promote", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	candidateID := flags.String("candidate", "", "evolution candidate ID (required)")
	approver := flags.String("approver", "", "operator attribution label, 1..64 runes (required)")
	approvalFingerprint := flags.String("approval-fingerprint", "", "promotion approval fingerprint sha256 (required)")
	approvalNonce := flags.String("approval-nonce", "", "one-time promotion approval nonce sha256 (required)")
	maxPromptTokens := flags.Int("max-prompt-tokens", 0, "variant max prompt tokens, must match the registration (required)")
	maxSummaryBytes := flags.Int("max-summary-bytes", 0, "variant max summary bytes (required)")
	inventoryPercent := flags.Int("inventory-percent", 0, "variant inventory budget percent (required)")
	reservePercent := flags.Int("reserve-percent", 0, "variant threshold reserve percent (required)")
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "candidate state directory")
	suiteStateRoot := flags.String("suite-state-dir", "", "state root hosting the evaluations directory (required)")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if *candidateID == "" || *approver == "" || *approvalFingerprint == "" || *approvalNonce == "" || *suiteStateRoot == "" || flags.NArg() != 0 ||
		*maxPromptTokens == 0 || *maxSummaryBytes == 0 || *inventoryPercent == 0 || *reservePercent == 0 {
		fmt.Fprintln(command.Stderr, "memory candidate promote requires --candidate, --approver, --approval-nonce, --approval-fingerprint, --suite-state-dir and all variant parameters")
		return ExitUsage
	}
	variant := memory.CompactionParamsVariant{
		SchemaVersion:           memory.EvolutionSchemaVersion,
		MaxPromptTokens:         *maxPromptTokens,
		MaxSummaryBytes:         *maxSummaryBytes,
		InventoryBudgetPercent:  *inventoryPercent,
		ThresholdReservePercent: *reservePercent,
	}
	variantDigest, err := memory.VariantSpecDigest(variant)
	if err != nil {
		fmt.Fprintln(command.Stderr, "variant parameters rejected:", err)
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	promotion, err := command.preparePromotion(ctx, *candidateID, *approver, *approvalNonce, variant, variantDigest, *suiteStateRoot, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate promote failed:", err)
		return exitFor(err)
	}
	promoted, err := command.Service.PromoteEvolutionCandidate(ctx, app.MemoryPromotionOptions{
		Promotion: promotion, ApprovalFingerprint: *approvalFingerprint,
		SuiteStateRoot: *suiteStateRoot, Workspace: *workspace, StateDir: *stateDir, Config: configuration,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate promote failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory candidate promote", promoted)
	}
	fmt.Fprintf(command.Stdout, "promotion decision recorded: %s version=%d target=%s activation=inactive approver=%s\n", promoted.ID, promoted.Version, promoted.TargetKind, promoted.Approver)
	return ExitOK
}

// preparePromotion 组装晋升行正文：读取候选与评测行、按摘要校验并提取套
// 件指标、重述变体并与登记规格摘要核对、派生身份与下一版本号。
func (command Command) preparePromotion(ctx context.Context, candidateID, approver, approvalNonce string, variant memory.CompactionParamsVariant, variantDigest, suiteStateRoot, workspace, stateDir string, configuration config.Config) (memory.Promotion, error) {
	inspected, err := command.Service.InspectEvolutionCandidate(ctx, app.MemoryEvolutionInspectOptions{CandidateID: candidateID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		return memory.Promotion{}, err
	}
	candidate := inspected.Candidate
	if candidate.Status != memory.CandidateStateEvaluatedPassed {
		return memory.Promotion{}, fmt.Errorf("promotion requires an evaluated_passed candidate, got %s", candidate.Status)
	}
	// 变体重述核对：登记行只存规格摘要，晋升正文必须与登记事实逐字一致。
	if candidate.VariantSpecSHA256 != variantDigest {
		return memory.Promotion{}, errors.New("restated variant parameters diverge from the registered candidate specification")
	}
	if inspected.Evaluation == nil {
		return memory.Promotion{}, errors.New("promotion requires the concluded evaluation row")
	}
	result, err := loadSuiteResultForPromotion(*inspected.Evaluation, suiteStateRoot)
	if err != nil {
		return memory.Promotion{}, err
	}
	metrics, err := extractSuiteMetricSummary(result.Records, resultAttemptsHint(result))
	if err != nil {
		return memory.Promotion{}, err
	}
	promotions, activation, activationFound, err := command.Service.InspectEvolutionPromotions(ctx, candidate.TargetKind, workspace, stateDir, configuration)
	if err != nil && !errors.Is(err, memory.ErrNotFound) {
		return memory.Promotion{}, err
	}
	nextVersion := 1
	if len(promotions) > 0 {
		nextVersion = promotions[len(promotions)-1].Version + 1
	}
	// 回退目标锚定晋升时刻的激活版本：激活为基线时目标为空（回退即回基
	// 线），保证任意版本回退后仍可继续回退。
	rollbackTarget := ""
	if activationFound {
		rollbackTarget = activation.PromotionID
	}
	promotion := memory.Promotion{
		SchemaVersion: memory.PromotionSchemaVersion, CandidateID: candidate.ID, EvaluationID: inspected.Evaluation.ID, TargetKind: candidate.TargetKind,
		Version: nextVersion, Variant: variant, VariantFingerprint: candidate.VariantFingerprint, VariantSpecSHA256: candidate.VariantSpecSHA256,
		BaselineFingerprint: candidate.BaselineFingerprint, EvalSetFingerprint: inspected.Evaluation.EvalSetFingerprint,
		SuiteResultDigest: inspected.Evaluation.SuiteResultDigest, ComparatorVersion: inspected.Evaluation.ComparatorVersion,
		Conclusion: memory.EvaluationStatusPassed, ConclusionTrigger: result.Trigger,
		Metrics: metrics, ApprovalNonce: approvalNonce, Approver: approver, RollbackTargetID: rollbackTarget,
		Status: memory.PromotionStatusPromotedInactive, CreatedAt: time.Now().UTC(),
	}
	promotion.ID, err = memory.PromotionID(candidate.TargetKind, promotion.Version)
	if err != nil {
		return memory.Promotion{}, err
	}
	return promotion, nil
}

// resultAttemptsHint 从套件结果反推每任务尝试数；记录不足时返回零使摘要
// 提取器的完备性检查失败关闭。
func resultAttemptsHint(result benchmark.SandboxSuiteResult) int {
	armCounts := map[string]int{}
	for _, record := range result.Records {
		armCounts[record.Arm]++
	}
	if len(armCounts) != 2 {
		return 0
	}
	tasks := len(uniqueTaskIDs(result.Records))
	if tasks == 0 {
		return 0
	}
	attempts := 0
	for _, count := range armCounts {
		if attempts == 0 {
			attempts = count / tasks
		} else if attempts != count/tasks {
			return 0
		}
	}
	return attempts
}

// memoryPromotionActivate 把已批准的 inactive 版本显式激活到 sandbox；M4
// 不存在生产环境或自动激活入口。
func (command Command) memoryPromotionActivate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory promotion activate", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	versionID := flags.String("version", "", "approved inactive version ID (required)")
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if *versionID == "" || flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory promotion activate requires --version and no positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	promotion, activation, err := command.Service.ActivateEvolutionPromotion(ctx, app.MemoryPromotionActivationOptions{TargetKind: memory.TargetKindContextCompactionParams, PromotionID: *versionID, Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory promotion activate failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory promotion activate", struct {
			Promotion  memory.Promotion  `json:"promotion"`
			Activation memory.Activation `json:"activation"`
		}{Promotion: promotion, Activation: activation})
	}
	fmt.Fprintf(command.Stdout, "sandbox activated: %s version=%d previous=%s\n", promotion.ID, promotion.Version, activation.PreviousPromotionID)
	return ExitOK
}

// memoryPromotionRollback 回退当前 sandbox 激活版本：指针移回决策固定目标或
// 清空回产品基线，版本置 revoked 并写入带理由的审计。
func (command Command) memoryPromotionRollback(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory promotion rollback", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	reason := flags.String("reason", "", "rollback reason from the closed lexicon")
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if *reason == "" || flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory promotion rollback requires --reason and no positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	promotion, activation, err := command.Service.RollbackEvolutionPromotion(ctx, app.MemoryPromotionRollbackOptions{
		TargetKind: memory.TargetKindContextCompactionParams, Reason: *reason,
		Workspace: *workspace, StateDir: *stateDir, Config: configuration,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory promotion rollback failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory promotion rollback", struct {
			Promotion  memory.Promotion  `json:"promotion"`
			Activation memory.Activation `json:"activation"`
		}{Promotion: promotion, Activation: activation})
	}
	target := "baseline"
	if activation.PromotionID != "" {
		target = activation.PromotionID
	}
	fmt.Fprintf(command.Stdout, "rolled back: %s version=%d now active=%s\n", promotion.ID, promotion.Version, target)
	return ExitOK
}

// memoryPromotionList 只读列出版本史与当前激活指针。
func (command Command) memoryPromotionList(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory promotion list", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory promotion list does not accept positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	promotions, activation, found, err := command.Service.InspectEvolutionPromotions(ctx, memory.TargetKindContextCompactionParams, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory promotion list failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory promotion list", struct {
			Promotions []memory.Promotion `json:"promotions"`
			Activation *memory.Activation `json:"activation,omitempty"`
		}{Promotions: promotions, Activation: activationPointer(activation, found)})
	}
	for _, promotion := range promotions {
		fmt.Fprintf(command.Stdout, "version %d: %s status=%s candidate=%s trigger=%s\n", promotion.Version, promotion.ID, promotion.Status, promotion.CandidateID, promotion.ConclusionTrigger)
	}
	if found {
		fmt.Fprintf(command.Stdout, "active: %s (since %s)\n", activation.PromotionID, activation.ActivatedAt.Format(time.RFC3339))
	} else {
		fmt.Fprintln(command.Stdout, "active: baseline")
	}
	return ExitOK
}

// activationPointer 把查询出的激活指针折叠为可空投影；行缺失表示基线。
func activationPointer(activation memory.Activation, found bool) *memory.Activation {
	if !found {
		return nil
	}
	return &activation
}

// memoryTelemetry 只读输出记忆体系聚合遥测快照：NDJSON 结构化投影或人可
// 读分组行，覆盖记忆、巩固、演化与召回四段。零正文零路径零密钥。
func (command Command) memoryTelemetry(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory telemetry", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory telemetry does not accept positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	snapshot, err := command.Service.InspectMemoryTelemetry(ctx, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory telemetry failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("memory telemetry", struct {
			Type     string `json:"type"`
			Snapshot any    `json:"snapshot"`
		}{Type: "memory_telemetry", Snapshot: snapshot})
	}
	fmt.Fprintf(command.Stdout, "memory: candidates=%d active=%d revoked=%d superseded=%d\n",
		snapshot.Memory.CandidatesTotal, snapshot.Memory.CandidatesActive, snapshot.Memory.CandidatesRevoked, snapshot.Memory.CandidatesSuperseded)
	fmt.Fprintf(command.Stdout, "consolidation: committed=%d derived=%d rejected=%d\n",
		snapshot.Consolidation.ConsolidationsCommitted, snapshot.Consolidation.DerivedTotal, snapshot.Consolidation.DerivedRejected)
	statuses := make([]string, 0)
	for status, count := range snapshot.Evolution.CandidatesByStatus {
		statuses = append(statuses, fmt.Sprintf("%s=%d", status, count))
	}
	sort.Strings(statuses)
	if len(statuses) == 0 {
		statuses = append(statuses, "none")
	}
	fmt.Fprintf(command.Stdout, "evolution candidates: %s\n", strings.Join(statuses, " "))
	evaluationStatuses := make([]string, 0)
	for status, count := range snapshot.Evolution.EvaluationsByStatus {
		evaluationStatuses = append(evaluationStatuses, fmt.Sprintf("%s=%d", status, count))
	}
	sort.Strings(evaluationStatuses)
	fmt.Fprintf(command.Stdout, "evaluations: %s\n", strings.Join(evaluationStatuses, " "))
	fmt.Fprintf(command.Stdout, "promotions: active=%d revoked=%d cooldowns_active=%d\n",
		snapshot.Evolution.PromotionsActive, snapshot.Evolution.PromotionsRevoked, snapshot.Evolution.CooldownsActive)
	if len(snapshot.Evolution.RollbackDurationsMS) > 0 {
		fmt.Fprintf(command.Stdout, "rollback durations (ms): %v\n", snapshot.Evolution.RollbackDurationsMS)
	}
	fmt.Fprintf(command.Stdout, "recall: calls=%d avg_candidates=%.2f conflict_rate=%.2f evidence_insufficient_rate=%.2f\n",
		snapshot.Recall.TotalCalls, snapshot.Recall.AverageCandidates, snapshot.Recall.ConflictRate, snapshot.Recall.EvidenceInsufficientRate)
	return ExitOK
}
