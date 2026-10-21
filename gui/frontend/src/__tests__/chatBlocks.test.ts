import { describe, expect, it } from "vitest";
import { buildChatBlocks } from "../desktop/chatBlocks";
import type { DomainEvent } from "../desktop/types";

function ev(partial: Partial<DomainEvent>): DomainEvent {
  return {
    schema_version: 1,
    event_id: "e",
    run_id: "r1",
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

describe("buildChatBlocks", () => {
  it("folds events into conversation blocks", () => {
    const blocks = buildChatBlocks([
      ev({ type: "run_created", seq: 1, payload: { task: "修 bug" } }),
      ev({ type: "assistant_message_appended", seq: 2, payload: { text: "我来处理" } }),
      ev({ type: "tool_call_proposed", seq: 3, operation_id: "op1", payload: { tool: "read_file", summary: "a.go" } }),
      ev({ type: "operation_succeeded", seq: 4, operation_id: "op1" }),
      ev({ type: "approval_requested", seq: 5, payload: { fingerprint: "fp", summary: "写入 a.go" } }),
      ev({ type: "approval_granted", seq: 6 }),
      ev({ type: "run_completed", seq: 7 }),
    ]);
    const kinds = blocks.map((b) => b.kind);
    expect(kinds).toContain("user");
    expect(kinds).toContain("agent");
    expect(kinds).toContain("tool");
    expect(kinds).toContain("approval");
    const tool = blocks.find((b) => b.kind === "tool");
    expect(tool && tool.kind === "tool" && tool.status).toBe("ok");
    const ap = blocks.find((b) => b.kind === "approval");
    expect(ap && ap.kind === "approval" && ap.resolved).toBe("approved");
  });
});
