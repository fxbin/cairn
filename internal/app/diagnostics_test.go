package app

import (
	"context"
	"encoding/json"
	"errors"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// TestDoctorReportRedactsCredentialMaterial 锁定 doctor 输出的脱敏合同：只出现
// 密钥环境变量名，绝不出现其值；报告字段schema稳定。
func TestDoctorReportRedactsCredentialMaterial(t *testing.T) {
	t.Setenv("CAIRN_DOCTOR_TEST_KEY", "doctor-secret-sentinel-value")
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.APIKeyEnv = "CAIRN_DOCTOR_TEST_KEY"
	workspacePath, stateDir := t.TempDir(), t.TempDir()
	_, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		t.Fatal(err)
	}
	// doctor 只读打开已有 journal；先以可写模式初始化一次空日志。
	seed, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	report, err := service.Doctor(context.Background(), workspacePath, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "doctor-secret-sentinel-value") {
		t.Fatalf("doctor report leaked credential value: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"api_key_env":"CAIRN_DOCTOR_TEST_KEY"`) {
		t.Fatalf("doctor report missing key env name: %s", encoded)
	}
	if !report.JournalOK || !report.ArtifactsOK || !report.ConfigValid {
		t.Fatalf("report = %+v", report)
	}
	if !report.Provider.APIKeyPresent {
		t.Fatal("key presence flag must be true when env var is set")
	}
	if report.Build.Flavor != "plain" || report.Build.Fingerprint == "" {
		t.Fatalf("doctor report build identity = %+v", report.Build)
	}
	if strings.Join(report.Tools, ",") != "list_files,read_file,search_text,apply_patch,exec_command" {
		t.Fatalf("tools = %v", report.Tools)
	}
}

// TestDoctorTreatsEmptyAPIKeyAsAbsent 锁定空字符串环境变量不满足密钥前提。
func TestDoctorTreatsEmptyAPIKeyAsAbsent(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.APIKeyEnv = "CAIRN_DOCTOR_EMPTY_KEY"
	t.Setenv(configuration.Provider.APIKeyEnv, "")
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	report, err := service.Doctor(context.Background(), t.TempDir(), t.TempDir(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	if report.Provider.APIKeyPresent || report.Ready {
		t.Fatalf("empty key must be absent and not ready: %+v", report)
	}
}

// TestBuildIdentityIsPlainAndDeterministic 锁定当前二进制身份只报告真实的 plain
// flavor，并让同一份构建事实得到可复算的非敏感摘要。
func TestBuildIdentityIsPlainAndDeterministic(t *testing.T) {
	first := buildIdentity()
	second := buildIdentity()
	if first != second {
		t.Fatalf("build identity is not deterministic: first=%+v second=%+v", first, second)
	}
	if first.Flavor != "plain" || first.Fingerprint == "" || first.Fingerprint != buildIdentityFingerprint(first) {
		t.Fatalf("build identity = %+v", first)
	}
}

// TestBuildIdentityMarksMissingDirtyMetadataUnknown 锁定没有 VCS BuildInfo 时不把
// dirty 状态误报为 false，并让显式 metadata 原样进入冻结 Run 身份。
func TestBuildIdentityMarksMissingDirtyMetadataUnknown(t *testing.T) {
	missing := buildIdentityFromSettings(nil)
	if missing.Revision != "" || missing.Dirty != "unknown" || missing.Flavor != "plain" || missing.Fingerprint == "" {
		t.Fatalf("missing BuildInfo identity = %+v", missing)
	}
	known := buildIdentityFromSettings([]debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.modified", Value: "true"}})
	if known.Revision != "abc" || known.Dirty != "true" || known.Flavor != "plain" || known.Fingerprint == missing.Fingerprint {
		t.Fatalf("known BuildInfo identity = %+v", known)
	}
}

// TestDoctorReportOnFreshWorkspaceSupportsPreflight 锁定首次运行前 preflight：
// 没有 journal 的新工作区不报错，journal_state=absent，就绪判定按配置与
// Provider 前提给出，而不是失败。
func TestDoctorReportOnFreshWorkspaceSupportsPreflight(t *testing.T) {
	t.Setenv("CAIRN_DOCTOR_TEST_KEY", "present")
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.APIKeyEnv = "CAIRN_DOCTOR_TEST_KEY"
	configuration.Provider.BaseURL = "https://provider.test/v1"
	configuration.Provider.Model = "test-model"
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	report, err := service.Doctor(context.Background(), t.TempDir(), t.TempDir(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	if report.JournalState != "absent" || report.JournalOK || report.ArtifactsOK {
		t.Fatalf("journal state = %q ok=%v artifacts=%v", report.JournalState, report.JournalOK, report.ArtifactsOK)
	}
	if !report.Ready || !report.ConfigValid {
		t.Fatalf("fresh workspace preflight must be ready, report = %+v", report)
	}
	if report.ConfigFingerprint == "" {
		t.Fatal("doctor report missing config fingerprint")
	}
	// 遥测就绪态合同：库存在时记忆侧只读聚合快照与召回采集可用（snapshot_
	// available），库缺失的 preflight 分支如实报告 disabled；运行级
	// TelemetrySink（OTLP 导出）仍未接线。新增观测能力时必须同步更新本断
	// 言与投影。
	if report.Telemetry.Status != "disabled" {
		t.Fatalf("telemetry status = %q, want disabled on absent memory store", report.Telemetry.Status)
	}
	if report.Policy.Write != "ask" || report.Policy.Exec != "ask" {
		t.Fatalf("policy health = %+v", report.Policy)
	}
}

// TestDoctorReadinessFailsClosedOnMissingPrerequisites 锁定就绪判定的失败关闭：
// 密钥环境变量缺失或 Docker 后端镜像不可用时 ready=false，即使其余检查通过。
func TestDoctorReadinessFailsClosedOnMissingPrerequisites(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.BaseURL = "https://provider.test/v1"
	configuration.Provider.Model = "test-model"
	configuration.Sandbox.Backend = "host"
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	workspacePath, stateDir := t.TempDir(), t.TempDir()
	_, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := service.Doctor(context.Background(), workspacePath, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if report.Ready {
		t.Fatalf("missing API key must not be ready, report = %+v", report)
	}
	dockerConfig := withTestProvider(config.Defaults())
	dockerConfig.Provider.BaseURL = "https://provider.test/v1"
	dockerConfig.Provider.Model = "test-model"
	dockerConfig.Sandbox.Backend = "docker"
	dockerConfig.Sandbox.DockerImage = "cairn-doctor-missing-image:definitely-absent"
	t.Setenv(dockerConfig.Provider.APIKeyEnv, "present")
	dockerReport, err := service.Doctor(context.Background(), workspacePath, t.TempDir(), dockerConfig)
	if err != nil {
		t.Fatal(err)
	}
	if dockerReport.Ready {
		t.Fatalf("missing docker image must not be ready, report = %+v", dockerReport)
	}
}

// TestSummarizeRunAnswersFourQuestions 锁定 inspect 摘要：完成 run 的可信完成
// 证据绑定、预算计数与空待决；等待审批 run 给出可发现的下一步动作。
func TestSummarizeRunAnswersFourQuestions(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write = "allow"
	configuration.Policy.Exec = "allow"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	completedRun, err := service.Run(context.Background(), RunOptions{Task: "patch", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := service.Inspect(context.Background(), workspace, stateDir, configuration, completedRun.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	summary := completed.Summary
	if !summary.CompletionTrusted || summary.StopReason != "" || summary.PendingApproval || summary.NextAction != "" {
		t.Fatalf("completed summary = %+v", summary)
	}
	if summary.TurnsUsed < 1 || summary.ToolCallsStarted < 1 || summary.InputTokens < 0 {
		t.Fatalf("budget accounting empty: %+v", summary)
	}
	if summary.LastVerification == nil || !summary.LastVerification.OK || summary.LastVerification.Seq <= summary.LastMutationSeq {
		t.Fatalf("verification binding missing: %+v", summary)
	}
	if summary.ConfigFingerprint != configuration.Fingerprint() || summary.Build == nil || summary.Build.Flavor != "plain" || summary.WorkspaceHash == "" || summary.WorkspaceObservation == "" {
		t.Fatalf("run identity or observation missing: %+v", summary)
	}

	waitingService := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &patchThenFinalProvider{}, nil }}
	waitingWorkspace, waitingState := t.TempDir(), t.TempDir()
	waitingResult, err := waitingService.Run(context.Background(), RunOptions{Task: "patch", Workspace: waitingWorkspace, StateDir: waitingState, Config: withTestProvider(config.Defaults())})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) || waitingResult.ID == "" {
		t.Fatalf("expected waiting approval, got %v", err)
	}
	waitingInspect, err := waitingService.Inspect(context.Background(), waitingWorkspace, waitingState, withTestProvider(config.Defaults()), waitingResult.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	waitingSummary := waitingInspect.Summary
	if !waitingSummary.PendingApproval || !strings.Contains(waitingSummary.NextAction, "--approve") {
		t.Fatalf("waiting summary = %+v", waitingSummary)
	}
}

func TestNextActionForReconciliationIncludesRequiredEvidence(t *testing.T) {
	projection := domain.RunProjection{PendingOperation: &domain.PendingOperation{SideEffectClass: "process"}}
	action := nextActionFor(projection)
	if !strings.Contains(action, "--note <text>") || !strings.Contains(action, "--evidence <ref>") {
		t.Fatalf("next action is not executable under the CLI contract: %q", action)
	}
}
