/**
 * [INPUT]: 用户原始输入、用户选择的编排上下文、工作区定义和 App 实例投影。
 * [OUTPUT]: 对话式工作流意图探测/协议展开、cairn-workflow 草案 JSON 解析与多节点草案校验与实例展示。
 * [POS]: studio/frontend/src/workbench 对话创建/调试/编辑工作流的核心协调模型，不改变后端授权与验证边界。
 *
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 * Author: fxbin
 */
import type { WorkflowDefinitionItem, WorkflowInstanceView, WorkflowNodeKind, WorkflowStepSpec } from "../desktop/types";

export type { WorkflowDefinitionItem, WorkflowStepSpec } from "../desktop/types";

export interface WorkflowBlueprintSpec {
  id?: string;
  name: string;
  steps: WorkflowStepSpec[];
}

export interface WorkflowComposeContext {
  mode: "create" | "edit";
  workflow?: WorkflowDefinitionItem;
  note?: string;
}

export type WorkflowStageRuntimeStatus = "done" | "running" | "waiting_approval" | "waiting_reconcile" | "failed" | "pending";

export interface WorkflowStageRuntime {
  index: number;
  label: string;
  runId?: string;
  status: WorkflowStageRuntimeStatus;
}

export interface ResolvedWorkflowExecution {
  instance: WorkflowInstanceView;
  workflowId: string;
  workflowName: string;
  latestRunId: string;
  status: string;
  completedCount: number;
  activeStepIndex: number;
  stages: WorkflowStageRuntime[];
}

export interface StarterWorkflowTemplate {
  id: string;
  title: string;
  badge: string;
  description: string;
  prompt: string;
}

const WORKFLOW_PROTOCOL_MARKER = "\n\n[工作流编排约定]";
const WORKFLOW_NODE_KINDS: readonly WorkflowNodeKind[] = ["execute", "script", "verify", "accept"];

/**
 * STARTER_WORKFLOW_PROMPTS 提供对话创建工作流的推荐起手模板。
 */
export const STARTER_WORKFLOW_PROMPTS: readonly StarterWorkflowTemplate[] = [
  {
    id: "code-review-pipeline",
    title: "代码变更深度审查流",
    badge: "3 阶段 · 只读优先",
    description: "只读盘点近期改动 → 排查边界风险与测试缺口 → 汇总可验证修改清单。",
    prompt:
      "帮我设计一个 3 阶段工作流：第 1 步先在只读探索模式下梳理当前 Git 变更与影响模块；第 2 步核查边界条件、权限与测试覆盖缺口；第 3 步汇总结构化的审查结论与改进建议。",
  },
  {
    id: "regression-triage-pipeline",
    title: "测试回归与排障流",
    badge: "3 阶段 · 闭环验证",
    description: "先运行既有测试锁定失败现场，再做最小化修复与复测验证。",
    prompt:
      "帮我创建一个调试排障工作流：第 1 步只读定位测试失败日志与根因；第 2 步针对根因做最小改动修复；第 3 步复测相关用例并收集验证证据。",
  },
  {
    id: "repo-health-digest",
    title: "架构健康度巡检流",
    badge: "2 阶段 · 零副作用",
    description: "全程只读巡检依赖边界、待提交工作区状态与潜在债务，输出巡检简报。",
    prompt:
      "帮我编排一个全程只读的架构健康度巡检工作流：第 1 步扫描核心模块边界、配置健康状态与待提交改动；第 2 步生成重点问题与行动建议简报。",
  },
];

/**
 * 探测用户自由输入是否显式请求通过对话设计、创建、调整或调试工作流。
 */
export function detectImplicitWorkflowIntent(rawTask: string): boolean {
  return /(?:设计|创建|新建|编写|生成|调整|修改|优化|调试).{0,14}工作流|SaveWorkflow|cairn-workflow/i.test(
    rawTask,
  );
}

/**
 * 在显式工作流上下文或明确编排意图下，为任务文本附加受控工作流协议后缀，指导 Agent 输出可保存草案。
 */
