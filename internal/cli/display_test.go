// display_test 锁定 CLI 的人类视图复用统一展示词表：状态行、完成证据等级与
// 对账指引都取自 contract 词表；--json 的 NDJSON 契约不受影响。
//
// Author: fxbin
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// TestInspectHumanViewUsesContractPhaseLabel 锁定 inspect 的状态行使用统一
// 词表的展示词，并保留原始阶段取值。
func TestInspectHumanViewUsesContractPhaseLabel(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeCapabilityConfig(t, workspace)
	configuration := withTestProvider(config.Defaults())
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}
	result, err := service.Run(context.Background(), app.RunOptions{Task: "display probe", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result == nil {
		t.Fatal("运行没有产出合同结果")
	}
	phase := result.Result.Outcome
	var stdout, stderr bytes.Buffer
	command := Command{Service: service, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"inspect", "--workspace", workspace, "--state-dir", stateDir, string(result.ID)}); code != ExitOK {
		t.Fatalf("inspect exit=%d stderr=%q", code, stderr.String())
	}
	expected := "state: " + contract.RunPhaseLabel(phase) + " (" + phase + ")"
	if !strings.Contains(stdout.String(), expected) {
		t.Fatalf("inspect 状态行缺少统一展示词 %q: %s", expected, stdout.String())
	}
}

// TestInspectHumanViewUsesContractTrustLabel 锁定 inspect 的完成证据行使用
// 统一词表的可信等级，并且三条等级都有展示。
func TestInspectHumanViewUsesContractTrustLabel(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeCapabilityConfig(t, workspace)
	configuration := withTestProvider(config.Defaults())
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}
	result, err := service.Run(context.Background(), app.RunOptions{Task: "trust probe", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	verificationSeq := uint64(0)
	if inspected.Summary.LastVerification != nil {
		verificationSeq = inspected.Summary.LastVerification.Seq
	}
	trust := contract.ClassifyCompletionTrust(inspected.Summary.CompletionTrusted, verificationSeq)
	var stdout, stderr bytes.Buffer
	command := Command{Service: service, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"inspect", "--workspace", workspace, "--state-dir", stateDir, string(result.ID)}); code != ExitOK {
		t.Fatalf("inspect exit=%d stderr=%q", code, stderr.String())
	}
	expected := "completion: " + contract.CompletionTrustLabel(trust)
	if !strings.Contains(stdout.String(), expected) {
		t.Fatalf("inspect 完成证据行缺少统一展示词 %q: %s", expected, stdout.String())
	}
}

// TestEvidenceHumanViewUsesContractLabels 锁定 evidence 的人类视图使用统一
// 阶段词与可信等级；原始字段仍在，机器视图契约不变。
func TestEvidenceHumanViewUsesContractLabels(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeCapabilityConfig(t, workspace)
	configuration := withTestProvider(config.Defaults())
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}
	result, err := service.Run(context.Background(), app.RunOptions{Task: "evidence probe", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result == nil {
		t.Fatal("运行没有产出合同结果")
	}
	var stdout, stderr bytes.Buffer
	command := Command{Service: service, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"evidence", "--workspace", workspace, "--state-dir", stateDir, string(result.ID)}); code != ExitOK {
		t.Fatalf("evidence exit=%d stderr=%q", code, stderr.String())
	}
	for _, required := range []string{
		"outcome: " + result.Result.Outcome,
		contract.RunPhaseLabel(result.Result.Outcome),
		"trusted=",
		contract.CompletionTrustLabel(contract.CompletionTrustOf(result.Result.Evidence)),
	} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("evidence 人类视图缺少 %q: %s", required, stdout.String())
		}
	}
}
