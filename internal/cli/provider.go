// 本文件实现 provider 命令族：注册表与目录查询、合同探测、配置生成与
// 准入决定记录，构成"探测 → 准入 → run 过门"的完整闭环入口。全部命令
// 遵循仓库输出合同：--json 时 stdout 只输出 NDJSON，敏感值（密钥、端点
// 明文）不进入任何输出。
//
// Author: fxbin
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/modelcatalog"
	"github.com/fxbin/cairn/internal/provider"
	"github.com/fxbin/cairn/internal/tools"
)

// providerListLimit 是 provider models 无查询词时的默认返回条数上限。
const providerListLimit = 20

// providerCommand 分派 provider 子命令；无子命令按用法错误退出。
func (command Command) providerCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "provider requires a subcommand: list, models, probe, configure, admit, status")
		return ExitUsage
	}
	switch arguments[0] {
	case "list":
		return command.providerList(ctx, arguments[1:])
	case "models":
		return command.providerModels(ctx, arguments[1:])
	case "probe":
		return command.checkProvider(ctx, arguments[1:])
	case "configure":
		return command.providerConfigure(ctx, arguments[1:])
	case "admit":
		return command.providerAdmit(ctx, arguments[1:])
	case "status":
		return command.providerStatus(ctx, arguments[1:])
	default:
		fmt.Fprintf(command.Stderr, "unknown provider subcommand %q\n", arguments[0])
		return ExitUsage
	}
}

// providerListEntry 是注册表成员的非敏感投影。
type providerListEntry struct {
	ProviderID string `json:"provider_id"`
	ProtocolID string `json:"protocol_id"`
}

// providerConfigureReport 是 provider configure --json 的稳定输出合同。
type providerConfigureReport struct {
	ConfigPath     string   `json:"config_path"`
	CatalogMatched bool     `json:"catalog_matched"`
	ProbeRan       bool     `json:"probe_ran"`
	Notes          []string `json:"notes"`
}

// providerAdmissionReport 是 provider admission 检查的稳定输出合同。
type providerAdmissionReport struct {
	Admission          string `json:"admission"`
	RequestFingerprint string `json:"request_profile_fingerprint"`
	GateError          string `json:"gate_error,omitempty"`
	HeadDecision       string `json:"head_decision,omitempty"`
	HeadEvidence       string `json:"head_evidence,omitempty"`
	HeadFingerprint    string `json:"head_fingerprint,omitempty"`
	Allowed            bool   `json:"allowed"`
}

