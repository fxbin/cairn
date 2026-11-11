import { describe, expect, it } from "vitest";
import { toTimeline, toSubagentViews, toContextCompactions, toContextUsage, filterTimelineByKind } from "../desktop/timeline";
import type { DomainEvent } from "../desktop/types";

function event(partial: Partial<DomainEvent>): DomainEvent {
  return {
    schema_version: 1,
    event_id: "e1",
    run_id: "run-1",
    workspace_hash: "w",
    seq: 1,
    occurred_at: new Date().toISOString(),
    type: "run_started",
    payload: {},
    prev_hash: "",
    event_hash: "",
    ...partial,
  };
}

describe("timeline projection", () => {
  it("labels events and extracts payload detail", () => {
    const items = toTimeline([
      event({ type: "assistant_message_appended", payload: { text: "你好" }, seq: 2 }),
      event({ type: "operation_failed", payload: { error: "boom" }, seq: 3, event_id: "e2" }),
    ]);
    expect(items[0].label).toBe("助手消息");
    expect(items[0].detail).toContain("你好");
    expect(items[1].tone).toBe("danger");
  });
});

describe("subagent projection", () => {
  it("projects delegated and completed child runs", () => {
    const views = toSubagentViews([
      event({ type: "subagent_delegated", payload: { child_run_id: "child-1", task_digest: "摘要甲" }, seq: 2, event_id: "e2" }),
      event({ type: "subagent_completed", payload: { child_run_id: "child-1", outcome: "ok" }, seq: 3, event_id: "e3" }),
      event({ type: "subagent_completed", payload: { child_run_id: "child-2", outcome: "failed" }, seq: 4, event_id: "e4" }),
    ]);
    expect(views).toHaveLength(2);
    expect(views[0].childRunId).toBe("child-1");
    expect(views[0].state).toBe("completed");
    expect(views[0].outcome).toBe("ok");
    expect(views[0].task).toBe("摘要甲");
    expect(views[1].childRunId).toBe("child-2");
    expect(views[1].state).toBe("completed");
    expect(views[1].outcome).toBe("failed");
  });

  it("ignores events without child_run_id and duplicate delegates", () => {
    const views = toSubagentViews([
      event({ type: "subagent_delegated", payload: {}, seq: 1 }),
      event({ type: "subagent_delegated", payload: { child_run_id: "c", task_digest: "t" }, seq: 2, event_id: "e2" }),
      event({ type: "subagent_delegated", payload: { child_run_id: "c", task_digest: "t2" }, seq: 3, event_id: "e3" }),
    ]);
    expect(views).toHaveLength(1);
    expect(views[0].task).toBe("t");
  });
});

describe("context usage projection", () => {
  it("projects compaction reason and usage detail", () => {
    const events = [
      event({
        type: "context_compacted",
        seq: 5,
        event_id: "e5",
        payload: {
          through_seq: 4,
          summary: "历史已压缩为检查点",
          estimated_units_before: 12000,
          estimated_units_after: 3200,
          strategy: "deterministic_journal_checkpoint_v4",
          trigger: "prompt_estimate",
          units_source: "estimate_bytes_div2",
          last_provider_prompt_tokens: 9800,
          compacted_turn_ids: ["t1", "t2"],
          retained_turn_ids: [],
        },
      }),
    ];
    const usage = toContextUsage(events);
    expect(usage.compactionCount).toBe(1);
    expect(usage.estimatedUnitsBefore).toBe(12000);
    expect(usage.estimatedUnitsAfter).toBe(3200);
    expect(usage.lastTrigger).toBe("prompt_estimate");
    const items = toContextCompactions(events);
    expect(items[0].compactedTurns).toBe(2);
    expect(items[0].summaryPreview).toContain("检查点");
  });

  it("returns empty usage without compaction events", () => {
    const usage = toContextUsage([event({ type: "run_started" })]);
    expect(usage.compactionCount).toBe(0);
    expect(usage.lastTrigger).toBe("");
  });
});

describe("context usage projection", () => {
  it("projects compaction triggers and usage details", () => {
    const events = [
      event({
        type: "context_compacted",
        seq: 5,
        event_id: "e5",
        payload: {
          trigger: "prompt_estimate",
          estimated_units_before: 1200,
          estimated_units_after: 400,
          strategy: "deterministic_journal_checkpoint_v4",
          last_provider_prompt_tokens: 1180,
          summary: "摘要内容",
          compacted_turn_ids: ["t1", "t2"],
        },
      }),
    ];
    const compactions = toContextCompactions(events);
    expect(compactions).toHaveLength(1);
    expect(compactions[0].trigger).toBe("prompt_estimate");
    expect(compactions[0].unitsBefore).toBe(1200);
    expect(compactions[0].unitsAfter).toBe(400);
    expect(compactions[0].compactedTurns).toBe(2);
    expect(compactions[0].summaryPreview).toContain("摘要内容");

    const usage = toContextUsage(events);
    expect(usage.compactionCount).toBe(1);
    expect(usage.lastTrigger).toBe("prompt_estimate");
    expect(usage.lastProviderPromptTokens).toBe(1180);
    expect(usage.estimatedUnitsBefore).toBe(1200);
    expect(usage.estimatedUnitsAfter).toBe(400);
  });

  it("returns empty usage without compaction events", () => {
    const usage = toContextUsage([event({ type: "run_started", seq: 1 })]);
    expect(usage.compactionCount).toBe(0);
    expect(usage.lastTrigger).toBe("");
    expect(usage.estimatedUnitsBefore).toBe(0);
  });
});


