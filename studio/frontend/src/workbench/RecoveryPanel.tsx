/**
 * Cairn Studio GUI 副作用对账与运行安全收尾面板：只接受用户明确填写的运行、操作、判定与证据。
 * 绝不将未知副作用作为可自动重试的运行。
 * Author: fxbin
 */
import { useMemo, useState } from "react";
import { desktop } from "../desktop/client";
import type { DomainEvent } from "../desktop/types";

type Verdict = "applied_success" | "applied_failure" | "not_applied" | "abort_run";

/**
 * 从运行事件流中提取去重后的待核实候选操作标识列表。
 * 优先保留出现过 operation_started 且缺少明确终态回执的操作顺序。
 */
function extractCandidateOperations(events: DomainEvent[]): string[] {
  const seen = new Set<string>();
  const ordered: string[] = [];
  for (const event of events) {
    if (!event.operation_id) continue;
    if (!seen.has(event.operation_id)) {
      seen.add(event.operation_id);
      ordered.push(event.operation_id);
    }
  }
  return ordered;
}

/**
 * 人工副作用对账面板：要求明确提供操作 ID、对账结论、核查说明与独立证据引用。
 */
export function ReconciliationPanel({
  workspace,
  runId,
  events,
  onDone,
  onError,
}: {
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

  const candidates = useMemo(() => extractCandidateOperations(events), [events]);

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

  return (
    <section className="wb-reconcile" data-testid="reconcile-panel">
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap" }}>
        <div className="wb-label">需要人工对账 · 不能直接重试</div>
        <span className="wb-badge is-warn">waiting_reconcile · 严禁盲目重试</span>
      </div>
      <strong>这次操作的副作用尚未确认</strong>
      <p>
        Journal 已记录 <code>operation_started</code> 但缺少完成回执。请先依据工作区真实状态核对操作是否已发生，再提供独立可核查证据及人工判定；对账不是授权，亦不会自动继续执行。
      </p>
      {candidates.length > 0 && (
        <div className="wb-op-chips" role="group" aria-label="快捷候选操作" style={{ display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap", marginTop: 10 }}>
          <span className="wb-label">候选操作快捷填入：</span>
          {candidates.map((id) => (
            <button
              key={id}
              type="button"
              aria-label={"选择候选操作 " + id}
              className={"wb-badge " + (operationId === id ? "is-accent" : "is-muted")}
              onClick={() => setOperationId(id)}
            >
              #{id}
            </button>
          ))}
        </div>
      )}
      <div className="wb-form-grid">
        <label>
          操作 ID
          <input
            value={operationId}
            placeholder="operation_id"
            onChange={(e) => setOperationId(e.target.value)}
            list="wb-operations"
            aria-label="操作 ID"
          />
          <datalist id="wb-operations">
            {candidates.map((id) => (
              <option key={id} value={id} />
            ))}
          </datalist>
        </label>
        <label>
          核实结论
          <select aria-label="对账结论" value={verdict} onChange={(e) => setVerdict(e.target.value as Verdict)}>
            <option value="abort_run">结束运行，不继续</option>
            <option value="applied_success">确认已执行成功</option>
            <option value="applied_failure">确认已执行但失败</option>
            <option value="not_applied">确认没有执行</option>
          </select>
        </label>
        <label>
          人工核查说明
          <textarea
            value={note}
            placeholder="说明你如何核实这次副作用…"
            onChange={(e) => setNote(e.target.value)}
            aria-label="人工核查说明"
          />
        </label>
        <label>
          证据引用
          <input
            value={evidence}
            placeholder="可核查的日志 / 路径 / Commit / 记录编号"
            onChange={(e) => setEvidence(e.target.value)}
            aria-label="证据引用"
          />
        </label>
      </div>
      <div className="wb-actions">
        <button
          type="button"
          className="wb-primary"
          disabled={!operationId.trim() || !note.trim() || !evidence.trim() || busy}
          onClick={() => void submit()}
        >
          {busy ? "正在提交…" : "提交人工对账"}
        </button>
      </div>
    </section>
  );
}

/**
 * 运行安全收尾面板：当无法安全继续时明确终止当前运行并保留完整审计轨迹。
 */
export function AbortRunPanel({
  workspace,
  runId,
  onDone,
  onError,
}: {
  workspace: string;
  runId: string;
  onDone: () => void;
  onError: (message: string) => void;
}) {
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);

  const abort = async () => {
    if (!reason.trim() || busy) return;
    setBusy(true);
    try {
      await desktop.abortRun({ workspace, run_id: runId, reason: reason.trim() });
      onDone();
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="wb-abort" data-testid="abort-panel">
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap", marginBottom: 4 }}>
        <span className="wb-label">安全终止收尾</span>
        <span className="wb-badge is-muted">保留 Journal 审计记录</span>
      </div>
      <p>无法安全继续时，可明确终止当前运行并保留历史记录；终止运行不等于清除未确认的副作用。</p>
      <label>
        终止原因
        <input
          aria-label="终止原因"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          placeholder="写明停止理由"
        />
      </label>
      <button type="button" disabled={!reason.trim() || busy} onClick={() => void abort()}>
        {busy ? "处理中…" : "终止当前运行"}
      </button>
    </div>
  );
}
