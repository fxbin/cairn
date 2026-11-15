// [INPUT]: 非法钩子配置、真实执行入口与隔离状态目录。
// [OUTPUT]: 验证执行前置拒绝、冻结规则与Doctor口径。
// [POS]: 应用层Hooks准入回归；不发送真实模型请求。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// TestAssembleBootRejectsInvalidHooksBeforeResources 禁止静默丢弃误填的安全规则。
func TestAssembleBootRejectsInvalidHooksBeforeResources(t *testing.T) {
	for _, rule := range []config.HookConfig{
		{ID: "deny", Event: "pre_tool", Effect: "denny", Enabled: true},
		{ID: "deny", Event: "unknown", Effect: "deny", Enabled: true},
		{ID: "", Event: "pre_tool", Effect: "deny", Enabled: true},
	} {
		t.Run(rule.ID+"/"+rule.Event+"/"+rule.Effect, func(t *testing.T) {
			configuration := withTestProvider(config.Defaults())
			configuration.Hooks = []config.HookConfig{rule}
			called := false
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
				called = true
				return nil, errors.New("unexpected provider construction")
			}}
			state := filepath.Join(t.TempDir(), "state")
			boot, err := service.assembleBoot(t.TempDir(), state, configuration, nil)
			if boot != nil {
				_ = boot.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "hook") || called {
				t.Fatalf("invalid hook not rejected before resources: boot=%v provider=%v err=%v", boot != nil, called, err)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("invalid hook created state directory: %v", err)
			}
		})
	}
}

// TestAssembleBootFreezesHookPolicyAndActions 同一规则投影显式接入约束和动作。
func TestAssembleBootFreezesHookPolicyAndActions(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	rule := config.HookConfig{ID: "frozen", Event: "pre_tool", Effect: "deny", Reason: "frozen", Enabled: true}
	rule.Actions = append(rule.Actions, struct {
		Kind    string `yaml:"kind"`
		Content string `yaml:"content,omitempty"`
	}{Kind: "queue_followup", Content: "frozen"})
	configuration.Hooks = []config.HookConfig{rule}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	boot, err := service.assembleBoot(t.TempDir(), t.TempDir(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	configuration.Hooks[0].Effect = "allow"
	configuration.Hooks[0].Actions[0].Content = "changed"
	decision, err := boot.Gate.Decide("read_file", json.RawMessage(`{}`))
	if err != nil || decision.Effect != contract.AuthorizationDeny || decision.RuleID != "hook_deny" || decision.Summary != "frozen" {
		t.Fatalf("hook authorization=%+v err=%v", decision, err)
	}
	if len(boot.Gate.ToolConstraints) != 1 || len(boot.Loop.Hooks) != 1 || len(boot.Loop.Hooks[0].Actions) != 1 || boot.Loop.Hooks[0].Actions[0].Content != "frozen" {
		t.Fatalf("hook projection drifted: %+v", boot.Loop.Hooks)
	}
}

// TestExecutionEntriesRejectInvalidHooksBeforeState 公共执行入口不得先写状态再拒绝规则。
func TestExecutionEntriesRejectInvalidHooksBeforeState(t *testing.T) {
	for _, entry := range []string{"run", "resume", "reconcile", "workflow_run", "workflow_resume", "workflow_accept"} {
		t.Run(entry, func(t *testing.T) {
			configuration := withTestProvider(config.Defaults())
			configuration.Hooks = []config.HookConfig{{ID: "deny", Event: "pre_tool", Effect: "denny", Enabled: true}}
			configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
			called := false
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
				called = true
				return nil, errors.New("unexpected provider construction")
			}}
			workspace, state := t.TempDir(), filepath.Join(t.TempDir(), "state")
			var err error
			switch entry {
			case "run":
				_, err = service.Run(context.Background(), RunOptions{Task: "inspect", Workspace: workspace, StateDir: state, Config: configuration})
			case "resume":
				_, err = service.Resume(context.Background(), ResumeOptions{Workspace: workspace, StateDir: state, Config: configuration})
			case "reconcile":
				_, err = service.Reconcile(context.Background(), ReconcileOptions{Verdict: "failed", Workspace: workspace, StateDir: state, Config: configuration})
			case "workflow_run":
				_, err = service.WorkflowRun(context.Background(), WorkflowRunOptions{Task: "inspect", Workspace: workspace, StateDir: state, Config: configuration})
			case "workflow_resume":
				_, err = service.WorkflowResume(context.Background(), WorkflowResumeOptions{Workspace: workspace, StateDir: state, Config: configuration})
			case "workflow_accept":
				_, err = service.WorkflowAccept(context.Background(), WorkflowAcceptOptions{Workspace: workspace, StateDir: state, Config: configuration})
			}
			if err == nil || !strings.Contains(err.Error(), "hook") || called {
				t.Errorf("entry did not reject invalid hook: provider=%v err=%v", called, err)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Errorf("invalid hook created state: %v", err)
			}
		})
	}
}

// TestDoctorReportsInvalidHooks 配置就绪不能与实际能力准入分叉。
func TestDoctorReportsInvalidHooks(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Hooks = []config.HookConfig{{ID: "deny", Event: "pre_tool", Effect: "denny", Enabled: true}}
	report, err := (Service{}).Doctor(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "state"), configuration)
	if report.ConfigValid || report.Ready {
		t.Fatalf("invalid hooks reported ready: valid=%v ready=%v err=%v", report.ConfigValid, report.Ready, err)
	}
}
