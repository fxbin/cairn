import type { DomainEvent } from "../desktop/types";

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

export function toTimeline(events: DomainEvent[]): TimelineItem[] {
  return events.map((event) => ({
    id: event.event_id || `${event.run_id}-${event.seq}`,
    seq: event.seq,
    type: event.type,
    label: typeLabels[event.type] || event.type,
    detail: payloadText(event.payload),
    tone: toneOf(event.type),
  }));
}
