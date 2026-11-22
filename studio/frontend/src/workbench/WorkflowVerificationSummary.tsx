/**
 * [INPUT]: App 工作流报告及打开持久 Run 的回调。
 * [OUTPUT]: 逐标准结论、覆盖状态、未定论发现与证据入口。
 * [POS]: 工作流历史的只读视图，验收权留在 App。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 */
import type { WorkflowVerificationView } from "../desktop/types";

const verdictLabel: Record<string, string> = {
  passed: "通过",
  failed: "失败",
  unknown: "证据不足",
  waived: "已豁免",
  not_applicable: "不适用",
};

const coverageLabel: Record<string, string> = {
  selected: "待执行",
  pending: "待执行",
  running: "进行中",
  completed: "已覆盖",
  reused: "证据已复用",
  failed: "覆盖不足",
  skipped: "未执行",
};

/** 显示与同一工作区版本绑定的验证报告。 */
export function WorkflowVerificationSummary({
  report,
  criteria = [],
  onOpenRun,
}: {
  report: WorkflowVerificationView;
  criteria?: Array<{ id: string; statement: string }>;
  onOpenRun?: (runId: string) => void;
}) {
  const { evaluation, coverage, findings } = report;
  const evidence = [
    { label: "Worker", id: report.worker_run_id },
    { label: "程序验证", id: report.program_run_id },
    { label: "独立审查", id: report.review_run_id },
  ].filter((source): source is { label: string; id: string } => Boolean(source.id));

  return (
    <section aria-label="任务验收报告" className="wb-wf-verification">
      <div className="wb-wf-history-top">
        <strong>验收报告</strong>
        <span className={"wb-badge " + (evaluation.verdict === "passed" ? "is-ok" : "is-warn")}>
          {verdictLabel[evaluation.verdict] ?? evaluation.verdict}
        </span>
      </div>
      <ul>
        {evaluation.criteria.map((criterion) => (
          <li key={criterion.criterion_id}>
            {criteria.find(({ id }) => id === criterion.criterion_id)?.statement ?? criterion.criterion_id}
            {" · "}{verdictLabel[criterion.verdict] ?? criterion.verdict}
            {criterion.reason && <small> · {criterion.reason}</small>}
          </li>
        ))}
      </ul>
      <div className="wb-wf-history-sub">
        {coverage.entries.map((entry) => (
          <span key={entry.object_id} title={entry.reason}>
            {entry.object_id === "program" ? "程序验证" : "独立审查"} · {coverageLabel[entry.status] ?? entry.status}{" "}
          </span>
        ))}
      </div>
      {[...(findings.confirmed ?? []), ...(findings.inconclusive ?? [])].map((finding) => (
        <p key={finding.id}>
          {finding.status === "confirmed" ? "已确认" : "待复核"} · {finding.description}
        </p>
      ))}
      <div className="wb-wf-history-actions">
        {evidence.map(({ label, id }) =>
          onOpenRun ? (
            <button type="button" key={id} className="wb-ghost" onClick={() => onOpenRun(id)}>
              查看{label}
            </button>
          ) : (
            <span key={id} className="wb-hash-pill">{label} {id}</span>
          ),
        )}
      </div>
    </section>
  );
}
