/**
 * 消息流视图模型：由 Journal 事件与流通知投影为对话时间线。
 * 参照 Codex/MiMo Desktop：用户气泡 + Agent 正文 + 工具卡片 + 知情审批卡片 + 实时生命体征。
 * Author: fxbin
 */
import type { DomainEvent, StreamEvent } from "../desktop/types";

/** 审批卡片内联补丁差异预览行。 */
export interface ApprovalPatchPreviewLine {
  kind: "add" | "del" | "meta";
  text: string;
}

/** 绑定至审批卡片的关联提议载荷预览（只读投影自 Journal 事件）。 */
export interface ApprovalProposalPreview {
  toolName: string;
  targetPath?: string;
  mode?: string;
  expectedSha?: string;
  commandArgv?: string;
  patchLines?: ApprovalPatchPreviewLine[];
}

/** 运行中会话顶部/底部脉冲条展示的确定性实时生命体征。 */
export interface LiveRunVital {
  turn: number;
  toolCount: number;
  stageLabel: string;
  detail?: string;
}

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
      proposal?: ApprovalProposalPreview;
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

const MAX_APPROVAL_PATCH_LINES = 14;

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

/** 将未知载荷安全规整为普通键值记录。 */
function asPayloadRecord(value: unknown): Record<string, unknown> {
  if (typeof value === "string") {
    const trimmed = value.trim();
    if (trimmed.startsWith("{")) {
      try {
        const parsed: unknown = JSON.parse(trimmed);
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          return parsed as Record<string, unknown>;
        }
      } catch {
        return {};
      }
    }
    return {};
  }
  if (value && typeof value === "object" && !Array.isArray(value)) {
    return value as Record<string, unknown>;
  }
  return {};
}

