// subagent_start_test 锁定 AGT-02 受控启动：工具收窄、委派校验、无虚构身份。
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// TestNarrowToolsEnforcesSubset 锁定子工具集 ⊆ 父工具集。
func TestNarrowToolsEnforcesSubset(t *testing.T) {
	got, err := NarrowTools([]string{"read_file", "apply_patch"}, []string{"read_file"})
	if err != nil || len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("收窄应成功: %v %v", got, err)
	}
	if _, err := NarrowTools([]string{"read_file"}, []string{"exec_command"}); !errors.Is(err, ErrSubagentStart) {
		t.Fatalf("越界应拒绝: %v", err)
	}
	// 去重
	got, err = NarrowTools([]string{"a", "b"}, []string{"a", "a", "b"})
	if err != nil || len(got) != 2 {
		t.Fatalf("应去重: %v %v", got, err)
	}
}

// TestPrepareSubagentStartRejectsFictional 锁定无委派事实时拒绝虚构子代理。
func TestPrepareSubagentStartRejectsFictional(t *testing.T) {
	journal := newCancelMemJournal()
	ctx := context.Background()
	parent := domain.RunID("run-p")
	child := domain.RunID("run-c")
	appendCancelEvent(t, journal, parent, domain.EventRunCreated, `{"payload_version":1,"task":"parent","workspace":"."}`)
	// 无 subagent_delegated：必须失败关闭
	_, err := PrepareSubagentStart(ctx, journal, StartSubagentOptions{
		ParentRunID: parent, ChildRunID: child, Task: "t",
		WorkspaceHash: "ws", ParentTools: []string{"read_file"}, ChildTools: []string{"read_file"},
	})
	if !errors.Is(err, ErrSubagentStart) {
		t.Fatalf("无委派应拒绝: %v", err)
	}
}

// TestPrepareSubagentStartNarrowsAndChecksBudget 锁定有委派时可启动且预算收窄。
func TestPrepareSubagentStartNarrowsAndChecksBudget(t *testing.T) {
	journal := newCancelMemJournal()
	ctx := context.Background()
	parent := domain.RunID("run-p")
	child := domain.RunID("run-c")
	appendCancelEvent(t, journal, parent, domain.EventRunCreated, `{"payload_version":1,"task":"parent","workspace":"."}`)
	appendCancelEvent(t, journal, parent, domain.EventSubagentDelegated,
		`{"payload_version":1,"child_run_id":"run-c","task_digest":"`+TaskDigest("t")+`","budget":{"max_turns":2,"max_tokens":100}}`)
	cfg := config.Config{Budget: config.BudgetConfig{MaxTurns: 5, MaxInputTokens: 200, MaxOutputTokens: 100}}
	got, err := PrepareSubagentStart(ctx, journal, StartSubagentOptions{
		ParentRunID: parent, ChildRunID: child, Task: "t",
		WorkspaceHash: "ws", ParentTools: []string{"read_file", "exec_command"}, ChildTools: []string{"read_file"},
		Config: cfg,
	})
	if err != nil {
		t.Fatalf("应可启动: %v", err)
	}
	if got.Budget.MaxTurns != 2 {
		t.Fatalf("轮次预算应被委派收窄到 2，得到 %d", got.Budget.MaxTurns)
	}
	// 任务摘要不符
	if _, err := PrepareSubagentStart(ctx, journal, StartSubagentOptions{
		ParentRunID: parent, ChildRunID: child, Task: "other",
		WorkspaceHash: "ws", ParentTools: []string{"read_file"}, ChildTools: []string{"read_file"},
	}); !errors.Is(err, ErrSubagentStart) {
		t.Fatalf("任务不符应拒绝: %v", err)
	}
}

// TestBuildStartEvidence 锁定启动证据字段完整。
func TestBuildStartEvidence(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	evidence := BuildStartEvidence("run-p", "run-c", []string{"read_file"}, at)
	if evidence.ChildRunID != "run-c" || evidence.ParentRunID != "run-p" || len(evidence.NarrowedTools) != 1 {
		t.Fatalf("证据不符: %+v", evidence)
	}
	if !evidence.StartedAt.Equal(at) {
		t.Fatalf("时间不符: %+v", evidence)
	}
}
