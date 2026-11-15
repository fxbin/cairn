/**
 * 受控线性工作流面板：支持可视化有序步骤管道编辑、计划预演与显式确认运行。
 * 预演只检查有序计划，真实运行仍受运行时 Policy、授权与验证合同约束。
 * Author: fxbin
 */
import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";

type WorkflowStep = {
  task: string;
  model?: string;
  mode?: string;
};

type Workflow = {
  id: string;
  name: string;
  steps: WorkflowStep[];
};

/**
 * 将编辑态步骤规范化为提交给 SaveWorkflow 的纯净步骤结构。
 * 仅在用户显式配置了非空 mode 或 model 时写入对应字段，默认空态不附加多余键。
 */
function normalizeStepForSave(step: WorkflowStep): WorkflowStep {
  const normalized: WorkflowStep = { task: step.task.trim() };
  if (step.model && step.model.trim()) {
    normalized.model = step.model.trim();
  }
  if (step.mode && step.mode.trim()) {
    normalized.mode = step.mode.trim();
  }
  return normalized;
}

/**
 * 更新指定步骤的可选运行模式，选回默认模式时移除 mode 字段以保持结构纯净。
 */
function applyStepMode(step: WorkflowStep, nextMode: string): WorkflowStep {
  const updated: WorkflowStep = { task: step.task };
  if (step.model) {
    updated.model = step.model;
  }
  if (nextMode.trim()) {
    updated.mode = nextMode.trim();
  }
  return updated;
}

/**
 * 线性工作流列表与可视化管道编辑器组件。
 */
export default function WorkflowsView({
  workspace,
  notify,
}: {
  workspace: string;
  notify: (message: string) => void;
}) {
  const [items, setItems] = useState<Workflow[]>([]);
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const [steps, setSteps] = useState<WorkflowStep[]>([{ task: "" }]);
  const [busy, setBusy] = useState(false);
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    let active = true;
    void desktop
      .listWorkflows(workspace)
      .then((data) => {
        if (active) setItems(data);
      })
      .catch((e) => {
        if (active) notify(e instanceof Error ? e.message : String(e));
      });
    return () => {
      active = false;
    };
  }, [workspace, revision, notify]);

  const edit = (item?: Workflow) => {
    setId(item?.id || "");
    setName(item?.name || "");
    setSteps(item ? item.steps.map((x) => ({ ...x })) : [{ task: "" }]);
  };

  const perform = async (job: () => Promise<unknown>, message: string) => {
    if (busy) return;
    setBusy(true);
    try {
      await job();
      setRevision((v) => v + 1);
      notify(message);
    } catch (e) {
      notify(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const valid =
    name.trim().length > 0 &&
    steps.length > 0 &&
    steps.length <= 8 &&
    steps.every((x) => x.task.trim().length > 0);

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
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12, flexWrap: "wrap" }}>
                <div>
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>{item.name}</strong>
                    <span className="wb-badge is-muted">{item.steps.length} 个步骤</span>
                  </div>
                </div>
                <div className="wb-actions" style={{ paddingTop: 0 }}>
                  <button type="button" onClick={() => edit(item)}>
                    编辑
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() =>
                      void perform(() => desktop.runWorkflow(workspace, item.id, true), "已提交预演，未开始真实运行。")
                    }
                  >
                    预演
                  </button>
                  <button
                    type="button"
                    className="wb-primary"
                    disabled={busy}
                    onClick={() => {
                      if (!window.confirm("确认运行工作流？每个步骤都受现有权限和验证合同约束。")) return;
                      void perform(
                        () => desktop.runWorkflow(workspace, item.id, false),
                        "工作流运行请求已受理；请检查相应 Run 的 Journal 状态。"
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
                    <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 6, marginBottom: 6 }}>
                      <span className="wb-badge is-accent">STEP {String(index + 1).padStart(2, "0")}</span>
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
          style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 10, marginBottom: 12 }}
        >
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <h3 style={{ margin: 0 }}>{id ? "编辑工作流" : "新工作流"}</h3>
            <span className="wb-badge is-muted">{steps.length} / 8 步</span>
          </div>
          <button type="button" className="wb-secondary" style={{ marginTop: 0, padding: "6px 12px" }} onClick={() => edit()}>
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
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap", marginBottom: 8 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                  <span className="wb-badge is-accent">STEP {String(i + 1).padStart(2, "0")}</span>
                  <select
                    aria-label={"第" + (i + 1) + "步运行模式"}
                    value={step.mode || ""}
                    onChange={(e) =>
                      setSteps((current) => current.map((x, j) => (i === j ? applyStepMode(x, e.target.value) : x)))
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
                  setSteps((current) => current.map((x, j) => (i === j ? { ...x, task: e.target.value } : x)))
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
            onClick={() => setSteps((current) => (current.length < 8 ? current.concat({ task: "" }) : current))}
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
                  id: id || undefined,
                  workspace,
                  name: name.trim(),
                  steps: steps.map(normalizeStepForSave),
                });
                edit();
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
