package app

import (
	"context"
	"encoding/json"
	"errors"
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
	configuration := config.Defaults()
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
	if strings.Join(report.Tools, ",") != "list_files,read_file,search_text,apply_patch,exec_command" {
		t.Fatalf("tools = %v", report.Tools)
	}
}

// TestSummarizeRunAnswersFourQuestions 锁定 inspect 摘要：完成 run 的可信完成
// 证据绑定、预算计数与空待决；等待审批 run 给出可发现的下一步动作。
func TestSummarizeRunAnswersFourQuestions(t *testing.T) {
	provider := &patchThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := config.Defaults()
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

	waitingService := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &patchThenFinalProvider{}, nil }}
	waitingWorkspace, waitingState := t.TempDir(), t.TempDir()
	waitingResult, err := waitingService.Run(context.Background(), RunOptions{Task: "patch", Workspace: waitingWorkspace, StateDir: waitingState, Config: config.Defaults()})
	var waiting *agent.WaitingApprovalError
	if !errors.As(err, &waiting) || waitingResult.ID == "" {
		t.Fatalf("expected waiting approval, got %v", err)
	}
	waitingInspect, err := waitingService.Inspect(context.Background(), waitingWorkspace, waitingState, config.Defaults(), waitingResult.ID, false)
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
