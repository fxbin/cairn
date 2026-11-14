package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/policy"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// projectedContextFixture 模拟只读状态投影，不依赖 Skill 或完整工具注册表。
type projectedContextFixture struct {
	blocks []string
	err    error
}

func (fixture *projectedContextFixture) ContextBlocks() ([]string, error) {
	return fixture.blocks, fixture.err
}

func TestContextProvidersOrderEmptyAndFrozenBudget(t *testing.T) {
	first := &projectedContextFixture{blocks: []string{"", " \n", "first"}}
	second := &projectedContextFixture{blocks: []string{"second"}}
	loop := Loop{Tools: &basicOnlyRegistry{}, ContextProviders: []ContextProvider{first, second}, ContextMaxPromptTokens: 2000, ContextMaxSummaryBytes: 256}
	if changed, err := loop.refreshContextProviders(); err != nil || !changed {
		t.Fatalf("first projection changed=%v err=%v", changed, err)
	}
	messages := loop.contextMessages("task", "prior", "summary")
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
		if message.Content == "first" || message.Content == "second" {
			if message.Role != "user" || len(message.ToolCalls) != 0 || message.ToolCallID != "" || len(message.ProtocolBlocks) != 0 {
				t.Fatal("context extension acquired protocol authority")
			}
		}
	}
	want := []string{loop.prompt(), sessionContextBlock("prior"), "first", "second", "task", contextCheckpointPrefix + "summary"}
	if !reflect.DeepEqual(contents, want) {
		t.Fatalf("unexpected order: %#v", contents)
	}
	first.blocks[2] = "changed outside safety point"
	if got := loop.contextMessages("task", "prior", "summary"); !reflect.DeepEqual(got, messages) {
		t.Fatal("budget/context snapshot changed outside the safety point")
	}
	size := loop.compactionSummaryLimit("task", "prior")
	if size <= 0 || loop.estimatePromptTokens(loop.contextMessages("task", "prior", strings.Repeat("x", size))) > loop.ContextMaxPromptTokens {
		t.Fatal("context extension missing from compaction budget")
	}
	second.err = errors.New("projection unavailable")
	if _, err := loop.refreshContextProviders(); !errors.Is(err, second.err) {
		t.Fatalf("error lost: %v", err)
	}
	if got := loop.contextMessages("task", "prior", "summary"); !reflect.DeepEqual(got, messages) {
		t.Fatal("failed provider partially replaced the frozen snapshot")
	}
	loop.ContextProviders = nil
	if changed, err := loop.refreshContextProviders(); err != nil || !changed {
		t.Fatalf("clear projection changed=%v err=%v", changed, err)
	}
	base := loop.contextMessages("task", "", "")
	loop.ContextProviders = []ContextProvider{&projectedContextFixture{blocks: []string{"", " \n"}}}
	if changed, err := loop.refreshContextProviders(); err != nil || changed || !reflect.DeepEqual(base, loop.contextMessages("task", "", "")) {
		t.Fatal("empty provider changed the no-extension behavior")
	}
}

// newContextProviderLoop 组装不具备高级工具方法的真实 Loop 执行夹具。
func newContextProviderLoop(t *testing.T, extension ContextProvider, provider *scriptedProvider) (*Loop, *memory.Journal, RunInput) {
	t.Helper()
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	input := RunInput{RunID: domain.NewRunID(), Task: "read input", WorkspaceHash: "workspace"}
	appendInitialRunningEvents(t, journal, input.RunID, input.Task)
	return &Loop{Journal: journal, Provider: provider, Tools: &basicOnlyRegistry{}, Gate: gate, Model: "test", MaxTurns: 3, ContextProviders: []ContextProvider{extension}}, journal, input
}

func TestContextProviderFailureAndOversizeStopBeforeModel(t *testing.T) {
	sentinel := errors.New("projection failed")
	for _, test := range []struct {
		name      string
		extension ContextProvider
		budget    int
		want      error
	}{
		{name: "failure", extension: &projectedContextFixture{err: sentinel}, want: sentinel},
		{name: "missing", extension: nil},
		{name: "oversize", extension: &projectedContextFixture{blocks: []string{strings.Repeat("完整正文", 3000)}}, budget: 3000},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "must not run"}, {Kind: ports.Done}}}}
			loop, journal, input := newContextProviderLoop(t, test.extension, provider)
			loop.ContextAutoCompact = true
			loop.ContextMaxPromptTokens = test.budget
			_, err := loop.Run(context.Background(), input)
			if err == nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("expected closed failure, got %v", err)
			}
			events, loadErr := journal.Load(context.Background(), input.RunID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if len(provider.requests) != 0 || countEvent(events, domain.EventModelRequestStarted) != 0 || countEvent(events, domain.EventContextCompacted) != 0 {
				t.Fatal("failed/oversized projection reached model or discarded persistent text")
			}
		})
	}
}

func TestContextProviderRefreshAfterPersistedToolResult(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "updated", true: "failure"}[fail], func(t *testing.T) {
			extension := &projectedContextFixture{}
			provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
				{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"input.txt"}`)}}, {Kind: ports.Done}},
				{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}},
			}}
			loop, journal, input := newContextProviderLoop(t, extension, provider)
			sentinel := errors.New("updated context unavailable")
			loop.OnEvent = func(event domain.Event) {
				if event.Type == domain.EventToolResultAppended {
					extension.blocks = []string{"persisted extension body"}
					if fail {
						extension.err = sentinel
					}
				}
			}
			_, err := loop.Run(context.Background(), input)
			if fail {
				if !errors.Is(err, sentinel) || len(provider.requests) != 1 {
					t.Fatalf("second request crossed projection failure: requests=%d err=%v", len(provider.requests), err)
				}
			} else {
				if err != nil || len(provider.requests) != 2 {
					t.Fatalf("requests=%d err=%v", len(provider.requests), err)
				}
				found := 0
				for _, message := range provider.requests[1].Messages {
					if message.Content == "persisted extension body" {
						found++
						if message.Role != "user" {
							t.Fatal("context trust level changed")
						}
					}
				}
				if found != 1 {
					t.Fatalf("next model request has %d copies of updated projection", found)
				}
			}
			events, err := journal.Load(context.Background(), input.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if countEvent(events, domain.EventOperationStarted) != 1 || countEvent(events, domain.EventToolResultAppended) != 1 {
				t.Fatal("extension replayed the tool operation")
			}
		})
	}
}