export function expandTaskWithWorkflowProtocol(
  rawTask: string,
  ctx: WorkflowComposeContext | null,
  workflows: readonly WorkflowDefinitionItem[],
): string {
  if (!ctx && !detectImplicitWorkflowIntent(rawTask)) {
    return rawTask;
  }

  const lines: string[] = [
    "[工作流编排约定]",
    "你正在协助用户通过对话设计、调试并迭代 Cairn 受控工作流（最多 8 个顺序阶段）。",
    "kind 可为 execute（Agent，须 task）、script（固定 argv 数组）、verify（运行宿主验证）或 accept（人工门）。",
    "可用 id 标识节点、inputs 引用此前产物、max_attempts 设 1 到 4 次尝试、retry_from 指定此前返工起点。",
    "安全边界：只读分析阶段建议设置 mode: \"read-only\"；涉及文件写入、命令运行或外部修改的阶段必须使用默认模式（执行时仍走完整审批与 Verifier 验证链）。",
    "当步骤设计清晰或根据用户反馈修改完成时，请在回复末尾附带一个单独的 ```cairn-workflow 代码块，格式如下：",
    "```cairn-workflow",
    "{",
    ctx?.mode === "edit" && ctx.workflow?.id ? `  "id": ${JSON.stringify(ctx.workflow.id)},` : undefined,
    '  "name": "简明工作流名称",',
    '  "steps": [',
    '    { "task": "阶段 1 目标", "mode": "read-only" },',
    '    { "task": "阶段 2 目标" }',
    "  ]",
    "}",
    "```",
  ].filter((line): line is string => Boolean(line));

  if (ctx?.mode === "edit" && ctx.workflow) {
    lines.push(
      `当前待调整工作流定义（${ctx.workflow.name}）：${JSON.stringify({
        id: ctx.workflow.id,
        name: ctx.workflow.name,
        steps: ctx.workflow.steps,
      })}`,
    );
    if (ctx.note) {
      lines.push(`补充调试线索：${ctx.note}`);
    }
  } else if (workflows.length > 0) {
    const summary = workflows
      .slice(0, 8)
      .map((item) => `${item.name}(id=${item.id}, ${item.steps.length}步)`)
      .join("；");
    lines.push(`当前工作区已有工作流参考：${summary}`);
  }

  return `${rawTask}\n\n${lines.join("\n")}`;
}

/**
 * 从已发送的用户消息正文中剥离 [工作流编排约定] 协议后缀，保持对话气泡与召回卡片简洁可读。
 */
export function stripWorkflowProtocolSuffix(text: string): {
  body: string;
  hasWorkflowProtocol: boolean;
} {
  const markerIdx = text.indexOf(WORKFLOW_PROTOCOL_MARKER);
  if (markerIdx < 0) {
    return { body: text, hasWorkflowProtocol: false };
  }
  return {
    body: text.slice(0, markerIdx).trimEnd(),
    hasWorkflowProtocol: true,
  };
}

/**
 * 校验并解析 Agent 输出中的 cairn-workflow JSON 草案，不合法时返回 null 以退化为普通代码块。
 */
export function parseCairnWorkflowBlock(rawCode: string): WorkflowBlueprintSpec | null {
  const trimmed = rawCode.trim();
  if (!trimmed.startsWith("{")) return null;

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return null;
  }

  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return null;
  }

  const rec = parsed as Record<string, unknown>;
  const name = typeof rec.name === "string" ? rec.name.trim() : "";
  if (!name || !Array.isArray(rec.steps) || rec.steps.length === 0 || rec.steps.length > 8) {
    return null;
  }

  if (Object.keys(rec).some((key) => key !== "id" && key !== "name" && key !== "steps")) return null;
  if (rec.id !== undefined && typeof rec.id !== "string") return null;

  const steps: WorkflowStepSpec[] = [];
  for (const item of rec.steps) {
    const step = parseWorkflowStep(item);
    if (!step) return null;
    steps.push(step);
  }
  const id = typeof rec.id === "string" ? rec.id.trim() || undefined : undefined;
  return { id, name, steps };
}