describe("subagent usage projection", () => {
  it("汇总 completed usage 到子运行卡片", () => {
    const views = toSubagentViews([
      event({ type: "subagent_delegated", payload: { child_run_id: "c1", task_digest: "任务" }, seq: 1, event_id: "e1" }),
      event({
        type: "subagent_completed",
        payload: {
          child_run_id: "c1",
          outcome: "ok",
          usage: { tool_calls: 3, prompt_tokens: 120, completion_tokens: 40 },
        },
        seq: 2,
        event_id: "e2",
      }),
    ]);
    expect(views).toHaveLength(1);
    expect(views[0].toolCalls).toBe(3);
    expect(views[0].promptTokens).toBe(120);
    expect(views[0].completionTokens).toBe(40);
  });

  it("usage 缺失时保持 0，不填假值", () => {
    const views = toSubagentViews([
      event({ type: "subagent_completed", payload: { child_run_id: "c2", outcome: "failed" }, seq: 1, event_id: "e1" }),
    ]);
    expect(views[0].toolCalls).toBe(0);
    expect(views[0].promptTokens).toBe(0);
    expect(views[0].completionTokens).toBe(0);
    expect(views[0].outcome).toBe("failed");
  });

  it("非法 usage 数值忽略为 0", () => {
    const views = toSubagentViews([
      event({
        type: "subagent_completed",
        payload: {
          child_run_id: "c3",
          outcome: "ok",
          usage: { tool_calls: -1, prompt_tokens: "x", completion_tokens: 5 },
        },
        seq: 1,
        event_id: "e1",
      }),
    ]);
    expect(views[0].toolCalls).toBe(0);
    expect(views[0].promptTokens).toBe(0);
    expect(views[0].completionTokens).toBe(5);
  });
});


describe("compaction summary full view", () => {
  it("保留摘要全文与 preview", () => {
    const long = "检查点摘要".repeat(30);
    const views = toContextCompactions([
      event({
        type: "context_compacted",
        seq: 3,
        event_id: "e3",
        payload: {
          trigger: "prompt_estimate",
          estimated_units_before: 100,
          estimated_units_after: 40,
          strategy: "deterministic_journal_checkpoint_v4",
          summary: long,
          compacted_turn_ids: ["t1"],
        },
      }),
    ]);
    expect(views[0].summaryFull).toBe(long);
    expect(views[0].summaryPreview.length).toBeLessThan(long.length);
    expect(views[0].summaryPreview.endsWith("…")).toBe(true);
  });

  it("短摘要 preview 与全文一致", () => {
    const views = toContextCompactions([
      event({
        type: "context_compacted",
        seq: 1,
        event_id: "e1",
        payload: {
          trigger: "prompt_estimate",
          estimated_units_before: 10,
          estimated_units_after: 5,
          strategy: "deterministic_journal_checkpoint_v4",
          summary: "短摘要",
          compacted_turn_ids: [],
        },
      }),
    ]);
    expect(views[0].summaryPreview).toBe("短摘要");
    expect(views[0].summaryFull).toBe("短摘要");
  });
});

describe("compaction timeline preview", () => {
  it("context_compacted 时间线 detail 含摘要预览与触发", () => {
    const items = toTimeline([
      event({
        type: "context_compacted",
        seq: 6,
        event_id: "e6",
        payload: {
          summary: "历史已压缩",
          trigger: "prompt_estimate",
          estimated_units_before: 100,
          estimated_units_after: 40,
        },
      }),
    ]);
    expect(items[0].detail).toContain("历史已压缩");
    expect(items[0].detail).toContain("prompt_estimate");
    expect(items[0].detail).toContain("100→40");
    expect(items[0].label).toBe("上下文压缩");
  });

  it("长摘要在时间线截断为预览", () => {
    const long = "检查点".repeat(40);
    const items = toTimeline([
      event({ type: "context_compacted", seq: 1, event_id: "e1", payload: { summary: long } }),
    ]);
    expect(items[0].detail.length).toBeLessThan(long.length);
    expect(items[0].detail).toContain("…");
  });
});

describe("timeline kind filter", () => {
  it("按子运行/压缩/工具筛选事件", () => {
    const items = toTimeline([
      event({ type: "subagent_delegated", payload: { child_run_id: "c1" }, seq: 1, event_id: "e1" }),
      event({ type: "context_compacted", payload: { summary: "s", trigger: "prompt_estimate" }, seq: 2, event_id: "e2" }),
      event({ type: "tool_call_proposed", payload: { tool: "read_file" }, seq: 3, event_id: "e3" }),
      event({ type: "run_started", payload: {}, seq: 4, event_id: "e4" }),
    ]);
    expect(filterTimelineByKind(items, "all")).toHaveLength(4);
    expect(filterTimelineByKind(items, "subagent")).toHaveLength(1);
    expect(filterTimelineByKind(items, "compaction")).toHaveLength(1);
    expect(filterTimelineByKind(items, "tool")).toHaveLength(1);
    expect(filterTimelineByKind(items, "subagent")[0].type).toBe("subagent_delegated");
    expect(filterTimelineByKind(items, "tool")[0].type).toBe("tool_call_proposed");
  });
});
