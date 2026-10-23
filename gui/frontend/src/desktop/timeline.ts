/** 投影输入最小事件形状：完整 DomainEvent 与流式轻量事件均可传入。 */
export interface ProjectionEvent {
  event_id?: string;
  run_id: string;
  seq: number;
  type: string;
  payload: unknown;
}

/** 将领域事件投影为可读时间线条目。 */
export interface TimelineItem {
  id: string;
  seq: number;
  type: string;
  label: string;
  detail: string;
  tone: "neutral" | "ok" | "warn" | "danger" | "accent";
}

const typeLabels: Record<string, string> = {
  run_created: "创建运行",
  run_started: "开始执行",
  run_resumed: "恢复执行",
  run_cancel_requested: "请求取消",
  run_completed: "运行完成",
  run_failed: "运行失败",
  run_cancelled: "运行已取消",
  turn_opened: "打开回合",
  turn_closed: "关闭回合",
  model_request_started: "请求模型",
  model_response_finished: "模型响应",
  model_request_failed: "模型请求失败",
  tool_call_proposed: "工具提议",
  verification_command_proposed: "验证命令提议",
  policy_decided: "策略决定",
  approval_requested: "请求审批",
  approval_granted: "已批准",
  approval_denied: "已拒绝",
  operation_started: "操作开始",
  operation_succeeded: "操作成功",
  operation_failed: "操作失败",
  operation_cancelled: "操作取消",
  operation_reconciled: "操作对账",
  tool_result_appended: "工具结果",
  assistant_message_appended: "助手消息",
  verification_feedback_appended: "验证反馈",
  context_compacted: "上下文压缩",
  verification_started: "验证开始",
  verification_finished: "验证结束",
  steering_input_submitted: "补充指导",
  steering_input_consumed: "已消费指导",
  queue_input_submitted: "入队",
  queue_input_consumed: "出队执行",
  queue_input_cancelled: "取消入队",
  subagent_delegated: "子运行委派",
  subagent_completed: "子运行完成",
};

function toneOf(type: string): TimelineItem["tone"] {
  if (type.includes("failed") || type.includes("denied") || type === "policy_decided") {
    return "danger";
  }
  if (type.includes("succeeded") || type.includes("completed") || type.includes("granted")) {
    return "ok";
  }
  if (type.includes("approval") || type.includes("cancel")) {
    return "warn";
  }
  if (type.includes("assistant") || type.includes("model_response")) {
    return "accent";
  }
  return "neutral";
}

/** 压缩事件时间线摘要预览：截断 summary 并带触发/单位，不展开全文。 */
function compactionTimelineDetail(payload: unknown): string {
  if (!payload || typeof payload !== "object") return "";
  const record = payload as Record<string, unknown>;
  const str = (v: unknown) => (typeof v === "string" ? v : "");
  const summary = str(record.summary);
  const preview = summary.length > 60 ? `${summary.slice(0, 60)}…` : summary;
  const parts: string[] = [];
  if (preview) parts.push(preview);
  const trigger = str(record.trigger) || "unknown";
  parts.push(`触发 ${trigger}`);
  const strategy = str(record.strategy) || "unknown";
  parts.push(`策略 ${strategy}`);
  const before = typeof record.estimated_units_before === "number" && Number.isFinite(record.estimated_units_before) ? record.estimated_units_before : null;
  const after = typeof record.estimated_units_after === "number" && Number.isFinite(record.estimated_units_after) ? record.estimated_units_after : null;
  if (before !== null || after !== null) {
    parts.push(`units ${before ?? "—"}→${after ?? "—"}`);
  }
  return parts.join(" · ");
}

function payloadText(payload: unknown): string {
  if (!payload || typeof payload !== "object") {
    return "";
  }
  const record = payload as Record<string, unknown>;
  const preferred = ["summary", "message", "text", "reason", "tool", "command", "path", "error", "note"];
  const parts: string[] = [];
  for (const key of preferred) {
    const value = record[key];
    if (typeof value === "string" && value.trim()) {
      parts.push(`${key}: ${value.trim()}`);
    }
  }
  return parts.join(" · ");
}

export function toTimeline(events: ProjectionEvent[]): TimelineItem[] {
  return events.map((event) => ({
    id: event.event_id || `${event.run_id}-${event.seq}`,
    seq: event.seq,
    type: event.type,
    label: typeLabels[event.type] || event.type,
    detail: event.type === "context_compacted" ? compactionTimelineDetail(event.payload) : payloadText(event.payload),
    tone: toneOf(event.type),
  }));
}


/** 子运行只读展示快照；与 TUI SubagentView 同源事件投影对齐。 */
export interface SubagentView {
  childRunId: string;
  parentRunId: string;
  task: string;
  state: string;
  outcome: string;
  toolCalls: number;
  promptTokens: number;
  completionTokens: number;
  /** completed 载荷是否携带 usage；缺失时 UI 显示未回报而非假 0。 */
  usageReported: boolean;
}

function payloadRecord(payload: unknown): Record<string, unknown> {
  return payload && typeof payload === "object" ? (payload as Record<string, unknown>) : {};
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function asInt(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) && value >= 0 ? Math.trunc(value) : 0;
}