/** 只呈现可见且明确声明的节点字段。宿主再次校验执行与依赖合同。 */
function parseWorkflowStep(value: unknown): WorkflowStepSpec | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const fields = value as Record<string, unknown>;
  const supported = new Set(["task", "id", "kind", "model", "mode", "argv", "inputs", "max_attempts", "retry_from"]);
  if (Object.keys(fields).some((key) => !supported.has(key))) return null;
  const kind = fields.kind === undefined ? "execute" : fields.kind;
  if (typeof kind !== "string" || !WORKFLOW_NODE_KINDS.includes(kind as WorkflowNodeKind)) return null;
  for (const key of ["task", "id", "model", "mode", "retry_from"]) {
    if (fields[key] !== undefined && typeof fields[key] !== "string") return null;
  }
  const task = (fields.task as string | undefined)?.trim() ?? "";
  const mode = (fields.mode as string | undefined)?.trim();
  const model = (fields.model as string | undefined)?.trim();
  if (mode && mode !== "read-only" && mode !== "default") return null;
  if (kind === "execute" && (!task || fields.argv !== undefined)) return null;
  if (kind !== "execute" && (mode || model)) return null;
  if (kind === "script" && (task || !Array.isArray(fields.argv) || !fields.argv.length || fields.argv.some((arg) => typeof arg !== "string"))) return null;
  if (kind !== "script" && fields.argv !== undefined) return null;
  if (fields.inputs !== undefined && (!Array.isArray(fields.inputs) || fields.inputs.some((input) => typeof input !== "string"))) return null;
  if (fields.max_attempts !== undefined && (typeof fields.max_attempts !== "number" || !Number.isInteger(fields.max_attempts) || fields.max_attempts < 1 || fields.max_attempts > 4)) return null;

  return {
    task,
    kind: kind as WorkflowNodeKind,
    id: fields.id as string | undefined,
    model,
    mode,
    argv: fields.argv as string[] | undefined,
    inputs: fields.inputs as string[] | undefined,
    max_attempts: fields.max_attempts as number | undefined,
    retry_from: fields.retry_from as string | undefined,
  };
}

/** 宿主合同的阶段名称，供草案与版本详情显示。 */
export function workflowStepLabel(step: WorkflowStepSpec): string {
  if (step.kind === "script") return step.argv?.map((arg) => JSON.stringify(arg)).join(" ") || "固定命令";
  if (step.kind === "verify") return step.task || "独立验证";
  if (step.kind === "accept") return step.task || "人工验收";
  return step.task;
}

/** 应用门面阶段的界面词表。 */
export function workflowPhaseLabel(phase: string): string {
  switch (phase) {
    case "completed":
    case "succeeded": return "已完成";
    case "failed": return "执行失败";
    case "cancelled": return "已取消";
    case "paused": return "已暂停";
    case "waiting_approval": return "待工具审批";
    case "waiting_human": return "待人工验收";
    case "waiting_reconcile":
    case "uncertain": return "待副作用对账";
    case "retry_required": return "待确认返工";
    case "binding_unconfirmed": return "运行创建待确认";
    case "created":
    case "cancelling":
    case "settling": return "待收尾";
    case "running": return "运行中";
    case "unknown": return "状态待确认";
    default: return "待执行";
  }
}

/** 只使用应用门面的已绑定 Run、步骤状态和实例游标。 */
export function resolveWorkflowExecution(instance: WorkflowInstanceView): ResolvedWorkflowExecution {
  const status = instance.finished ? instance.finish_state || "unknown" : instance.phase || "ready";
  let latestRunId = "";
  let latestSequence = 0;
  const stages = instance.steps.map((step, index): WorkflowStageRuntime => {
    for (const attempt of step.attempts ?? []) {
      if (attempt.run_id && attempt.started_seq > latestSequence) {
        latestSequence = attempt.started_seq;
        latestRunId = attempt.run_id;
      }
    }
    let stageStatus: WorkflowStageRuntimeStatus = "pending";
    if (step.state === "succeeded") stageStatus = "done";
    else if (step.run_phase === "waiting_reconcile" || step.state === "uncertain") stageStatus = "waiting_reconcile";
    else if (step.run_phase === "waiting_approval" || (index === instance.resume_index && status === "waiting_human")) stageStatus = "waiting_approval";
    else if (step.state === "failed" || step.state === "cancelled") stageStatus = "failed";
    else if (step.state === "running") stageStatus = "running";
    return { index, label: step.id, runId: step.run_id, status: stageStatus };
  });
  return {
    instance, status, stages, latestRunId,
    workflowId: instance.definition?.id ?? "",
    workflowName: instance.definition_name || instance.workflow_id,
    completedCount: stages.filter((step) => step.status === "done").length,
    activeStepIndex: instance.resume_index < 0 ? Math.max(0, stages.length - 1) : instance.resume_index,
  };
}
