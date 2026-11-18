/**
 * INPUT: 用户原始输入、当前工作流编排上下文、工作区工作流列表与 RunHistoryView 运行序列。
 * OUTPUT: 对话式工作流意图探测/协议展开、cairn-workflow 草案 JSON 解析与多阶段执行进度投影纯函数。
 * POS: studio/frontend/src/workbench 对话创建/调试/编辑工作流的核心协调模型，不改变后端授权与验证边界。
 *
 * Author: fxbin
 */
import type { RunHistoryView } from "../desktop/types";

export interface WorkflowStepSpec {
  task: string;
  model?: string;
  mode?: string;
}

export interface WorkflowDefinitionItem {
  id: string;
  workspace?: string;
  name: string;
  steps: WorkflowStepSpec[];
  max_steps?: number;
}

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

export interface WorkflowSessionBinding {
  workflowId: string;
  workflowName: string;
  workspace: string;
  firstRunId: string;
  sessionId?: string;
  steps: WorkflowStepSpec[];
  startedAt: string;
}

export type WorkflowStageRuntimeStatus =
  | "done"
  | "running"
  | "waiting_approval"
  | "failed"
  | "pending";

export interface WorkflowStageRuntime {
  index: number;
  step: WorkflowStepSpec;
  run?: RunHistoryView;
  status: WorkflowStageRuntimeStatus;
}

export interface ResolvedWorkflowExecution {
  binding: WorkflowSessionBinding;
  workflowId: string;
  workflowName: string;
  sessionId?: string;
  latestRunId: string;
  status: "running" | "waiting_approval" | "completed" | "failed";
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
const WORKFLOW_BINDINGS_STORAGE_KEY = "cairn.gui.workflowBindings.v1";
const MAX_STORED_BINDINGS = 30;
const TERMINAL_STATES = new Set(["completed", "failed", "cancelled"]);

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
    "每个阶段字段为 { \"task\": string, \"mode\"?: \"read-only\" | \"default\", \"model\"?: string }。",
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

  const steps: WorkflowStepSpec[] = [];
  for (const item of rec.steps) {
    if (!item || typeof item !== "object" || Array.isArray(item)) {
      return null;
    }
    const stepObj = item as Record<string, unknown>;
    const task = typeof stepObj.task === "string" ? stepObj.task.trim() : "";
    if (!task) {
      return null;
    }
    const step: WorkflowStepSpec = { task };
    if (typeof stepObj.model === "string" && stepObj.model.trim()) {
      step.model = stepObj.model.trim();
    }
    if (typeof stepObj.mode === "string") {
      const m = stepObj.mode.trim();
      if (m === "read-only" || m === "default") {
        step.mode = m;
      } else if (m !== "") {
        return null;
      }
    }
    steps.push(step);
  }

  const id = typeof rec.id === "string" && rec.id.trim() ? rec.id.trim() : undefined;
  return {
    ...(id ? { id } : {}),
    name,
    steps,
  };
}

/**
 * 从 localStorage 读取指定工作区最近通过前端触发的工作流运行绑定索引。
 */
