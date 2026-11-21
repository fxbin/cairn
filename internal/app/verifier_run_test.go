// verifier_run_test.go 端到端回归独立只读 Verifier 子运行与结构化输出。
//
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// verifierJSONProvider 输出结构化 Finding JSON 作为子 Run 终态文本。
type verifierJSONProvider struct {
	text string
}

func (provider *verifierJSONProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: provider.text}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// seedVerifierParent 在父 Journal 写入一个最小 run_created/run_started，
// 供委派事实与子运行启动核对父身份。
func seedVerifierParent(t *testing.T, service Service, workspace, stateDir string, cfg config.Config, parentID domain.RunID) {
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
	seedVerifierParent(t, service, workspace, stateDir, request.Config, parentID)

	result, err := service.RunVerifier(context.Background(), request)
	if err != nil {
		t.Fatalf("RunVerifier: %v", err)
	}
	if result.VerifierRunID == "" {
		t.Fatal("no verifier run id")
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
	seedVerifierParent(t, service, workspace, stateDir, request.Config, parentID)

	if _, err := service.RunVerifier(context.Background(), request); err == nil {
		t.Fatal("non-JSON verifier output was accepted")
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
	seedVerifierParent(t, service, workspace, stateDir, request.Config, parentID)
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
	if _, err := service.RunVerifier(context.Background(), request); err == nil {
		t.Fatal("verifier run with exhausted parent budget was accepted")
	}
}

func TestVerifierJSONProviderShape(t *testing.T) {
	// 锁定测试替身输出形状：可被 parseVerifierOutput 直接消费。
	var output verifierOutput
	if err := json.Unmarshal([]byte(`{"findings":[]}`), &output); err != nil {
		t.Fatalf("output shape: %v", err)
	}
}
