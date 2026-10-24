// memory candidate 生命周期子命令。
// Author: fxbin
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/benchmark"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

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