export function loadWorkflowBindings(workspace: string): WorkflowSessionBinding[] {
  if (!workspace || typeof localStorage === "undefined") return [];
  try {
    const raw = localStorage.getItem(WORKFLOW_BINDINGS_STORAGE_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(
      (item): item is WorkflowSessionBinding =>
        Boolean(item) &&
        typeof item === "object" &&
        (item as WorkflowSessionBinding).workspace === workspace &&
        typeof (item as WorkflowSessionBinding).firstRunId === "string" &&
        Array.isArray((item as WorkflowSessionBinding).steps),
    );
  } catch {
    return [];
  }
}

/**
 * 持久化更新后的工作区工作流运行绑定列表，自动保留全局最近记录上限。
 */
export function saveWorkflowBindings(
  workspace: string,
  workspaceList: readonly WorkflowSessionBinding[],
): void {
  if (!workspace || typeof localStorage === "undefined") return;
  try {
    const raw = localStorage.getItem(WORKFLOW_BINDINGS_STORAGE_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    const otherWorkspaces = Array.isArray(parsed)
      ? parsed.filter(
          (item): item is WorkflowSessionBinding =>
            Boolean(item) &&
            typeof item === "object" &&
            (item as WorkflowSessionBinding).workspace !== workspace,
        )
      : [];

    const deduped: WorkflowSessionBinding[] = [];
    const seen = new Set<string>();
    for (const item of workspaceList) {
      if (!item.firstRunId || seen.has(item.firstRunId)) continue;
      seen.add(item.firstRunId);
      deduped.push(item);
    }

    const merged = [...deduped.slice(0, MAX_STORED_BINDINGS), ...otherWorkspaces].slice(
      0,
      MAX_STORED_BINDINGS * 2,
    );
    localStorage.setItem(WORKFLOW_BINDINGS_STORAGE_KEY, JSON.stringify(merged));
  } catch {
    // 存储配额受限时静默降级，不影响运行事实。
  }
}

/**
 * 将 RunHistoryView 列表按工作流绑定关系投影为有序多阶段运行进度与活跃阶段下标。
 */
export function resolveWorkflowExecution(
  binding: WorkflowSessionBinding,
  runs: readonly RunHistoryView[],
): ResolvedWorkflowExecution {
  const byId = new Map<string, RunHistoryView>();
  for (const r of runs) {
    byId.set(r.run_id, r);
  }

  const matchedIds = new Set<string>();
  const firstRun = byId.get(binding.firstRunId);
  const resolvedSessionId = binding.sessionId || firstRun?.session_id;

  const byChain: RunHistoryView[] = [];
  if (firstRun) {
    byChain.push(firstRun);
    matchedIds.add(firstRun.run_id);
  }

  // 优先沿 prev_run_id 线性推进链收敛有序步骤，再用同 session_id 兜底补齐。
  let advanced = true;
  let safety = 0;
  while (advanced && safety < 20) {
    advanced = false;
    safety += 1;
    const tail = byChain[byChain.length - 1];
    if (!tail) break;
    const nextRun = runs.find(
      (r) => r.prev_run_id === tail.run_id && !matchedIds.has(r.run_id),
    );
    if (nextRun) {
      matchedIds.add(nextRun.run_id);
      byChain.push(nextRun);
      advanced = true;
    }
  }

  if (resolvedSessionId) {
    const sameSession = runs
      .filter((r) => r.session_id === resolvedSessionId && !matchedIds.has(r.run_id))
      .sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));
    for (const r of sameSession) {
      matchedIds.add(r.run_id);
      byChain.push(r);
    }
  }

  const stages: WorkflowStageRuntime[] = binding.steps.map((step, index) => {
    const run = byChain[index];
    if (!run) {
      return { index, step, status: "pending" };
    }
    let status: WorkflowStageRuntimeStatus = "running";
    if (run.waiting_approval || run.waiting_reconcile) {
      status = "waiting_approval";
    } else if (run.state === "completed") {
      status = "done";
    } else if (run.state === "failed" || run.state === "cancelled") {
      status = "failed";
    }
    return { index, step, run, status };
  });

  const completedCount = stages.filter((s) => s.status === "done").length;
  let activeStepIndex = stages.findIndex(
    (s) => s.status === "running" || s.status === "waiting_approval",
  );
  if (activeStepIndex < 0) {
    const failedIdx = stages.findIndex((s) => s.status === "failed");
    activeStepIndex = failedIdx >= 0 ? failedIdx : Math.max(0, stages.length - 1);
  }

  let overallStatus: ResolvedWorkflowExecution["status"] = "running";
  if (stages.some((s) => s.status === "waiting_approval")) {
    overallStatus = "waiting_approval";
  } else if (stages.some((s) => s.status === "running")) {
    overallStatus = "running";
  } else if (stages.some((s) => s.status === "failed")) {
    overallStatus = "failed";
  } else if (stages.length > 0 && completedCount === stages.length) {
    overallStatus = "completed";
  } else if (byChain.length > 0) {
    const last = byChain[byChain.length - 1];
    if (TERMINAL_STATES.has(last.state) && last.state !== "completed") {
      overallStatus = "failed";
    }
  }

  const latestRunId = byChain[byChain.length - 1]?.run_id || binding.firstRunId;
  return {
    binding: { ...binding, sessionId: resolvedSessionId },
    workflowId: binding.workflowId,
    workflowName: binding.workflowName,
    sessionId: resolvedSessionId,
    latestRunId,
    status: overallStatus,
    completedCount,
    activeStepIndex,
    stages,
  };
}
