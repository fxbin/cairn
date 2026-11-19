/**
 * [INPUT]: App 实例、活动审批、对账与返工投影。
 * [OUTPUT]: 从显式用户输入提交实例控制请求。
 * [POS]: 工作流目录使用同一请求和权威状态，界面不执行步骤。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 * Author: fxbin
 */
import { useState } from "react";
import type { WorkflowAction, WorkflowControlRequest, WorkflowInstanceView } from "../desktop/types";
import { workflowPhaseLabel } from "./workflowConversation";

export type WorkflowControl = (request: WorkflowControlRequest) => Promise<void>;

export function WorkflowInstanceControls({
  workspace,
  instance,
  onControl,
  onOpenRun,
}: {
  workspace: string;
  instance: WorkflowInstanceView;
  onControl: WorkflowControl;
  onOpenRun?: (runID: string) => void;
}) {
  const [reason, setReason] = useState("");
  const [target, setTarget] = useState("");
  const [verdict, setVerdict] = useState("");
  const [evidence, setEvidence] = useState("");
  const [busy, setBusy] = useState(false);
  const node = instance.steps[instance.resume_index];
  const approval = instance.pending_approval;
  const reconcile = instance.pending_reconcile;
  const gate = !instance.paused && instance.phase === "waiting_human";
  const canDecide = gate || (!instance.paused && Boolean(approval));
  const retryTargets = instance.retry_targets ?? [];
  const retryTarget = retryTargets.includes(target) ? target : retryTargets[0];
  const canControl = Boolean(instance.sequence && !instance.finished && node && instance.phase !== "binding_unconfirmed");

  const submit = async (action: WorkflowAction) => {
    if (!instance.sequence || busy || !canControl || (action !== "advance" && !reason.trim())) return;
    const request: WorkflowControlRequest = {
      workspace, workflow_id: instance.workflow_id, sequence: instance.sequence, action,
      reason: reason.trim() || undefined,
    };
    if (action === "approve" || action === "reject") {
      request.step_id = node.id;
      request.run_id = approval?.run_id;
      request.operation_id = approval?.operation_id;
      request.fingerprint = approval?.fingerprint;
    } else if (action === "retry") {
      request.step_id = retryTarget;
    } else if (action === "reconcile" && reconcile) {
      request.step_id = node.id;
      request.run_id = reconcile.run_id;
      request.operation_id = reconcile.operation_id;
      request.verdict = verdict;
      request.evidence = evidence.trim() || undefined;
    }
    setBusy(true);
    try {
      await onControl(request);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="wb-wf-attempts">
      <details>
        <summary>阶段尝试</summary>
        <ol>
          {instance.steps.map((step) => (
            <li key={step.id}>
              <strong>{step.id}</strong>
              {(step.attempts ?? []).map((attempt) => (
                <div key={attempt.number}>
                  第 {attempt.number} 次 · {workflowPhaseLabel(attempt.state === "succeeded" ? "completed" : attempt.state)}
                  {attempt.error_class && <span> · {attempt.error_class}</span>}
                  {attempt.decision && <span> · {attempt.decision === "approve" ? "通过" : "拒绝"}</span>}
                  {attempt.run_id && onOpenRun && (
                    <button type="button" className="wb-ghost" onClick={() => onOpenRun(attempt.run_id!)}>
                      查看运行 {attempt.run_id}
                    </button>
                  )}
                </div>
              ))}
            </li>
          ))}
        </ol>
      </details>
      {instance.finish_reason && <p>{instance.finish_reason}</p>}
      {canControl && (
        <fieldset disabled={busy} aria-label="实例操作">
          {canDecide && <p>{approval?.summary || `确认人工门「${node.id}」`}</p>}
          {reconcile && <p>{reconcile.tool_name} · {reconcile.operation_id}</p>}
          <label>操作原因 <input value={reason} onChange={(event) => setReason(event.target.value)} maxLength={256} /></label>
          <div className="wb-wf-history-actions">
            {canDecide && (
              <>
                <button type="button" disabled={!reason.trim()} onClick={() => void submit("approve")}>通过</button>
                <button type="button" disabled={!reason.trim()} onClick={() => void submit("reject")}>拒绝</button>
              </>
            )}
            {retryTarget && (
              <>
                <label>返工起点
                  <select value={retryTarget} onChange={(event) => setTarget(event.target.value)}>
                    {retryTargets.map((item) => <option key={item}>{item}</option>)}
                  </select>
                </label>
                <button type="button" disabled={!reason.trim()} onClick={() => void submit("retry")}>确认返工</button>
              </>
            )}
            {reconcile && (
              <>
                <label>对账结论
                  <select value={verdict} onChange={(event) => setVerdict(event.target.value)}>
                    <option value="">选择结论</option>
                    {!instance.paused && (
                      <>
                        <option value="applied_success">已生效并成功</option>
                        <option value="applied_failure">已生效但失败</option>
                        <option value="not_applied">未生效</option>
                      </>
                    )}
                    <option value="abort_run">终止运行</option>
                  </select>
                </label>
                <label>对账证据 <input value={evidence} onChange={(event) => setEvidence(event.target.value)} /></label>
                <button type="button" disabled={!reason.trim() || !verdict || (instance.paused && verdict !== "abort_run") || (reconcile.side_effect_class === "process" && !evidence.trim())} onClick={() => void submit("reconcile")}>提交对账</button>
              </>
            )}
            {instance.paused ? (
              <button type="button" disabled={!reason.trim()} onClick={() => void submit("resume")}>解除暂停</button>
            ) : (
              <>
                {!canDecide && !reconcile && !retryTarget && <button type="button" onClick={() => void submit("advance")}>继续推进</button>}
                <button type="button" disabled={!reason.trim()} onClick={() => void submit("pause")}>暂停</button>
              </>
            )}
            {!reconcile && <button type="button" disabled={!reason.trim()} onClick={() => void submit("cancel")}>取消实例</button>}
          </div>
        </fieldset>
      )}
    </div>
  );
}
