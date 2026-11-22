// verifier_run_test.go 端到端回归独立只读 Verifier 子运行与结构化输出。
//
// Author: fxbin
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
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// verifierJSONProvider 输出结构化 Finding JSON 作为子 Run 终态文本。
type verifierJSONProvider struct {
	text   string
	prompt string
}

func (provider *verifierJSONProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	for _, message := range request.Messages {
		provider.prompt += message.Content
	}
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: provider.text}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// seedVerifierParent 在父 Journal 写入一个最小 run_created/run_started，
// 供委派事实与子运行启动核对父身份。
func seedVerifierParent(t *testing.T, workspace, stateDir string, cfg config.Config, parentID domain.RunID) {
	t.Helper()
	_, hash, err := workspaceFor(workspace, cfg)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{
		RunID: parentID, WorkspaceHash: hash, Type: domain.EventRunCreated,
		Payload: mustJSON(t, domain.RunCreatedPayload{Task: "worker task", Workspace: workspace, ConfigFingerprint: cfg.Fingerprint()}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{
		RunID: parentID, WorkspaceHash: hash, Type: domain.EventRunStarted,
	}); err != nil {
		t.Fatal(err)
	}
}

// seedVerifierTarget 通过真实运行入口生成已落盘的 Worker 与程序验证制品。
func seedVerifierTarget(t *testing.T, request *VerifierRequest) {
	t.Helper()
	worker := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	result, err := worker.Run(context.Background(), RunOptions{
		Task: "Worker 自述不得进入独立验证", Workspace: request.Workspace,
		StateDir: request.StateDir, Config: request.Config, Acceptance: &request.Acceptance,
	})
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}
	request.TargetRunID = result.ID
}

func TestRunVerifierProducesPendingFindings(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	request := verifierTestRequest(t)
	request.Workspace, request.StateDir = workspace, stateDir
	parentID, targetID := domain.NewRunID(), domain.NewRunID()
	request.ParentRunID, request.TargetRunID = parentID, targetID
	// 父工具面：coding 父运行能看到全部基础工具，子只读工具面是其子集。
	request.ParentTools = verifierParentToolNames()

	findingJSON := `{"findings":[{"id":"v-1","target_id":"obj-1","category":"correctness","severity":"high","description":"独立验证发现问题","status":"confirmed","criterion_id":"compile"}]}`
	provider := &verifierJSONProvider{text: findingJSON}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	seedVerifierTarget(t, &request)
	seedVerifierParent(t, workspace, stateDir, request.Config, parentID)

	result, err := service.RunVerifier(context.Background(), request)
	if err != nil {
		t.Fatalf("RunVerifier: %v", err)
	}
	if result.VerifierRunID == "" {
		t.Fatal("no verifier run id")
	}
	target := result.target
	if target.TargetRunID != request.TargetRunID || target.TargetVersion == "" || target.TerminalSequence == 0 ||
		target.WorkspaceObservation == "" || len(target.ProgramCriteria) != 1 || len(target.Artifacts) == 0 || target.Artifacts[0].Ref.SHA256 == "" {
		t.Fatalf("incomplete target context: %+v", target)
	}
	for _, want := range []string{target.TargetVersion, "项目可编译", target.Artifacts[0].Ref.SHA256} {
		if !strings.Contains(provider.prompt, want) {
			t.Fatalf("target context missing %q", want)
		}
	}
	if strings.Contains(provider.prompt, "Worker 自述不得进入独立验证") {
		t.Fatal("worker prompt leaked into verification")
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(result.Findings))
	}
	// 模型自报 confirmed，必须降级为 Proposed 待核验。
	if result.Findings[0].Status != contract.FindingProposed {
		t.Fatalf("finding status = %s, want proposed", result.Findings[0].Status)
	}
}

func TestRunVerifierRejectsEmptyOutput(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	request := verifierTestRequest(t)
	request.Workspace, request.StateDir = workspace, stateDir
	parentID := domain.NewRunID()
	request.ParentRunID, request.TargetRunID = parentID, domain.NewRunID()
	request.ParentTools = verifierParentToolNames()

	provider := &verifierJSONProvider{text: "没有发现问题"}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	seedVerifierTarget(t, &request)
	seedVerifierParent(t, workspace, stateDir, request.Config, parentID)

	if _, err := service.RunVerifier(context.Background(), request); !errors.Is(err, ErrVerifierOutput) {
		t.Fatalf("invalid output returned %v, want ErrVerifierOutput", err)
	}
}

func TestRunVerifierRejectsInvalidRequest(t *testing.T) {
	service := Service{}
	request := verifierTestRequest(t)
	request.ParentRunID = "" // 缺少父 run
	if _, err := service.RunVerifier(context.Background(), request); err == nil {
		t.Fatal("verifier request without parent was accepted")
	}
}

