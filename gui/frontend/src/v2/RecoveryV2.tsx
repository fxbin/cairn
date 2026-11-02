/**
 * Cairn 2.0 副作用对账：只接受用户明确填写的运行、操作、判定与证据。
 * 不会将未知副作用作为可自动重试的运行。
 */
import { useState } from "react";
import { desktop } from "../desktop/client";
import type { DomainEvent } from "../desktop/types";

type Verdict = "applied_success" | "applied_failure" | "not_applied" | "abort_run";
export function ReconciliationV2({ workspace, runId, events, onDone, onError }: {
  workspace: string;
  runId: string;
  events: DomainEvent[];
  onDone: () => void;
  onError: (message: string) => void;
}) {
  const [operationId, setOperationId] = useState("");
  const [verdict, setVerdict] = useState<Verdict>("abort_run");
  const [note, setNote] = useState("");
  const [evidence, setEvidence] = useState("");
  const [busy, setBusy] = useState(false);
  const candidates = Array.from(new Set(events.filter((event) => event.operation_id).map((event) => event.operation_id!)));

  const submit = async () => {
    if (!workspace || !runId || !operationId.trim() || !note.trim() || !evidence.trim() || busy) return;
    setBusy(true);
    try {
      await desktop.resolveReconcile({
        workspace,
        run_id: runId,
        operation_id: operationId.trim(),
        verdict,
        note: note.trim(),
        evidence: evidence.trim(),
      });
      onDone();
    } catch (error) {
      onError(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  return <section className="v2-reconcile" data-testid="v2-reconcile">
    <div className="v2-label">需要人工对账 · 不能直接重试</div>
    <strong>这次操作的副作用尚未确认</strong>
    <p>请先依据工作区真实状态核对操作是否发生，再提供独立证据及判定。对账不是授权，亦不会自动继续执行。</p>
    <div className="v2-form-grid">
      <label>操作 ID
        <input value={operationId} placeholder="operation_id" onChange={(e) => setOperationId(e.target.value)} list="v2-operations" aria-label="操作 ID" />
        <datalist id="v2-operations">{candidates.map((id) => <option key={id} value={id}/>)}</datalist>
      </label>
      <label>核实结论
        <select aria-label="对账结论" value={verdict} onChange={(e) => setVerdict(e.target.value as Verdict)}>
          <option value="abort_run">结束运行，不继续</option>
          <option value="applied_success">确认已执行成功</option>
          <option value="applied_failure">确认已执行但失败</option>
          <option value="not_applied">确认没有执行</option>
        </select>
      </label>
      <label>人工核查说明
        <textarea value={note} placeholder="说明你如何核实这次副作用…" onChange={(e) => setNote(e.target.value)} aria-label="人工核查说明" />
      </label>
      <label>证据引用
        <input value={evidence} placeholder="可核查的日志 / 路径 / Commit / 记录编号" onChange={(e) => setEvidence(e.target.value)} aria-label="证据引用" />
      </label>
    </div>
    <div className="v2-actions"><button type="button" className="v2-primary" disabled={!operationId.trim() || !note.trim() || !evidence.trim() || busy} onClick={() => void submit()}>{busy ? "正在提交…" : "提交人工对账"}</button></div>
  </section>;
}

export function AbortRunV2({ workspace, runId, onDone, onError }: {
  workspace: string; runId: string; onDone: () => void; onError: (message: string) => void;
}) {
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const abort = async () => {
    if (!reason.trim() || busy) return;
    setBusy(true);
    try {
      await desktop.abortRun({ workspace, run_id: runId, reason: reason.trim() });
      onDone();
    } catch (e) { onError(e instanceof Error ? e.message : String(e)); }
    finally { setBusy(false); }
  };
  return <div className="v2-abort" data-testid="v2-abort">
    <p>无法安全继续时，可明确终止当前运行并保留历史记录；不等于清除未确认的副作用。</p>
    <label>终止原因<input aria-label="终止原因" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="写明停止理由"/></label>
    <button disabled={!reason.trim() || busy} onClick={() => void abort()}>{busy ? "处理中…" : "终止当前运行"}</button>
  </div>;
}