/** 从 completed 载荷的 usage 汇总用量；字段缺失或非法时为 0，不填假值。 */
function usageOf(payload: Record<string, unknown>): { toolCalls: number; promptTokens: number; completionTokens: number; usageReported: boolean } {
  const raw = payload.usage;
  const reported = Boolean(raw) && typeof raw === "object";
  const usage = payloadRecord(raw);
  return {
    toolCalls: asInt(usage.tool_calls),
    promptTokens: asInt(usage.prompt_tokens),
    completionTokens: asInt(usage.completion_tokens),
    usageReported: reported,
  };
}

/** 从事件流投影子运行列表：委派建行，完成就地更新。 */
export function toSubagentViews(events: ProjectionEvent[]): SubagentView[] {
  const views: SubagentView[] = [];
  const byChild = new Map<string, SubagentView>();
  for (const event of events) {
    const payload = payloadRecord(event.payload);
    const childRunId = asString(payload.child_run_id);
    if (event.type === "subagent_delegated") {
      if (!childRunId) continue;
      if (byChild.has(childRunId)) continue;
      const view: SubagentView = {
        childRunId,
        parentRunId: event.run_id,
        task: asString(payload.task_digest),
        state: "delegated",
        outcome: "",
        toolCalls: 0,
        promptTokens: 0,
        completionTokens: 0,
        usageReported: false,
      };
      byChild.set(childRunId, view);
      views.push(view);
      continue;
    }
    if (event.type === "subagent_completed") {
      if (!childRunId) continue;
      const existing = byChild.get(childRunId);
      const usage = usageOf(payload);
      if (existing) {
        existing.state = "completed";
        existing.outcome = asString(payload.outcome);
        existing.toolCalls = usage.toolCalls;
        existing.promptTokens = usage.promptTokens;
        existing.completionTokens = usage.completionTokens;
        existing.usageReported = usage.usageReported;
        continue;
      }
      const view: SubagentView = {
        childRunId,
        parentRunId: event.run_id,
        task: "",
        state: "completed",
        outcome: asString(payload.outcome),
        toolCalls: usage.toolCalls,
        promptTokens: usage.promptTokens,
        completionTokens: usage.completionTokens,
        usageReported: usage.usageReported,
      };
      byChild.set(childRunId, view);
      views.push(view);
    }
  }
  return views;
}


/** 压缩检查点展示快照；只读投影，不改 Journal 语义。 */
export interface ContextCompactionView {
  seq: number;
  trigger: string;
  unitsBefore: number | null;
  unitsAfter: number | null;
  strategy: string;
  lastProviderPromptTokens: number;
  compactedTurns: number;
  summaryPreview: string;
  summaryFull: string;
}

function asNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

/** 从事件流投影压缩检查点列表，按事件序追加。 */
export function toContextCompactions(events: ProjectionEvent[]): ContextCompactionView[] {
  const views: ContextCompactionView[] = [];
  for (const event of events) {
    if (event.type !== "context_compacted") {
      continue;
    }
    const payload = payloadRecord(event.payload);
    const summary = asString(payload.summary);
    const compacted = Array.isArray(payload.compacted_turn_ids)
      ? (payload.compacted_turn_ids as unknown[]).filter((item) => typeof item === "string")
      : [];
    views.push({
      seq: event.seq,
      trigger: asString(payload.trigger) || "unknown",
      unitsBefore: typeof payload.estimated_units_before === "number" && Number.isFinite(payload.estimated_units_before) ? payload.estimated_units_before : null,
      unitsAfter: typeof payload.estimated_units_after === "number" && Number.isFinite(payload.estimated_units_after) ? payload.estimated_units_after : null,
      strategy: asString(payload.strategy) || "unknown",
      lastProviderPromptTokens: asNumber(payload.last_provider_prompt_tokens),
      compactedTurns: compacted.length,
      summaryPreview: summary.length > 80 ? `${summary.slice(0, 80)}…` : summary,
      summaryFull: summary,
    });
  }
  return views;
}

/** 上下文用量明细：最近估算单位与压缩次数，未知不填假百分比。 */
export interface ContextUsageView {
  compactionCount: number;
  estimatedUnitsBefore: number;
  estimatedUnitsAfter: number;
  lastTrigger: string;
  lastProviderPromptTokens: number;
}

export function toContextUsage(events: ProjectionEvent[]): ContextUsageView {
  const compactions = toContextCompactions(events);
  const last = compactions[compactions.length - 1];
  return {
    compactionCount: compactions.length,
    estimatedUnitsBefore: last ? (last.unitsBefore ?? 0) : 0,
    estimatedUnitsAfter: last ? (last.unitsAfter ?? 0) : 0,
    lastTrigger: last ? last.trigger : "",
    lastProviderPromptTokens: last ? last.lastProviderPromptTokens : 0,
  };
}


/** 时间线筛选类别：全部 / 子运行 / 压缩 / 工具。 */
export type TimelineKind = "all" | "subagent" | "compaction" | "tool";

/** 按类别筛选时间线条目；all 原样返回。 */
export function filterTimelineByKind(items: TimelineItem[], kind: TimelineKind): TimelineItem[] {
  if (kind === "all") return items;
  return items.filter((item) => {
    if (kind === "subagent") return item.type.startsWith("subagent_");
    if (kind === "compaction") return item.type === "context_compacted";
    if (kind === "tool") return item.type.includes("tool") || item.type.includes("operation_");
    return true;
  });
}
