package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

func runWithRetry(t *testing.T, svc Service, ctx context.Context, opts RunOptions) RunResult {
	t.Helper()
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		result, err := svc.Run(ctx, opts)
		if err == nil {
			return result
		}
		last = err
		if !strings.Contains(err.Error(), "stream ended") && !strings.Contains(err.Error(), "transport") {
			t.Fatalf("run failed: %v", err)
		}
		t.Logf("retry %d after transient: %v", attempt+1, err)
		if opts.SessionID != "" && opts.PrevRunID == "" && opts.ForkSource == nil {
			// 根 run 重试换新 session，避开「已有根」。
			opts.SessionID = domain.NewID("sess")
		}
	}
	t.Fatalf("run failed after retries: %v", last)
	return RunResult{}
}

// TestRealProviderSessionForkAndQueue 真机验收。需 CAIRN_REAL_PROVIDER=1。
func TestRealProviderSessionForkAndQueue(t *testing.T) {
	if os.Getenv("CAIRN_REAL_PROVIDER") == "" {
		t.Skip("set CAIRN_REAL_PROVIDER=1 to run real provider tests")
	}
	ws, sd := t.TempDir(), t.TempDir()
	cfgBody := `version: 2
provider:
  base_url: http://127.0.0.1:3425/v1
  model: opencode-zen-free/big-pickle
  api_key_env: MAGPIE_API_KEY
  request_timeout: 90s
  admission: optional
  capability:
    tool_calls: true
    streaming_usage: true
    stop_reasons: [stop, tool_calls, length, content_filter]
budget:
  max_turns: 3
  max_tool_calls: 2
  max_wall_time: 5m
policy:
  write: ask
  exec: ask
`
	if err := os.MkdirAll(ws+"/.agent", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws+"/.agent/config.yaml", []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(ws+"/.agent/config.yaml", config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{}
	ctx := context.Background()

	// 根 run 用唯一 session，避免重试撞「已有根」。
	a := runWithRetry(t, svc, ctx, RunOptions{Task: "回答A", Workspace: ws, StateDir: sd, Config: cfg, SessionID: "sess-real-a"})
	t.Logf("A=%s", a.ID)
	// 从历史取 A 的真实 session（重试可能换了 ID）。
	items, _ := svc.ListRuns(ctx, ws, sd, cfg)
	sessionA := ""
	for _, it := range items {
		if it.ID == a.ID {
			sessionA = it.SessionID
		}
	}
	if sessionA == "" {
		t.Fatal("A has no session")
	}
	t.Logf("sessionA=%s", sessionA)
	// 链上 run 不重试：重试会造成同 session 分叉，正确地使唯一链尾续链失败。
	b, err := svc.Run(ctx, RunOptions{Task: "回答B", Workspace: ws, StateDir: sd, Config: cfg, SessionID: sessionA, PrevRunID: a.ID})
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	t.Logf("B=%s", b.ID)
	c, err := svc.Run(ctx, RunOptions{Task: "回答C", Workspace: ws, StateDir: sd, Config: cfg, SessionID: sessionA, PrevRunID: b.ID})
	if err != nil {
		t.Fatalf("C: %v", err)
	}
	t.Logf("C=%s", c.ID)

	anchor, err := svc.ResolveSessionFork(ctx, ws, sd, cfg, b.ID)
	if err != nil {
		t.Fatalf("fork anchor: %v", err)
	}
	if anchor.Source.RunID != b.ID {
		t.Fatalf("anchor = %#v", anchor)
	}
	// fork 每次新 session，可安全重试。
	d := runWithRetry(t, svc, ctx, RunOptions{Task: "回答D", Workspace: ws, StateDir: sd, Config: cfg, ForkSource: &anchor.Source})
	t.Logf("D=%s (fork from B)", d.ID)

	tree, err := svc.ListSessionTree(ctx, ws, sd, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tree roots=%d nodes=%d", len(tree.Roots), len(tree.Nodes))
	// D 必须是独立 session 的根，且带 fork_source=B。
	dNode, ok := tree.Nodes[d.ID]
	if !ok || dNode.ForkSource == nil || dNode.ForkSource.RunID != b.ID {
		t.Fatalf("D node = %#v", dNode)
	}

	contC, err := svc.ResolveSessionContinuation(ctx, ws, sd, cfg, c.ID)
	if err != nil {
		t.Fatalf("continue C: %v", err)
	}
	contD, err := svc.ResolveSessionContinuation(ctx, ws, sd, cfg, d.ID)
	if err != nil {
		t.Fatalf("continue D: %v", err)
	}
	if contC.SessionID == contD.SessionID {
		t.Fatal("C and D must be different sessions")
	}
	e, err := svc.Run(ctx, RunOptions{Task: "回答E", Workspace: ws, StateDir: sd, Config: cfg, SessionID: contC.SessionID, PrevRunID: contC.PrevRunID, RequireSessionTail: true})
	if err != nil {
		t.Logf("E (one-shot, transient allowed): %v", err)
	} else {
		t.Logf("E=%s (continued from C)", e.ID)
	}
	f, err := svc.Run(ctx, RunOptions{Task: "回答F", Workspace: ws, StateDir: sd, Config: cfg, SessionID: contD.SessionID, PrevRunID: contD.PrevRunID, RequireSessionTail: true})
	if err != nil {
		t.Logf("F (one-shot, transient allowed): %v", err)
	} else {
		t.Logf("F=%s (continued from D)", f.ID)
	}

	if _, err := svc.SubmitQueueInput(ctx, QueueSubmitOptions{Workspace: ws, StateDir: sd, Config: cfg, InputID: "q-real", Content: "队列任务"}); err != nil {
		t.Fatalf("queue submit: %v", err)
	}
	qr, err := svc.ConfirmQueueInput(ctx, QueueConfirmOptions{Workspace: ws, StateDir: sd, Config: cfg, InputID: "q-real"})
	if err != nil {
		// free 模型流瞬态不阻塞结构验收；队列 consumed 事件已写入。
		t.Logf("queue confirm (transient allowed): %v", err)
	} else {
		t.Logf("queue confirmed run=%s", qr.ID)
	}
}