/** 从事件载荷对象按候选键顺序提取首个非空字符串表示。 */
function payloadString(payload: unknown, keys: string[]): string {
  const record = asPayloadRecord(payload);
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

/** 从载荷及可选嵌套 arguments 对象提取合并参数视图。 */
function extractArgumentsRecord(payload: unknown): Record<string, unknown> {
  const top = asPayloadRecord(payload);
  const nested = asPayloadRecord(top.arguments);
  return { ...top, ...nested };
}

/** 提取字符串数组字段并拼接为可读命令行文本。 */
function extractArgvString(record: Record<string, unknown>): string {
  const rawArgv = record.argv;
  if (Array.isArray(rawArgv)) {
    const tokens = rawArgv
      .map((item) => (typeof item === "string" ? item : String(item ?? "")))
      .filter((item) => item.length > 0);
    if (tokens.length > 0) {
      return tokens.join(" ");
    }
  }
  return payloadString(record, ["command", "cmd"]);
}

/** 从工具调用载荷中提取目标文件相对路径，支持直接路径字段、嵌套 arguments 与单文件补丁头部解析。 */
export function extractToolTargetPath(payload: unknown): string {
  const merged = extractArgumentsRecord(payload);
  const direct = payloadString(merged, [
    "path",
    "relative",
    "file",
    "file_path",
    "target_file",
  ]);
  if (direct && !direct.includes("\n") && direct.length <= 512) {
    return direct.replace(/^\.\//, "");
  }
  const patchText = payloadString(merged, ["patch", "diff"]);
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
    payloadString(event.payload, ["tool_name", "tool", "name", "command", "path"]) || event.type
  );
}

/** 从参数对象中裁剪出最多 MAX_APPROVAL_PATCH_LINES 行的补丁预览序列。 */
function extractPatchPreviewLines(
  args: Record<string, unknown>,
  toolName: string,
): ApprovalPatchPreviewLine[] | undefined {
  const lines: ApprovalPatchPreviewLine[] = [];
  let totalCount = 0;

  const pushLimited = (kind: ApprovalPatchPreviewLine["kind"], text: string) => {
    totalCount += 1;
    if (lines.length < MAX_APPROVAL_PATCH_LINES) {
      lines.push({ kind, text });
    }
  };

  if (Array.isArray(args.replacements)) {
    for (const item of args.replacements) {
      const rep = asPayloadRecord(item);
      const oldText = typeof rep.old === "string" ? rep.old : "";
      const newText = typeof rep.new === "string" ? rep.new : "";
      if (oldText) {
        for (const row of oldText.split("\n")) {
          pushLimited("del", `- ${row}`);
        }
      }
      if (newText) {
        for (const row of newText.split("\n")) {
          pushLimited("add", `+ ${row}`);
        }
      }
    }
  } else {
    const patchText = payloadString(args, ["patch", "diff"]);
    if (patchText) {
      for (const row of patchText.split("\n")) {
        if (/^(diff --git |index |--- |\+\+\+ )/.test(row)) continue;
        if (row.startsWith("@@")) {
          pushLimited("meta", row);
        } else if (row.startsWith("+")) {
          pushLimited("add", row);
        } else if (row.startsWith("-")) {
          pushLimited("del", row);
        }
      }
    } else {
      const contentText = typeof args.content === "string" ? args.content : "";
      const mode = payloadString(args, ["mode"]);
      if (contentText && (mode === "create" || isWritePatchTool(toolName))) {
        const rows = contentText.replace(/\n$/, "").split("\n");
        for (const row of rows) {
          pushLimited("add", `+ ${row}`);
        }
      }
    }
  }

  if (lines.length === 0) {
    return undefined;
  }
  if (totalCount > MAX_APPROVAL_PATCH_LINES) {
    lines.push({
      kind: "meta",
      text: `… 共 ${totalCount} 行变更预览，完整内容以审批通过后的工作区与 Diff 视图为准`,
    });
  }
  return lines;
}

/** 从工具或验证命令提议事件提取用于知情审批卡内联展示的预览结构。 */
function extractProposalPreview(event: DomainEvent): ApprovalProposalPreview {
  const args = extractArgumentsRecord(event.payload);
  const rawToolName =
    payloadString(event.payload, ["tool_name", "tool", "name"]) ||
    (event.type === "verification_command_proposed" ? "verification_command" : toolNameOf(event));
  const targetPath = extractToolTargetPath(event.payload);
  const mode = payloadString(args, ["mode"]);
  const rawSha = payloadString(args, ["expected_sha256"]);
  const expectedSha = rawSha ? (rawSha.length > 12 ? `${rawSha.slice(0, 12)}…` : rawSha) : "";
  const commandArgv = extractArgvString(args);
  const patchLines = extractPatchPreviewLines(args, rawToolName);

  return {
    toolName: rawToolName,
    ...(targetPath ? { targetPath } : {}),
    ...(mode ? { mode } : {}),
    ...(expectedSha ? { expectedSha } : {}),
    ...(commandArgv ? { commandArgv } : {}),
    ...(patchLines ? { patchLines } : {}),
  };
}

/** 合并历史持久化事件与实时流事件并按 seq 稳定升序排序。 */
function mergeAndSortEvents(events: DomainEvent[], stream?: StreamEvent[]): DomainEvent[] {
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
  return all;
}

/** 将领域事件流与实时流通知按序号折叠为可展示对话块序列。 */
export function buildChatBlocks(
  events: DomainEvent[],
  stream?: StreamEvent[],
): ChatBlock[] {
  const all = mergeAndSortEvents(events, stream);

  const blocks: ChatBlock[] = [];
  const toolIndex = new Map<string, number>();
  const proposalByOp = new Map<string, ApprovalProposalPreview>();
  let lastProposal: ApprovalProposalPreview | undefined;

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
            label:
              event.type === "run_started"
                ? "开始执行"
                : event.type === "run_resumed"
                  ? "恢复执行"
                  : "创建运行",
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
        const explicitTool =
          payloadString(event.payload, ["tool_name", "tool", "name"]) || name;
        const args = extractArgumentsRecord(event.payload);
        const summary =
          payloadString(event.payload, ["summary", "command", "path", "description"]) ||
          payloadString(args, ["path", "command"]) ||
          extractArgvString(args);
        const detail = JSON.stringify(event.payload ?? {}, null, 0).slice(0, 2000);
        const targetPath = extractToolTargetPath(event.payload);
        const readOnlyFlag =
          event.type === "tool_call_proposed" && isReadOnlyTool(explicitTool);
        const id = asStr(event.operation_id) || event.event_id;
        const proposal = extractProposalPreview(event);
        if (event.operation_id) {
          proposalByOp.set(event.operation_id, proposal);
        }
        lastProposal = proposal;

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
          const explicitTool =
            payloadString(event.payload, ["tool_name", "tool", "name"]) || name;
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
        const operationId =
          asStr(event.operation_id) || payloadString(event.payload, ["operation_id"]);
        const matchedProposal =
          (operationId ? proposalByOp.get(operationId) : undefined) || lastProposal;
        if (fingerprint) {
          blocks.push({
            kind: "approval",
            id: `ap-${event.event_id}`,
            runId: event.run_id,
            fingerprint,
            operationId,
            summary: summary || "等待人工审批",
            ...(matchedProposal ? { proposal: matchedProposal } : {}),
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

/**
 * 从已持久化与实时流事件中确定性推导出当前运行的实时生命体征（轮次、工具次数与活跃阶段）。
 */
export function deriveLiveRunVital(
  events: DomainEvent[],
  stream?: StreamEvent[],
): LiveRunVital {
  const all = mergeAndSortEvents(events, stream);
  let turn = 0;
  let toolCount = 0;
  let stageLabel = "等待运行事件";
  let detail = "";
  let activeTool = "";

  for (const ev of all) {
    const record = asPayloadRecord(ev.payload);
    switch (ev.type) {
      case "run_created":
        stageLabel = "已提交，初始化运行上下文";
        detail = "";
        break;
      case "run_started":
      case "run_resumed":
        stageLabel = ev.type === "run_resumed" ? "恢复运行中" : "开始执行";
        detail = "";
        break;
      case "turn_opened": {
        const idx = typeof record.turn_index === "number" ? record.turn_index : turn + 1;
        if (idx > turn) turn = idx;
        stageLabel = `第 ${turn} 轮推演`;
        detail = "";
        break;
      }
      case "model_request_started":
        stageLabel = "模型思考与规划中";
        detail = "";
        break;
      case "model_response_finished":
        stageLabel = "处理模型输出";
        detail = payloadString(record, ["stop_reason"]);
        break;
      case "tool_call_proposed":
      case "verification_command_proposed":
        toolCount += 1;
        activeTool = payloadString(record, ["tool_name", "tool", "name"]) || "tool";
        stageLabel = "准备工具调用";
        detail = activeTool;
        break;
      case "operation_started":
        stageLabel = "工具执行中";
        detail =
          payloadString(record, ["tool_name", "tool", "name"]) || activeTool || "";
        break;
      case "operation_succeeded":
      case "operation_failed":
      case "operation_cancelled":
        stageLabel = "工具结果已持久化";
        detail = activeTool;
        activeTool = "";
        break;
      case "approval_requested":
        stageLabel = "等待人工审批";
        detail = payloadString(record, ["summary"]);
        break;
      case "approval_granted":
        stageLabel = "已授权，恢复运行中";
        detail = "";
        break;
      case "verification_started":
        stageLabel = "Verifier 独立验证中";
        detail = "";
        break;
      case "verification_finished":
        stageLabel = "Verifier 验证已记录";
        detail = "";
        break;
      case "context_compacted":
        stageLabel = "上下文检查点已压缩";
        detail = "";
        break;
      case "run_cancel_requested":
        stageLabel = "正在安全收尾取消";
        detail = "";
        break;
      case "run_completed":
      case "run_failed":
      case "run_cancelled":
        stageLabel = "运行已收敛";
        detail = "";
        break;
      default:
        break;
    }
  }

  return {
    turn,
    toolCount,
    stageLabel,
    ...(detail ? { detail } : {}),
  };
}

/** 将可能为空的字符串引用归一化为空串。 */
function asStr(value: string | undefined | null): string {
  return value ?? "";
}
