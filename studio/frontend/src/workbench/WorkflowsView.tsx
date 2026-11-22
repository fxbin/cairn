/**
 * [INPUT]: 当前工作区、App 定义和实例、显式对话及控制回调。
 * [OUTPUT]: 定义目录、运行历史和显式控制。创建与编辑从对话入口确认。
 * [POS]: studio/frontend/src/workbench 工作流导航页视图，不越权绕过对话审查与后端执行边界。
 *
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 * Author: fxbin
 */
import { useEffect, useMemo, useState } from "react";
import { desktop } from "../desktop/client";
import type { WorkflowInstanceView } from "../desktop/types";
import { WorkflowInstanceControls, type WorkflowControl } from "./WorkflowInstanceControls";
import { WorkflowVerificationSummary } from "./WorkflowVerificationSummary";
import {
  STARTER_WORKFLOW_PROMPTS,
  resolveWorkflowExecution,
  type ResolvedWorkflowExecution,
  type WorkflowDefinitionItem,
  workflowPhaseLabel,
  workflowStepLabel,
  type WorkflowStepSpec,
} from "./workflowConversation";

export interface WorkflowsViewProps {
  workspace: string;
  notify: (message: string) => void;
  instances?: WorkflowInstanceView[];
  onControl?: WorkflowControl;
  historyError?: string;
  focusedWorkflowId?: string | null;
  onConverseCreate?: (initialPrompt?: string) => void;
  onConverseEdit?: (wf: WorkflowDefinitionItem, note?: string) => void;
  onLaunchWorkflow?: (wf: WorkflowDefinitionItem) => Promise<void> | void;
  onOpenRun?: (runId: string) => void;
}

/**
 * 将步骤规范化为 SaveWorkflow 所需的纯净载荷，省略空的 model/mode。
 */
function normalizeStepForSave(step: WorkflowStepSpec): WorkflowStepSpec {
  const normalized: WorkflowStepSpec = { ...step, task: step.task.trim() };
  delete normalized.model;
  delete normalized.mode;
  if (step.model && step.model.trim()) {
    normalized.model = step.model.trim();
  }
  if (step.mode && step.mode.trim()) {
    normalized.mode = step.mode.trim();
  }
  return normalized;
}

/**
 * 切换兼容编辑器中单个步骤的运行模式。
 */
function applyStepMode(step: WorkflowStepSpec, nextMode: string): WorkflowStepSpec {
  const updated: WorkflowStepSpec = { ...step };
  delete updated.mode;
  if (nextMode.trim()) updated.mode = nextMode.trim();
  return updated;
}

/**
 * 渲染工作流目录：当接入主工作台时为纯只读检视/运行历史/触发门户，创建与编辑统一跳转对话；
 * 独立无回调挂载时保留只用单测使用的基础回退表单。
 */
