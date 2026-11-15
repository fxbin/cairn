/**
 * 消息流视图模型：由 Journal 事件与流通知投影为对话时间线。
 * 参照 Codex/MiMo Desktop：用户气泡 + Agent 正文 + 工具卡片 + 审批卡片。
 * Author: fxbin
 */
import type { DomainEvent, StreamEvent } from "../desktop/types";

/** 对话流中的结构化消息块，承载用户指令、助手正文、工具回执、审批门禁与状态标记。 */
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
      targetPath?: string;
      isReadOnly?: boolean;
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

const READ_ONLY_TOOL_NAMES = new Set([
  "read_file",
  "list_dir",
  "list_files",
  "glob",
  "grep",
  "grep_search",
  "search",
  "search_files",
  "find_files",
  "skill",
]);

const WRITE_PATCH_TOOL_NAMES = new Set([
  "apply_patch",
  "write_file",
  "create_file",
  "edit_file",
  "delete_file",
]);

/** 判断工具名称是否属于纯只读探索类工具，用于连续探索批次折叠展示。 */
export function isReadOnlyTool(name: string): boolean {
  const normalized = name.trim().toLowerCase();
  return READ_ONLY_TOOL_NAMES.has(normalized);
}

/** 判断工具名称是否属于文件写入或确定性补丁工具，用于结构化补丁卡渲染。 */
export function isWritePatchTool(name: string): boolean {
  const normalized = name.trim().toLowerCase();
  return WRITE_PATCH_TOOL_NAMES.has(normalized);
}

/** 从事件载荷对象按候选键顺序提取首个非空字符串表示。 */
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

/** 从工具调用载荷中提取目标文件相对路径，支持直接路径字段与单文件补丁头部解析。 */
export function extractToolTargetPath(payload: unknown): string {
  const direct = payloadString(payload, [
    "path",
    "relative",
    "file",
    "file_path",
    "target_file",
  ]);
  if (direct && !direct.includes("\n") && direct.length <= 512) {
    return direct.replace(/^\.\//, "");
  }
  const patchText = payloadString(payload, ["patch", "diff"]);
  if (!patchText) {
    return "";
  }
  const applyHeader = /^\*\*\*\s+(?:Update|Add|Delete)\s+File:\s*([^\r\n]+)$/m.exec(patchText);
  if (applyHeader?.[1]?.trim()) {
    return applyHeader[1].trim().replace(/^\.\//, "");
  }
  const gitHeader = /^\+\+\+\s+b\/([^\r\n]+)$/m.exec(patchText);
  if (gitHeader?.[1]?.trim() && gitHeader[1].trim() !== "/dev/null") {
    return gitHeader[1].trim().replace(/^\.\//, "");
  }
  return "";
}

/** 提取领域事件对应的工具展示名，缺失时回退为事件类型。 */
function toolNameOf(event: DomainEvent): string {
  return (
    payloadString(event.payload, ["tool", "tool_name", "name", "command", "path"]) || event.type
  );
}

/** 将领域事件流与实时流通知按序号折叠为可展示对话块序列。 */
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
        const explicitTool = payloadString(event.payload, ["tool", "tool_name", "name"]) || name;
        const summary = payloadString(event.payload, ["summary", "command", "argv", "path", "description"]);
        const detail = JSON.stringify(event.payload ?? {}, null, 0).slice(0, 2000);
        const targetPath = extractToolTargetPath(event.payload);
        const readOnlyFlag = event.type === "tool_call_proposed" && isReadOnlyTool(explicitTool);
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
          ...(targetPath ? { targetPath } : {}),
          ...(readOnlyFlag ? { isReadOnly: true } : {}),
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
        const targetPath = extractToolTargetPath(event.payload);
        if (idx !== undefined && blocks[idx]?.kind === "tool") {
          const existing = blocks[idx] as Extract<ChatBlock, { kind: "tool" }>;
          existing.status = status;
          if (!existing.targetPath && targetPath) {
            existing.targetPath = targetPath;
          }
        } else {
          const name = toolNameOf(event);
          const explicitTool = payloadString(event.payload, ["tool", "tool_name", "name"]) || name;
          blocks.push({
            kind: "tool",
            id: `t-${id}`,
            name,
            summary: payloadString(event.payload, ["summary", "error", "result"]),
            detail: "",
            status,
            at,
            ...(targetPath ? { targetPath } : {}),
            ...(isReadOnlyTool(explicitTool) ? { isReadOnly: true } : {}),
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

/** 将可能为空的字符串引用归一化为空串。 */
function asStr(value: string | undefined | null): string {
  return value ?? "";
}
