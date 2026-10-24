// 记忆捕获、检索与派生检查。
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/memory"
)

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
