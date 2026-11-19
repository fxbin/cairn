/**
 * [INPUT]: Agent 输出的 cairn-workflow 蓝图草案或当前会话关联的多阶段工作流执行状态。
 * [OUTPUT]: 对话内“工作流草案确认卡”（WorkflowBlueprintCard）与“多阶段实时流水线卡”（WorkflowExecutionCard）。
 * [POS]: studio/frontend/src/workbench 对话式工作流的核心内联视图组件，只读反映草案与真实 Run 状态。
 *
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 * Author: fxbin
 */
import { useState } from "react";
import { workflowPhaseLabel, workflowStepLabel } from "./workflowConversation";
import type {
  ResolvedWorkflowExecution,
  WorkflowBlueprintSpec,
  WorkflowStageRuntimeStatus,
} from "./workflowConversation";

/**
 * 在助手回复内渲染结构化工作流草案，支持一键保存、保存并试运行与继续对话微调。
 */
export function WorkflowBlueprintCard({
  spec,
  rawJson,
  matchedExistingId,
  onSave,
  onSaveAndRun,
  onRefine,
  onOpenPortal,
}: {
  spec: WorkflowBlueprintSpec;
  rawJson: string;
  matchedExistingId?: string;
  onSave: (spec: WorkflowBlueprintSpec) => Promise<void>;
  onSaveAndRun: (spec: WorkflowBlueprintSpec) => Promise<void>;
  onRefine: (spec: WorkflowBlueprintSpec) => void;
  onOpenPortal: (workflowId?: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [savedId, setSavedId] = useState<string | null>(null);
  const [error, setError] = useState("");

  const effectiveId = matchedExistingId;
  const isUpdate = Boolean(effectiveId);

  const handleAction = async (job: () => Promise<void>, markSaved: boolean) => {
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      await job();
      if (markSaved) {
        setSavedId(effectiveId || spec.name);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section
      className="wb-wf-blueprint"
      data-testid="workflow-blueprint-card"
      aria-label={`工作流草案 ${spec.name}`}
    >
      <div className="wb-wf-blueprint-head">
        <div className="wb-wf-blueprint-title">
          <span className="wb-wf-icon" aria-hidden="true">
            ☍
          </span>
          <span className="wb-badge is-accent">
            {isUpdate ? "工作流更新草案" : "工作流草案"}
          </span>
          <strong>{spec.name}</strong>
          <span className="wb-badge is-muted">{spec.steps.length} 个阶段</span>
        </div>
        <button
          type="button"
          className="wb-ghost"
          onClick={() => onOpenPortal(effectiveId)}
        >
          查看工作流目录 →
        </button>
      </div>

      <ol className="wb-wf-blueprint-steps">
        {spec.steps.map((step, index) => (
          <li className="wb-wf-blueprint-step" key={index}>
            <div className="wb-wf-blueprint-step-meta">
              <span className="wb-badge is-muted">
                阶段 {String(index + 1).padStart(2, "0")}
              </span>
              <span
                className={
                  "wb-badge " + (step.mode === "read-only" ? "is-ok" : "is-muted")
                }
              >
                {step.mode === "read-only" ? "只读探索" : "按工作区权限"}
              </span>
              {step.model && <span className="wb-hash-pill">{step.model}</span>}
            </div>
            <p>{workflowStepLabel(step)}</p>
          </li>
        ))}
      </ol>

      <details className="wb-wf-blueprint-raw">
        <summary>查看结构化工作流 JSON</summary>
        <pre>{rawJson}</pre>
      </details>

      {error && <p role="alert">{error}</p>}
      <div className="wb-wf-blueprint-actions">
        <button
          type="button"
          className="wb-primary"
          disabled={busy || Boolean(savedId)}
          onClick={() =>
            void handleAction(
              () =>
                onSave({
                  ...spec,
                  ...(effectiveId ? { id: effectiveId } : {}),
                }),
              true,
            )
          }
        >
          {savedId
            ? "✓ 已保存到工作流目录"
            : isUpdate
              ? "✓ 更新工作流定义"
              : "✓ 保存为工作流"}
        </button>
        <button
          type="button"
          className="wb-secondary"
          disabled={busy || Boolean(savedId)}
          onClick={() =>
            void handleAction(
              () =>
                onSaveAndRun({
                  ...spec,
                  ...(effectiveId ? { id: effectiveId } : {}),
                }),
              true,
            )
          }
        >
          ▷ 保存并立即运行
        </button>
        <button
          type="button"
          className="wb-ghost"
          disabled={busy}
          onClick={() =>
            onRefine({
              ...spec,
              ...(effectiveId ? { id: effectiveId } : {}),
            })
          }
        >
          💬 继续对话调整
        </button>
      </div>
    </section>
  );
}

/**
 * 将运行态阶段映射为可读状态文案与语义圆点样式类。
 */
function stageStatusMeta(status: WorkflowStageRuntimeStatus): {
  toneClass: string;
  label: string;
} {
  switch (status) {
    case "done":
      return { toneClass: "is-done", label: "已完成" };
    case "running":
      return { toneClass: "is-running", label: "执行中" };
    case "waiting_approval":
      return { toneClass: "is-warn", label: "待授权" };
    case "waiting_reconcile":
      return { toneClass: "is-warn", label: "待对账" };
    case "failed":
      return { toneClass: "is-failed", label: "已中断" };
    default:
      return { toneClass: "is-pending", label: "待执行" };
  }
}

/**
 * 在对话流中渲染多阶段工作流实时流水线进度卡，支持直接跳转到对应阶段 Run 与一键对话调试。
 */
export function WorkflowExecutionCard({
  execution,
  activeRunId,
  onSelectStageRun,
  onOpenPortal,
  onDebugInChat,
  onStopRun,
}: {
  execution: ResolvedWorkflowExecution;
  activeRunId: string | null;
  onSelectStageRun: (runId: string) => void;
  onOpenPortal: (workflowId: string) => void;
  onDebugInChat: () => void;
  onStopRun?: (runId: string) => void;
}) {
  const statusDisplay = {
    badgeClass: execution.status === "completed" ? "is-ok" : execution.status === "running" ? "is-accent" : "is-warn",
    text: workflowPhaseLabel(execution.status),
  };

  const activeStage = execution.stages[execution.activeStepIndex];
  const canStop =
    execution.status === "running" &&
    Boolean(activeStage?.runId) &&
    Boolean(onStopRun);

  return (
    <section
      className="wb-wf-exec-card"
      data-testid="workflow-execution-card"
      aria-label={`工作流进度 ${execution.workflowName}`}
    >
      <div className="wb-wf-exec-head">
        <div className="wb-wf-exec-title">
          <span className="wb-wf-icon" aria-hidden="true">
            ☍
          </span>
          <span className={"wb-badge " + statusDisplay.badgeClass}>
            {statusDisplay.text}
          </span>
          <strong>{execution.workflowName}</strong>
        </div>
        <div className="wb-wf-exec-actions">
          <span className="wb-wf-exec-ratio">
            {execution.stages.length} 个阶段 · {execution.completedCount}/
            {execution.stages.length}
          </span>
          <button
            type="button"
            className="wb-ghost"
            onClick={() => onOpenPortal(execution.workflowId)}
          >
            查看目录
          </button>
          <button type="button" className="wb-ghost" onClick={onDebugInChat}>
            💬 对话调试
          </button>
          {canStop && activeStage?.runId && (
            <button
              type="button"
              className="wb-danger"
              onClick={() => onStopRun?.(activeStage.runId!)}
            >
              ■ 停止
            </button>
          )}
        </div>
      </div>

      <div className="wb-wf-exec-track" role="list" aria-label="工作流阶段列表">
        {execution.stages.map((stage) => {
          const meta = stageStatusMeta(stage.status);
          const stageRunId = stage.runId;
          const isViewed = Boolean(stageRunId && stageRunId === activeRunId);
          return (
            <div
              key={stage.index}
              role="listitem"
              className={
                "wb-wf-stage-node " +
                meta.toneClass +
                (isViewed ? " is-viewed" : "")
              }
            >
              <div className="wb-wf-stage-rail">
                <span className={"wb-wf-stage-dot " + meta.toneClass} aria-hidden="true" />
                <span className="wb-wf-stage-connector" aria-hidden="true" />
              </div>
              <button
                type="button"
                className="wb-wf-stage-pill"
                disabled={!stageRunId}
                title={stage.label}
                onClick={() => {
                  if (stageRunId) onSelectStageRun(stageRunId);
                }}
              >
                <span className="wb-wf-stage-idx">
                  {String(stage.index + 1).padStart(2, "0")}
                </span>
                <span className="wb-wf-stage-task">{stage.label}</span>
                <span className="wb-wf-stage-status">{meta.label}</span>
              </button>
            </div>
          );
        })}
      </div>
    </section>
  );
}