// providerList 列出静态注册表成员与各自绑定的规范协议；注册表是编译期
// 冻结的闭合集合，不存在动态发现。
func (command Command) providerList(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("provider list", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	entries := []providerListEntry{{ProviderID: "custom", ProtocolID: "openai-chat-completions"}}
	if *jsonOutput {
		for _, entry := range entries {
			if err := writeJSONChecked(command.Stdout, entry); err != nil {
				fmt.Fprintln(command.Stderr, "provider list output failed:", err)
				return ExitFailed
			}
		}
		return ExitOK
	}
	for _, entry := range entries {
		fmt.Fprintf(command.Stdout, "%s\t%s\n", entry.ProviderID, entry.ProtocolID)
	}
	return ExitOK
}

// providerModels 按查询词列出模型元数据目录条目；无查询词时返回前若干条
// 作为目录概览。目录是外部建议来源，任何条目都不构成已验证能力。
func (command Command) providerModels(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("provider models", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	query := flags.String("query", "", "model name filter (empty lists the catalog head)")
	catalogURL := flags.String("catalog-url", "", "model metadata catalog URL override")
	catalogCache := flags.String("catalog-cache", "", "model metadata cache file path override")
	limit := flags.Int("limit", providerListLimit, "maximum entries to list")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	options := modelcatalog.Options{CatalogURL: *catalogURL}
	if *catalogCache != "" {
		options.CachePath = *catalogCache
	} else if cachePath, err := modelcatalog.DefaultCachePath(); err == nil {
		options.CachePath = cachePath
	}
	catalog, err := modelcatalog.Load(options)
	if err != nil {
		fmt.Fprintln(command.Stderr, "catalog error:", err)
		return ExitFailed
	}
	entries := catalog.Entries
	if *query != "" {
		entries = catalog.Lookup(*query)
	}
	if len(entries) > *limit {
		entries = entries[:*limit]
	}
	for _, entry := range entries {
		if *jsonOutput {
			if err := writeJSONChecked(command.Stdout, entry); err != nil {
				fmt.Fprintln(command.Stderr, "provider models output failed:", err)
				return ExitFailed
			}
			continue
		}
		fmt.Fprintf(command.Stdout, "%s\tcontext=%d\toutput=%d\ttool_call=%t\n", entry.Name, entry.ContextTokens, entry.OutputTokens, entry.ToolCall)
	}
	return ExitOK
}

// providerConfigure 以显式旗标生成 v2 配置；admission 默认 required，只有
// 显式给出 optional 才生成非阻断配置。语义全部复用应用层 SetupService
// （目录预填、密钥在环境时的零期望探测、回读自证）。
func (command Command) providerConfigure(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("provider configure", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	baseURL := flags.String("base-url", "", "provider base URL (required)")
	model := flags.String("model", "", "model name (required)")
	apiKeyEnv := flags.String("api-key-env", "", "environment variable name holding the API key (required)")
	admission := flags.String("admission", "", "admission gate mode: optional|required (default: required)")
	catalogURL := flags.String("catalog-url", "", "model metadata catalog URL override")
	catalogCache := flags.String("catalog-cache", "", "model metadata cache file path override")
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	userScope := flags.Bool("user", false, "write the user-level config shared by all workspaces (default: workspace config)")
	overwrite := flags.Bool("overwrite", false, "replace an existing config (backs it up as config.yaml.bak)")
	jsonOutput := flags.Bool("json", false, "write JSON to stdout")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	if *baseURL == "" || *model == "" || *apiKeyEnv == "" {
		fmt.Fprintln(command.Stderr, "provider configure requires --base-url, --model and --api-key-env")
		return ExitUsage
	}
	input := app.SetupInput{
		BaseURL: *baseURL, Model: *model, APIKeyEnv: *apiKeyEnv,
		CatalogURL: *catalogURL, CatalogCacheDir: *catalogCache, Workspace: *workspace,
		Admission: *admission, OverwriteExisting: *overwrite, ScopeUserConfig: *userScope,
	}
	outcome, err := app.NewSetupService().Run(ctx, input)
	if err != nil {
		fmt.Fprintln(command.Stderr, "configure failed:", err)
		return ExitFailed
	}
	if *jsonOutput {
		if err := writeJSONChecked(command.Stdout, providerConfigureReport{ConfigPath: outcome.ConfigPath, CatalogMatched: outcome.CatalogMatched, ProbeRan: outcome.ProbeRan, Notes: outcome.Notes}); err != nil {
			fmt.Fprintln(command.Stderr, "configure output failed:", err)
			return ExitFailed
		}
		return ExitOK
	}
	fmt.Fprintf(command.Stdout, "配置已生成: %s\n", outcome.ConfigPath)
	for _, note := range outcome.Notes {
		fmt.Fprintf(command.Stdout, "  %s\n", note)
	}
	return ExitOK
}

// providerAdmit 由维护者基于在档证据写入准入决定：必填证据指纹与结论
// （admitted/rejected），可选 supersedes 引用同分区既有决定形成链。权威
// 固定为 maintainer；时间戳取系统时钟。
func (command Command) providerAdmit(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("provider admit", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	evidenceFingerprint := flags.String("evidence", "", "evidence fingerprint from provider probe (required)")
	decisionValue := flags.String("decision", "", "admitted or rejected (required)")
	reason := flags.String("reason", "", "bounded, non-sensitive reason (required)")
	supersedes := flags.String("supersedes", "", "decision fingerprint this one supersedes (optional)")
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	configPath := flags.String("config", "", "configuration path")
	stateDir := flags.String("state-dir", "", "override the ignored Cairn state directory")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	if *evidenceFingerprint == "" || *reason == "" || (*decisionValue != provider.DecisionAdmitted && *decisionValue != provider.DecisionRejected) {
		fmt.Fprintln(command.Stderr, "provider admit requires --evidence, --decision admitted|rejected and --reason")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	workspaceHash, err := cliWorkspaceHash(*workspace, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "workspace error:", err)
		return ExitFailed
	}
	if *stateDir == "" {
		*stateDir = app.DefaultStateDir()
	}
	evidenceStore, err := provider.OpenEvidenceStore(*stateDir, workspaceHash)
	if err != nil {
		fmt.Fprintln(command.Stderr, "evidence store error:", err)
		return ExitFailed
	}
	evidence, err := evidenceStore.Get(*evidenceFingerprint)
	if err != nil {
		fmt.Fprintln(command.Stderr, "evidence error:", err)
		return ExitFailed
	}
	profile, requestFingerprint, err := resolvedProfileForConfig(configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "profile error:", err)
		return ExitFailed
	}
	if evidence.Selection != profile.Selection {
		fmt.Fprintln(command.Stderr, "evidence selection does not match the current configuration binding")
		return ExitFailed
	}
	decision := provider.ProviderAdmissionDecision{
		SchemaVersion: 1, EvidenceFingerprint: *evidenceFingerprint,
		Binding:  provider.AdmissionBinding{RequestProfileFingerprint: requestFingerprint, ConfigFingerprint: configuration.Fingerprint()},
		Decision: *decisionValue, DecidedAt: time.Now().UTC(),
		Authority: provider.AuthorityMaintainer, Reason: *reason,
		SupersedesDecisionFingerprint: *supersedes,
	}
	decisions, err := provider.OpenDecisionStore(*stateDir, workspaceHash)
	if err != nil {
		fmt.Fprintln(command.Stderr, "decision store error:", err)
		return ExitFailed
	}
	fingerprint, err := decisions.Append(&decision)
	if err != nil {
		fmt.Fprintln(command.Stderr, "admit failed:", err)
		return ExitFailed
	}
	// 追加后立即校验链完整性：断链/环/多链头在写入当下暴露，而不是等到
	// run 过门时才发现分区损坏。
	partition, err := decisions.ListPartition(requestFingerprint)
	if err != nil {
		fmt.Fprintln(command.Stderr, "decision store corrupted:", err)
		return ExitFailed
	}
	if _, err := provider.ResolveChainHead(partition); err != nil {
		fmt.Fprintln(command.Stderr, "admission chain corrupted:", err)
		return ExitFailed
	}
	fmt.Fprintf(command.Stdout, "决定已记录: %s\n", fingerprint)
	return ExitOK
}

// providerStatus 展示当前 v2 配置绑定的准入状态：optional 如实显示链头或
// "未准入"；required 顺带执行门判定。展示口径遵循合同：声明与观测不得
// 展示为已验证，只有链头决定是合同状态。
func (command Command) providerStatus(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("provider status", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	configPath := flags.String("config", "", "configuration path")
	stateDir := flags.String("state-dir", "", "override the ignored Cairn state directory")
	jsonOutput := flags.Bool("json", false, "write JSON to stdout")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	workspaceHash, err := cliWorkspaceHash(*workspace, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "workspace error:", err)
		return ExitFailed
	}
	if *stateDir == "" {
		*stateDir = app.DefaultStateDir()
	}
	profile, requestFingerprint, err := resolvedProfileForConfig(configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "profile error:", err)
		return ExitFailed
	}
	decisions, err := provider.OpenDecisionStore(*stateDir, workspaceHash)
	if err != nil {
		fmt.Fprintln(command.Stderr, "decision store error:", err)
		return ExitFailed
	}
	evidenceStore, err := provider.OpenEvidenceStore(*stateDir, workspaceHash)
	if err != nil {
		fmt.Fprintln(command.Stderr, "evidence store error:", err)
		return ExitFailed
	}
	outcome, err := provider.EvaluateAdmissionGate(decisions, evidenceStore, profile, configuration.Provider.Admission)
	report := providerAdmissionReport{
		Admission:          configuration.Provider.Admission,
		RequestFingerprint: requestFingerprint,
	}
	if err != nil {
		report.GateError = err.Error()
	}
	if outcome.Head != nil {
		report.HeadDecision = string(outcome.Head.Decision)
		report.HeadEvidence = outcome.Head.EvidenceFingerprint
		report.HeadFingerprint = outcome.Head.DecisionFingerprint
	}
	report.Allowed = err == nil && outcome.Allowed
	if *jsonOutput {
		if writeErr := writeJSONChecked(command.Stdout, report); writeErr != nil {
			fmt.Fprintln(command.Stderr, "admission output failed:", writeErr)
			return ExitFailed
		}
	} else {
		if outcome.Head != nil {
			fmt.Fprintf(command.Stdout, "链头决定: %s（证据 %s）\n", outcome.Head.Decision, outcome.Head.EvidenceFingerprint)
		} else {
			fmt.Fprintln(command.Stdout, "未准入：该绑定没有任何准入决定")
		}
		if err != nil {
			fmt.Fprintf(command.Stdout, "门评估: %v\n", err)
		}
		fmt.Fprintf(command.Stdout, "admission=%s allowed=%t\n", configuration.Provider.Admission, report.Allowed)
	}
	if configuration.Provider.Admission == config.ProviderAdmissionRequired && !report.Allowed {
		return ExitFailed
	}
	return ExitOK
}

// resolvedProfileForConfig 由当前配置组装解析身份并返回身份选择与请求合
// 同指纹，供 admit/status 与配置绑定对齐（与 run 前门使用同一组装路径）。
func resolvedProfileForConfig(configuration config.Config) (contract.ResolvedProviderProfile, string, error) {
	profile, err := provider.ResolveResolvedProfile(provider.ResolvedFacts{
		Selection:     provider.SelectionFacts{ProviderID: configuration.Provider.ID, Model: configuration.Provider.Model, BaseURL: configuration.Provider.BaseURL},
		Capability:    config.ProviderCapabilityDeclaration(configuration),
		Generation:    config.ProviderGenerationDeclaration(configuration),
		CredentialRef: configuration.Provider.APIKeyEnv,
	})
	if err != nil {
		return contract.ResolvedProviderProfile{}, "", err
	}
	fingerprint, err := profile.Fingerprint()
	if err != nil {
		return contract.ResolvedProviderProfile{}, "", err
	}
	return profile, fingerprint, nil
}

// cliWorkspaceHash 复刻 app 装配的工作区哈希算法（符号链接解析后的绝对
// 路径摘要），保证证据与决定分区和 run 门完全一致；与探测落盘共用同一
// 实现。
func cliWorkspaceHash(workspacePath string, configuration config.Config) (string, error) {
	root := workspacePath
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = current
	}
	workspace, err := tools.NewWorkspace(root, configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(workspace.Root))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