export default function WorkflowsView({
  workspace,
  notify,
  instances = [],
  onControl,
  historyError,
  focusedWorkflowId,
  onConverseCreate,
  onConverseEdit,
  onLaunchWorkflow,
  onOpenRun,
}: WorkflowsViewProps) {
  const portalMode = typeof onConverseCreate === "function";

  const [items, setItems] = useState<WorkflowDefinitionItem[]>([]);
  const [selectedId, setSelectedId] = useState<string>("");
  const [detailTab, setDetailTab] = useState<"definition" | "history">("definition");
  const [busy, setBusy] = useState(false);
  const [revision, setRevision] = useState(0);

  // 兼容独立挂载模式下的受控表单状态（portalMode 下不渲染）。
  const [editId, setEditId] = useState("");
  const [name, setName] = useState("");
  const [steps, setSteps] = useState<WorkflowStepSpec[]>([{ task: "" }]);

  useEffect(() => {
    let active = true;
    void desktop
      .listWorkflows(workspace)
      .then((data) => {
        if (!active) return;
        const normalized = data.map((w) => ({ ...w, steps: w.steps ?? [] }));
        setItems(normalized);
        setSelectedId((prev) => {
          if (focusedWorkflowId && normalized.some((w) => w.id === focusedWorkflowId)) {
            return focusedWorkflowId;
          }
          if (prev && normalized.some((w) => w.id === prev)) {
            return prev;
          }
          return normalized[0]?.id || "";
        });
      })
      .catch((e) => {
        if (active) notify(e instanceof Error ? e.message : String(e));
      });
    return () => {
      active = false;
    };
  }, [workspace, revision, focusedWorkflowId, notify]);

  const resolvedExecutions = useMemo(() => {
    const byWorkflow = new Map<string, ResolvedWorkflowExecution[]>();
    for (const instance of instances) {
      const resolved = resolveWorkflowExecution(instance);
      const list = byWorkflow.get(resolved.workflowId) ?? [];
      list.push(resolved);
      byWorkflow.set(resolved.workflowId, list);
    }
    return byWorkflow;
  }, [instances]);

  const perform = async (job: () => Promise<unknown>, message?: string) => {
    if (busy) return;
    setBusy(true);
    try {
      await job();
      setRevision((v) => v + 1);
      if (message) notify(message);
    } catch (e) {
      notify(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  if (portalMode) {
    const selected = items.find((w) => w.id === selectedId) ?? items[0];
    const selectedExecs = selected ? resolvedExecutions.get(selected.id) ?? [] : [];
    const taskInstances = instances.filter((instance) => instance.task_id);
    const activeExec = selectedExecs.find(
      (exec) => !exec.instance.finished,
    );

    return (
      <div className="wb-workflows wb-workflow-portal" data-testid="workflows-view">
        <div className="wb-workflow-portal-hero">
          <div className="wb-workflow-portal-copy">
            <strong>工作流目录</strong>
            <p>创建或调整定义后，在这里选择修订、启动实例或处理人工操作。</p>
          </div>
          <button
            type="button"
            className="wb-primary"
            onClick={() => onConverseCreate?.()}
          >
            ✦ 通过对话创建工作流
          </button>
        </div>

        {taskInstances.length > 0 && (
          <section className="wb-wf-history-list" aria-label="自主任务">
            <h3>自主任务</h3>
            {taskInstances.map((instance) => (
              <article className="wb-wf-history-record" key={instance.workflow_id}>
                <div className="wb-wf-history-top">
                  <strong>{instance.definition_name || instance.task_id}</strong>
                  <span className="wb-badge">{workflowPhaseLabel(instance.finish_state ?? instance.phase ?? "ready")}</span>
                  <span className="wb-hash-pill">{instance.task_id}</span>
                </div>
                {instance.finish_reason && <p>{instance.finish_reason}</p>}
                {instance.verification && <WorkflowVerificationSummary report={instance.verification} criteria={instance.verification_criteria} onOpenRun={onOpenRun} />}
                {onControl && !historyError && (
                  <WorkflowInstanceControls
                    key={instance.sequence}
                    workspace={workspace}
                    instance={instance}
                    onControl={onControl}
                    onOpenRun={onOpenRun}
                  />
                )}
              </article>
            ))}
          </section>
        )}

        {items.length === 0 ? (taskInstances.length > 0 ? null : (
          <div className="wb-wf-empty-stage">
            <div className="wb-wf-how-strip" aria-label="对话工作流三步法">
              <span>1. 对话说明目标或贴入现有脚本</span>
              <span aria-hidden="true">→</span>
              <span>2. Agent 生成草案并支持直接试运行调试</span>
              <span aria-hidden="true">→</span>
              <span>3. 一键保存至此目录，随时复用或追改</span>
            </div>
            <div className="wb-wf-starter-grid">
              {STARTER_WORKFLOW_PROMPTS.map((tpl) => (
                <button
                  key={tpl.id}
                  type="button"
                  className="wb-wf-starter-card"
                  onClick={() => onConverseCreate?.(tpl.prompt)}
                >
                  <div className="wb-wf-starter-head">
                    <strong>{tpl.title}</strong>
                    <span className="wb-badge is-accent">{tpl.badge}</span>
                  </div>
                  <p>{tpl.description}</p>
                  <span className="wb-wf-starter-cta">去对话创建 →</span>
                </button>
              ))}
            </div>
          </div>
        )) : (
          <div className="wb-wf-portal-grid">
            <aside className="wb-wf-directory" aria-label="已保存工作流列表">
              <div className="wb-wf-directory-head">
                <span>已保存 ({items.length})</span>
              </div>
              <div className="wb-wf-directory-list">
                {items.map((wf) => {
                  const execs = resolvedExecutions.get(wf.id) ?? [];
                  const isRunning = execs.some(
                    (e) => !e.instance.finished,
                  );
                  const isSelected = selected?.id === wf.id;
                  return (
                    <button
                      key={wf.id}
                      type="button"
                      className={"wb-wf-dir-item" + (isSelected ? " is-selected" : "")}
                      onClick={() => {
                        setSelectedId(wf.id);
                        setDetailTab("definition");
                      }}
                    >
                      <div className="wb-wf-dir-item-top">
                        <strong>{wf.name}</strong>
                        {isRunning && <span className="wb-wf-live-dot" title="运行中" />}
                      </div>
                      <small>
                        {wf.steps.length} 个阶段
                        {execs.length > 0 ? ` · ${execs.length} 次记录` : ""}
                      </small>
                    </button>
                  );
                })}
              </div>
            </aside>

            {selected && (
              <section className="wb-wf-inspector" aria-label={`工作流详情 ${selected.name}`}>
                <div className="wb-wf-inspector-head">
                  <div className="wb-wf-inspector-title">
                    <div className="wb-wf-tab-switch" role="tablist" aria-label="工作流视图切换">
                      <button
                        type="button"
                        role="tab"
                        aria-selected={detailTab === "definition"}
                        className={detailTab === "definition" ? "is-active" : ""}
                        onClick={() => setDetailTab("definition")}
                      >
                        流程定义
                      </button>
                      <button
                        type="button"
                        role="tab"
                        aria-selected={detailTab === "history"}
                        className={detailTab === "history" ? "is-active" : ""}
                        onClick={() => setDetailTab("history")}
                      >
                        运行历史
                        {selectedExecs.length > 0 ? ` (${selectedExecs.length})` : ""}
                      </button>
                    </div>
                    <span
                      className={
                        "wb-wf-status-pill " +
                        (activeExec
                          ? activeExec.status === "running" ? "is-running" : "is-warn"
                          : "is-ready")
                      }
                    >
                      <i aria-hidden="true" />
                      {activeExec ? workflowPhaseLabel(activeExec.status) : "已就绪"}
                    </span>
                  </div>

                  <div className="wb-wf-inspector-actions">
                    <button
                      type="button"
                      className="wb-ghost"
                      disabled={busy}
                      onClick={() => onConverseEdit?.(selected)}
                    >
                      💬 对话调试 / 修改
                    </button>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() =>
                        void perform(
                          () => typeof desktop.startWorkflowInstance === "function"
                            ? desktop.startWorkflowInstance({ workspace, definition_id: selected.id, revision: selected.revision ?? 0, dry_run: true })
                            : desktop.runWorkflow(workspace, selected.id, true),
                          "已完成顺序计划预演（未执行实际运行）。",
                        )
                      }
                    >
                      预演
                    </button>
                    <button
                      type="button"
                      className="wb-primary"
                      aria-label="运行"
                      disabled={busy}
                      onClick={() => {
                        if (
                          !window.confirm(
                            "确认运行工作流？每个步骤仍受现有权限、审批与验证合同约束。",
                          )
                        ) {
                          return;
                        }
                        setDetailTab("history");
                        if (onLaunchWorkflow) {
                          void perform(() => Promise.resolve(onLaunchWorkflow(selected)));
                        } else {
                          void perform(
                            () => desktop.runWorkflow(workspace, selected.id, false),
                            "工作流运行请求已受理；请查看对话中的执行进度。",
                          );
                        }
                      }}
                    >
                      ▷ 立即运行
                    </button>
                  </div>
                </div>

                <div className="wb-wf-inspector-meta">
                  <h3>{selected.name}</h3>
                  <span className="wb-hash-pill">{selected.revision ? `修订 ${selected.revision}` : "旧版模板"}</span>
                </div>

                {detailTab === "definition" ? (
                  <>
                    <div className="wb-step-pipeline wb-wf-readonly-steps">
                      {selected.steps.map((step, index) => (
                        <div key={index} className="wb-step-card is-readonly">
                          <div className="wb-wf-step-card-head">
                            <span className="wb-badge is-accent">
                              STEP {String(index + 1).padStart(2, "0")}
                            </span>
                            <span
                              className={
                                "wb-badge " +
                                (step.mode === "read-only" ? "is-ok" : "is-muted")
                              }
                            >
                              {step.mode === "read-only"
                                ? "只读探索 (read-only)"
                                : "按工作区权限"}
                            </span>
                            {step.model && (
                              <span className="wb-hash-pill">{step.model}</span>
                            )}
                          </div>
                          <p>{workflowStepLabel(step)}</p>
                        </div>
                      ))}
                    </div>
                    <p className="wb-wf-readonly-hint">
                      如需增删阶段、修改提示词或排查执行异常，请点击右上角「💬 对话调试 / 修改」，由 Agent 在对话中协助修订。
                    </p>
                  </>
                ) : (
                  <div className="wb-wf-history-list">
                    {historyError ? <p role="alert">无法读取工作流历史。{historyError}</p> : null}
                    {!historyError && selectedExecs.length === 0 ? (
                      <div className="wb-wf-history-empty">
                        这个工作流暂无持久化实例。
                      </div>
                    ) : (
                      selectedExecs.map((exec) => {
                        const tone = exec.status === "completed" ? "is-ok" : exec.status === "running" ? "is-accent" : "is-warn";
                        const label = workflowPhaseLabel(exec.status);
                        return (
                          <div className="wb-wf-history-record" key={exec.instance.workflow_id}>
                          <div className="wb-wf-history-row">
                            <div className="wb-wf-history-main">
                              <div className="wb-wf-history-top">
                                <span className={"wb-badge " + tone}>{label}</span>
                                <span className="wb-hash-pill">{exec.instance.workflow_id}</span>
                                {exec.instance.definition && <span>修订 {exec.instance.definition.revision}</span>}
                                <span className="wb-wf-history-time">
                                  {exec.instance.created_at
                                    ? new Date(exec.instance.created_at).toLocaleString("zh-CN", {
                                        month: "numeric",
                                        day: "numeric",
                                        hour: "2-digit",
                                        minute: "2-digit",
                                      })
                                    : ""}
                                </span>
                              </div>
                              <div className="wb-wf-history-sub">
                                完成 {exec.completedCount} / {exec.stages.length} 个阶段
                              </div>
                            </div>
                            <div className="wb-wf-history-actions">
                              {onConverseEdit && (
                                <button
                                  type="button"
                                  className="wb-ghost"
                                  onClick={() =>
                                    onConverseEdit(
                                      selected,
                                      `关联历史运行 ${exec.latestRunId}（状态：${label}，完成 ${exec.completedCount}/${exec.stages.length} 步）`,
                                    )
                                  }
                                >
                                  💬 基于此运行调试
                                </button>
                              )}
                              {onOpenRun && exec.latestRunId && (
                                <button
                                  type="button"
                                  className="wb-secondary"
                                  style={{ marginTop: 0 }}
                                  onClick={() => onOpenRun(exec.latestRunId)}
                                >
                                  查看对话记录 →
                                </button>
                              )}
                            </div>
                          </div>
                            {exec.instance.verification && <WorkflowVerificationSummary report={exec.instance.verification} criteria={exec.instance.verification_criteria} onOpenRun={onOpenRun} />}
                            {onControl && !historyError && (
                              <WorkflowInstanceControls
                                key={[
                                  exec.instance.sequence,
                                  exec.instance.pending_approval?.operation_id,
                                  exec.instance.pending_reconcile?.operation_id,
                                ].join(":")}
                                workspace={workspace}
                                instance={exec.instance}
                                onControl={onControl}
                                onOpenRun={onOpenRun}
                              />
                            )}
                          </div>
                        );
                      })
                    )}
                  </div>
                )}
              </section>
            )}
          </div>
        )}
      </div>
    );
  }

  // 独立无回调挂载时的兼容视图（供既有孤立组件单测运行）。
  const valid =
    name.trim().length > 0 &&
    steps.length > 0 &&
    steps.length <= 8 &&
    steps.every((x) => x.task.trim().length > 0);

  const editCompat = (item?: WorkflowDefinitionItem) => {
    setEditId(item?.id || "");
    setName(item?.name || "");
    setSteps(item ? item.steps.map((x) => ({ ...x })) : [{ task: "" }]);
  };

  return (
    <div className="wb-workflows" data-testid="workflows-view" style={{ maxWidth: 840 }}>
      <p>工作流步骤按顺序运行；预演只检查执行计划，真实运行仍使用运行时的权限、验证与恢复合同。</p>

      {items.length > 0 && (
        <div style={{ display: "grid", gap: 14, marginBottom: 24 }}>
          {items.map((item) => (
            <div
              key={item.id}
              style={{
                padding: 16,
                borderRadius: 14,
                border: "1px solid var(--wb-line)",
                background: "var(--wb-card, var(--wb-paper))",
              }}
            >
              <div
                style={{
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: 12,
                  flexWrap: "wrap",
                }}
              >
                <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                  <strong>{item.name}</strong>
                  <span className="wb-badge is-muted">{item.steps.length} 个步骤</span>
                </div>
                <div className="wb-actions" style={{ paddingTop: 0 }}>
                  <button type="button" onClick={() => editCompat(item)}>
                    编辑
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() =>
                      void perform(
                        () => desktop.runWorkflow(workspace, item.id, true),
                        "已提交预演，未开始真实运行。",
                      )
                    }
                  >
                    预演
                  </button>
                  <button
                    type="button"
                    className="wb-primary"
                    disabled={busy}
                    onClick={() => {
                      if (!window.confirm("确认运行工作流？每个步骤都受现有权限和验证合同约束。"))
                        return;
                      void perform(
                        () => desktop.runWorkflow(workspace, item.id, false),
                        "工作流运行请求已受理；请检查相应 Run 的 Journal 状态。",
                      );
                    }}
                  >
                    运行
                  </button>
                </div>
              </div>
              <div className="wb-step-pipeline" style={{ marginTop: 12 }}>
                {item.steps.map((step, index) => (
                  <div key={index} className="wb-step-card">
                    <div
                      style={{
                        display: "flex",
                        alignItems: "center",
                        justifyContent: "space-between",
                        gap: 6,
                        marginBottom: 6,
                      }}
                    >
                      <span className="wb-badge is-accent">
                        STEP {String(index + 1).padStart(2, "0")}
                      </span>
                      <span className={"wb-badge " + (step.mode === "read-only" ? "is-ok" : "is-muted")}>
                        {step.mode || "default"}
                      </span>
                    </div>
                    <p style={{ margin: 0 }}>{step.task}</p>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}

      <div
        className="wb-workflow-editor"
        style={{
          padding: 18,
          borderRadius: 14,
          border: "1px solid var(--wb-line)",
          background: "var(--wb-card, var(--wb-paper))",
        }}
      >
        <div
          className="wb-toolbar-lite"
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            gap: 10,
            marginBottom: 12,
          }}
        >
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <h3 style={{ margin: 0 }}>{editId ? "编辑工作流" : "新工作流"}</h3>
            <span className="wb-badge is-muted">{steps.length} / 8 步</span>
          </div>
          <button
            type="button"
            className="wb-secondary"
            style={{ marginTop: 0, padding: "6px 12px" }}
            onClick={() => editCompat()}
          >
            新建
          </button>
        </div>

        <label style={{ display: "grid", gap: 6, fontSize: 12, marginBottom: 14 }}>
          工作流名称
          <input
            aria-label="工作流名称"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="例如：检查 → 修复 → 验证"
            style={{
              width: "100%",
              padding: "9px 11px",
              borderRadius: 9,
              border: "1px solid var(--wb-line)",
              background: "var(--wb-elev, var(--wb-side))",
              color: "var(--wb-title)",
            }}
          />
        </label>

        <div className="wb-workflow-steps wb-step-pipeline">
          {steps.map((step, i) => (
            <div key={i} className="wb-step-card">
              <div
                style={{
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: 8,
                  flexWrap: "wrap",
                  marginBottom: 8,
                }}
              >
                <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                  <span className="wb-badge is-accent">STEP {String(i + 1).padStart(2, "0")}</span>
                  <select
                    aria-label={"第" + (i + 1) + "步运行模式"}
                    value={step.mode || ""}
                    onChange={(e) =>
                      setSteps((current) =>
                        current.map((x, j) => (i === j ? applyStepMode(x, e.target.value) : x)),
                      )
                    }
                    style={{
                      padding: "4px 8px",
                      fontSize: 11.5,
                      borderRadius: 7,
                      border: "1px solid var(--wb-line)",
                      background: "var(--wb-card, var(--wb-paper))",
                      color: "var(--wb-ink)",
                    }}
                  >
                    <option value="">默认模式 (default)</option>
                    <option value="read-only">只读模式 (read-only)</option>
                  </select>
                </div>
                <button
                  type="button"
                  aria-label={"删除第" + (i + 1) + "步"}
                  disabled={steps.length === 1}
                  onClick={() => setSteps((current) => current.filter((_, j) => i !== j))}
                  style={{ padding: "2px 8px", borderRadius: 6 }}
                >
                  ×
                </button>
              </div>
              <textarea
                aria-label={"第" + (i + 1) + "步"}
                value={step.task}
                onChange={(e) =>
                  setSteps((current) =>
                    current.map((x, j) => (i === j ? { ...x, task: e.target.value } : x)),
                  )
                }
                placeholder="填写当前步骤的具体目标…"
                style={{
                  width: "100%",
                  minHeight: 68,
                  resize: "vertical",
                  padding: "8px 10px",
                  borderRadius: 8,
                  border: "1px solid var(--wb-line)",
                  background: "var(--wb-card, var(--wb-paper))",
                  color: "var(--wb-title)",
                }}
              />
            </div>
          ))}
        </div>

        <div className="wb-actions" style={{ marginTop: 14, flexWrap: "wrap" }}>
          <button
            type="button"
            className="wb-secondary"
            style={{ marginTop: 0 }}
            disabled={steps.length >= 8}
            onClick={() =>
              setSteps((current) => (current.length < 8 ? current.concat({ task: "" }) : current))
            }
          >
            ＋ 添加步骤（最多 8 步）
          </button>
          <button
            type="button"
            className="wb-primary"
            disabled={busy || !valid}
            onClick={() =>
              void perform(async () => {
                await desktop.saveWorkflow({
                  id: editId || undefined,
                  workspace,
                  name: name.trim(),
                  steps: steps.map(normalizeStepForSave),
                });
                editCompat();
              }, "工作流已保存到当前工作区。")
            }
          >
            保存工作流
          </button>
        </div>
      </div>
    </div>
  );
}