// verifierParentToolNames 返回 coding 父运行可见的基础工具名，覆盖只读子工具面。
func verifierParentToolNames() []string {
	return []string{
		"list_files", "read_file", "search_text", "todo", "git_status",
		"git_diff", "git_log", "git_commit", "apply_patch", "exec_command",
		"send_message", "delegate_task", "memory_recall",
	}
}

func TestRunVerifierClampsBudgetToParentRemaining(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	request := verifierTestRequest(t)
	request.Workspace, request.StateDir = workspace, stateDir
	parentID := domain.NewRunID()
	request.ParentRunID, request.TargetRunID = parentID, domain.NewRunID()
	request.ParentTools = verifierParentToolNames()
	// 父配置只允许 1 轮，且父已开 1 轮 → 剩余 0 → 应失败关闭。
	request.Config.Budget.MaxTurns = 1
	provider := &verifierJSONProvider{text: `{"findings":[]}`}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	seedVerifierTarget(t, &request)
	seedVerifierParent(t, workspace, stateDir, request.Config, parentID)
	// 在父 Journal 追加一个 turn_opened 消耗掉唯一预算。
	_, hash, _ := workspaceFor(workspace, request.Config)
	journal, _ := openJournal(stateDir, hash)
	turn := domain.NewTurnID()
	journal.Append(context.Background(), 2, domain.EventDraft{
		RunID: parentID, WorkspaceHash: hash, Type: domain.EventTurnOpened, TurnID: &turn,
		Payload: mustJSON(t, struct {
			Turn int `json:"turn"`
		}{Turn: 1}),
	})
	journal.Close()
	if _, err := service.RunVerifier(context.Background(), request); err == nil || !strings.Contains(err.Error(), "no remaining turns") {
		t.Fatalf("exhausted parent budget returned %v", err)
	}
}

func TestVerifierJSONProviderShape(t *testing.T) {
	// 锁定测试替身输出形状：可被 parseVerifierOutput 直接消费。
	var output verifierOutput
	if err := json.Unmarshal([]byte(`{"findings":[]}`), &output); err != nil {
		t.Fatalf("output shape: %v", err)
	}
}

func TestRunVerifierRejectsUnboundTargetBeforeDelegation(t *testing.T) {
	for _, mode := range []string{"missing", "running", "foreign_workspace", "foreign_identity", "workspace_drift", "contract_drift"} {
		t.Run(mode, func(t *testing.T) {
			request := verifierTestRequest(t)
			request.Workspace, request.StateDir = t.TempDir(), t.TempDir()
			request.ParentRunID, request.TargetRunID = domain.NewRunID(), domain.NewRunID()
			request.ParentTools = verifierParentToolNames()
			if mode != "workspace_drift" && mode != "contract_drift" {
				seedVerifierParent(t, request.Workspace, request.StateDir, request.Config, request.ParentRunID)
			}
			if mode == "running" {
				seedVerifierParent(t, request.Workspace, request.StateDir, request.Config, request.TargetRunID)
			}
			if mode == "workspace_drift" || mode == "contract_drift" {
				// 先完成 Worker，再改变工作区并创建运行中的父夹具。
				seedVerifierTarget(t, &request)
				seedVerifierParent(t, request.Workspace, request.StateDir, request.Config, request.ParentRunID)
				switch mode {
				case "workspace_drift":
					if err := os.WriteFile(filepath.Join(request.Workspace, "drift.txt"), []byte("new contents"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "contract_drift":
					criteria := contract.CloneAcceptanceCriteria(request.Acceptance.Criteria)
					criteria[0].Statement = "一个不同的验收标准"
					changed, err := contract.NewAcceptanceContract(criteria)
					if err != nil {
						t.Fatal(err)
					}
					request.Acceptance = changed
				}
			}
			if mode == "foreign_workspace" {
				foreign := request
				foreign.Workspace = t.TempDir()
				seedVerifierTarget(t, &foreign)
				request.TargetRunID = foreign.TargetRunID
			}
			_, hash, err := workspaceFor(request.Workspace, request.Config)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := openJournal(request.StateDir, hash)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			if mode == "foreign_identity" {
				_, err = journal.Append(context.Background(), 0, domain.EventDraft{
					RunID: request.TargetRunID, WorkspaceHash: "foreign", Type: domain.EventRunCreated,
					Payload: mustJSON(t, domain.RunCreatedPayload{Task: "foreign", Workspace: request.Workspace}),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
				t.Fatal("provider called before target validation")
				return nil, nil
			}}
			_, err = service.RunVerifier(context.Background(), request)
			if !errors.Is(err, ErrVerifierTarget) {
				t.Fatalf("RunVerifier: got %v, want ErrVerifierTarget", err)
			}
			events, err := journal.Load(context.Background(), request.ParentRunID)
			if err != nil || len(events) != 2 {
				t.Fatalf("parent journal changed: %d events, err %v", len(events), err)
			}
		})
	}
}
