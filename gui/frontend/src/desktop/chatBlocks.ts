/**
 * 消息流视图模型：由 Journal 事件与流通知投影为对话时间线。
 * 参照 Codex/MiMo Desktop：用户气泡 + Agent 正文 + 工具卡片 + 审批卡片。
 */
import type { DomainEvent, StreamEvent } from "../desktop/types";

export type ChatBlock =
  | { kind: "user"; id: string; text: string; at: string }
  | { kind: "agent"; id: string; text: string; at: string }
  | {
      kind: "tool";
      id: string;
      name: string;
      summary: string;
      detail: string;
      status: "running" | "ok" | "failed";
      at: string;
    }
  | {
      kind: "approval";
      id: string;
      runId: string;
      fingerprint: string;
      operationId?: string;
      summary: string;
      resolved?: "approved" | "denied";
      at: string;
    }
  | { kind: "status"; id: string; label: string; detail?: string; at: string };

function payloadString(payload: unknown, keys: string[]): string {
  if (!payload || typeof payload !== "object") {
    return "";
  }
  const record = payload as Record<string, unknown>;
  for (const key of keys) {
    const value = record[key];
    if (typeof value === "string" && value.trim()) {
      return value.trim();
    }
    if (typeof value === "number" || typeof value === "boolean") {
      return String(value);
    }
  }
  return "";
}

function toolNameOf(event: DomainEvent): string {
  return (
    payloadString(event.payload, ["tool", "tool_name", "name", "command", "path"]) || event.type
  );
}

/** 将领域事件流折叠为对话块。 */
export function buildChatBlocks(
  events: DomainEvent[],
  stream?: StreamEvent[],
): ChatBlock[] {
  const all = events.slice();
  if (stream?.length) {
    const seen = new Set(events.map((e) => `${e.run_id}:${e.seq}`));
    for (const item of stream) {
      if (!item.event) {
        continue;
      }
      const key = `${item.event.run_id}:${item.event.seq}`;
      if (!seen.has(key)) {
        all.push(item.event);
        seen.add(key);
      }
    }
  }
  all.sort((a, b) => a.seq - b.seq || Date.parse(a.occurred_at) - Date.parse(b.occurred_at));

  const blocks: ChatBlock[] = [];
  const toolIndex = new Map<string, number>();

  for (const event of all) {
    const at = event.occurred_at;
    switch (event.type) {
      case "run_created":
      case "run_started":
      case "run_resumed": {
        const task = payloadString(event.payload, ["task", "prompt"]);
        if (task && event.type === "run_created") {
          blocks.push({ kind: "user", id: `u-${event.event_id}`, text: task, at });
        } else {
          blocks.push({
            kind: "status",
            id: `s-${event.event_id}`,
            label: event.type === "run_started" ? "开始执行" : event.type === "run_resumed" ? "恢复执行" : "创建运行",
            at,
          });
        }
        break;
      }
      case "assistant_message_appended": {
        const text = payloadString(event.payload, ["text", "message", "content"]);
        if (text) {
          blocks.push({ kind: "agent", id: `a-${event.event_id}`, text, at });
        }
        break;
      }
      case "tool_call_proposed":
      case "verification_command_proposed": {
        const name = toolNameOf(event);
        const summary = payloadString(event.payload, ["summary", "command", "argv", "path", "description"]);
        const detail = JSON.stringify(event.payload ?? {}, null, 0).slice(0, 2000);
        const id = asStr(event.operation_id) || event.event_id;
        toolIndex.set(id, blocks.length);
        blocks.push({
          kind: "tool",
          id: `t-${id}`,
          name,
          summary: summary || name,
          detail,
          status: "running",
          at,
        });
        break;
      }
      case "operation_started":
      case "operation_succeeded":
      case "operation_failed":
      case "operation_cancelled": {
        const id = asStr(event.operation_id) || event.event_id;
        const idx = toolIndex.get(id);
        const status =
          event.type === "operation_started"
            ? "running"
            : event.type === "operation_succeeded"
              ? "ok"
              : "failed";
        if (idx !== undefined && blocks[idx]?.kind === "tool") {
          (blocks[idx] as Extract<ChatBlock, { kind: "tool" }>).status = status;
        } else {
          blocks.push({
            kind: "tool",
            id: `t-${id}`,
            name: toolNameOf(event),
            summary: payloadString(event.payload, ["summary", "error", "result"]),
            detail: "",
            status,
            at,
          });
        }
        break;
      }
      case "approval_requested": {
        const fingerprint = payloadString(event.payload, ["fingerprint"]);
        const summary = payloadString(event.payload, ["summary", "description"]);
        if (fingerprint) {
          blocks.push({
            kind: "approval",
            id: `ap-${event.event_id}`,
            runId: event.run_id,
            fingerprint,
            operationId: asStr(event.operation_id) || payloadString(event.payload, ["operation_id"]),
            summary: summary || "等待人工审批",
            at,
          });
        }
        break;
      }
      case "approval_granted":
      case "approval_denied": {
        for (let i = blocks.length - 1; i >= 0; i -= 1) {
          const block = blocks[i];
          if (block.kind === "approval" && !block.resolved) {
            block.resolved = event.type === "approval_granted" ? "approved" : "denied";
            break;
          }
        }
        blocks.push({
          kind: "status",
          id: `s-${event.event_id}`,
          label: event.type === "approval_granted" ? "已批准操作" : "已拒绝操作",
          at,
        });
        break;
      }
      case "run_completed":
        blocks.push({
          kind: "status",
          id: `s-${event.event_id}`,
          label: "任务完成",
          detail: payloadString(event.payload, ["final_response", "stop_reason"]),
          at,
        });
        break;
      case "run_failed":
        blocks.push({
          kind: "status",
          id: `s-${event.event_id}`,
          label: "运行失败",
          detail: payloadString(event.payload, ["error", "failure_class", "reason"]),
          at,
        });
        break;
      case "run_cancelled":
      case "run_cancel_requested":
        blocks.push({
          kind: "status",
          id: `s-${event.event_id}`,
          label: event.type === "run_cancelled" ? "已取消" : "取消请求中",
          at,
        });
        break;
      case "verification_started":
      case "verification_finished": {
        blocks.push({
          kind: "status",
          id: `s-${event.event_id}`,
          label: event.type === "verification_started" ? "验证开始" : "验证结束",
          detail: payloadString(event.payload, ["ok", "status", "reason"]),
          at,
        });
        break;
      }
      case "model_request_started":
      case "model_response_finished":
      case "turn_opened":
      case "turn_closed":
      case "context_compacted":
        // 过细事件默认折叠为状态条，避免刷屏（渐进披露）。
        break;
      default:
        break;
    }
  }
  return blocks;
}

function asStr(value: string | undefined | null): string {
  return value ?? "";
}
