// 晋升、沙箱激活与遥测。
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
	"github.com/fxbin/cairn/internal/memory"
)

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
