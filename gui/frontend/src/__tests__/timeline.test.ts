import { describe, expect, it } from "vitest";
import { toTimeline } from "../desktop/timeline";
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
