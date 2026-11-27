// command_task_context_test.go 验证 task context 的交接摘要、NDJSON 输出与未找到的退出码。
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// TestTaskContextCommandOutputsHandoffAndNDJSON 锁定 task context 命令：默认输出交接
// 摘要，--json 只输出一行可按合同校验的 NDJSON；未知任务返回 4 且不写 stdout。
func TestTaskContextCommandOutputsHandoffAndNDJSON(t *testing.T) {
	ctx := context.Background()
	workspace, stateDir, provider := workflowCommandFixture(t)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	command := Command{
		Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }},
		Stdout:  stdout, Stderr: stderr,
	}
	configuration, err := command.loadConfig(workspace, "", config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	// 只带文字验收条件的任务没有结构化验收合同，交接摘要因此出现显式的 unknown 标记。
	current, err := command.Service.CreateTask(ctx, app.TaskScope{Workspace: workspace, StateDir: stateDir, Config: configuration},
		app.TaskDefinition{Goal: "hand off this task", Acceptance: []string{"the answer exists"}})
	if err != nil {
		t.Fatal(err)
	}
	id := string(current.ID)

	if code := command.Run(ctx, []string{"task", "context", id, "--workspace", workspace, "--state-dir", stateDir}); code != ExitOK {
		t.Fatalf("handoff exit = %d stderr = %s", code, stderr.String())
	}
	handoff := stdout.String()
	for _, required := range []string{
		"task " + id + "\n", "goal hand off this task\n",
		"acceptance unknown - no_structured_acceptance\n", "permissions unknown - not_bound_to_workflow\n",
		"unresolved side effects started=0 settled=0", contract.TaskHandoffDisclaimer + "\n",
	} {
		if !strings.Contains(handoff, required) {
			t.Fatalf("handoff missing %q:\n%s", required, handoff)
		}
	}

	stdout.Reset()
	if code := command.Run(ctx, []string{"task", "context", id, "--workspace", workspace, "--state-dir", stateDir, "--json"}); code != ExitOK {
		t.Fatalf("json exit = %d stderr = %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("json output must be a single NDJSON line, got %q", stdout.String())
	}
	var context contract.TaskContextV1
	if err := json.Unmarshal([]byte(lines[0]), &context); err != nil {
		t.Fatalf("decode task context: %v", err)
	}
	if err := context.Validate(); err != nil {
		t.Fatalf("task context is not self-consistent: %v", err)
	}
	if context.TaskID != id || context.Goal.Status != contract.TaskFactKnown ||
		context.FrozenAcceptance.Status != contract.TaskFactUnknown || context.FrozenAcceptance.Reason != contract.TaskReasonNoStructuredAcceptance {
		t.Fatalf("task context = %+v", context)
	}

	stdout.Reset()
	stderr.Reset()
	if code := command.Run(ctx, []string{"task", "context", "missing-task", "--workspace", workspace, "--state-dir", stateDir}); code != ExitFailed {
		t.Fatalf("unknown task exit = %d, want %d", code, ExitFailed)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "task not found") {
		t.Fatalf("unknown task wrote stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}
