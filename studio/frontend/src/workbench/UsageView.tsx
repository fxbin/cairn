/**
 * 用量与成本观测面板：从当前项目真实 Journal / Run Evidence 聚合 Token 用量。
 * 严格遵守无可靠渠道报价不伪造账单的契约，费用缺失时诚实显示为未知。
 * Author: fxbin
 */
import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { EvidenceView, RunHistoryView } from "../desktop/types";

type UsageRow = {
  run: RunHistoryView;
  evidence: EvidenceView | null;
  error?: string;
};

/**
 * 计算单条运行证据的输入输出 Token 合计与可视化比例百分比。
 */
function computeTokenRatio(inputTokens: number, outputTokens: number): {
  total: number;
  inputPct: number;
  outputPct: number;
} {
  const safeIn = Math.max(0, inputTokens || 0);
  const safeOut = Math.max(0, outputTokens || 0);
  const total = safeIn + safeOut;
  if (total === 0) {
    return { total: 0, inputPct: 0, outputPct: 0 };
  }
  const inputPct = Math.round((safeIn / total) * 100);
  return { total, inputPct, outputPct: Math.max(0, 100 - inputPct) };
}

/**
 * 渲染分组卡片风格的用量汇总与单次 Run 明细面板，保留估算警示与未知费用诚实展示契约。
 */
export default function UsageView({
  workspace,
  notify,
}: {
  workspace: string;
  notify: (message: string) => void;
}) {
  const [rows, setRows] = useState<UsageRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [limit, setLimit] = useState(20);

  useEffect(() => {
    if (!workspace) {
      setRows([]);
      setLoading(false);
      return;
    }
    let active = true;
    setRows([]);
    setLoading(true);
    const run = async () => {
      try {
        const all = await desktop.listRuns({ workspace });
        const chosen = all.slice(0, limit);
        const loaded = await Promise.all(
          chosen.map(async (item) => {
            try {
              const evidence = await desktop.evidence({ workspace, run_id: item.run_id });
              return { run: item, evidence } as UsageRow;
            } catch (error) {
              return {
                run: item,
                evidence: null,
                error: error instanceof Error ? error.message : String(error),
              } as UsageRow;
            }
          })
        );
        if (active) setRows(loaded);
      } catch (error) {
        if (active) notify(error instanceof Error ? error.message : String(error));
      } finally {
        if (active) setLoading(false);
      }
    };
    void run();
    return () => {
      active = false;
    };
  }, [workspace, limit, notify]);

  const measured = rows.filter((x) => x.evidence !== null);
  const estimated = measured.some((x) => x.evidence?.metrics.usage_estimated);
  const hasError = rows.some((x) => Boolean(x.error));
  const input = measured.reduce((sum, x) => sum + (x.evidence?.metrics.input_tokens || 0), 0);
  const output = measured.reduce((sum, x) => sum + (x.evidence?.metrics.output_tokens || 0), 0);

  return (
    <div className="wb-usage" data-testid="usage-view">
      <section className="wb-settings-section">
        <div
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            gap: 10,
            flexWrap: "wrap",
          }}
        >
          <h3 className="wb-settings-section-title" style={{ margin: 0 }}>
            用量统计摘要
          </h3>
          <div style={{ display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
            {estimated && <span className="wb-badge is-warn">含估算用量</span>}
            <span className="wb-badge is-muted">无可靠报价时费用显示为未知</span>
          </div>
        </div>

        <div className="wb-usage-summary" style={{ margin: "10px 0 0" }}>
          <div>
            <span>已读取 Run</span>
            <strong>{measured.length}</strong>
          </div>
          <div>
            <span>输入 Token</span>
            <strong>{input.toLocaleString()}</strong>
          </div>
          <div>
            <span>输出 Token</span>
            <strong>{output.toLocaleString()}</strong>
          </div>
          <div>
            <span>费用</span>
            <strong>未知</strong>
          </div>
        </div>

        {(estimated || hasError) && (
          <div className="wb-settings-card" style={{ marginTop: 10 }}>
            {estimated && (
              <div className="wb-setting-row">
                <div className="wb-setting-meta">
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>含估算用量警示</strong>
                    <span className="wb-badge is-warn">估算值</span>
                  </div>
                  <p>部分 Run 的用量是估算值；总和不能解释为 Provider 账单。</p>
                </div>
              </div>
            )}
            {hasError && (
              <div className="wb-setting-row">
                <div className="wb-setting-meta">
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>部分运行缺少可读证据</strong>
                    <span className="wb-badge is-muted">未计入合计</span>
                  </div>
                  <p>部分运行未能读取证据，未计入上方合计。</p>
                </div>
              </div>
            )}
          </div>
        )}
      </section>

      <section className="wb-settings-section">
        <div
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            gap: 10,
            flexWrap: "wrap",
          }}
        >
          <h3 className="wb-settings-section-title" style={{ margin: 0 }}>
            单次运行明细
          </h3>
          {loading && <span className="wb-badge is-muted">读取中…</span>}
        </div>

        <div className="wb-settings-card" style={{ marginTop: 10 }}>
          {rows.map((x) => {
            const ev = x.evidence;
            const ratio = ev
              ? computeTokenRatio(ev.metrics.input_tokens, ev.metrics.output_tokens)
              : null;
            return (
              <div
                className="wb-setting-row"
                key={x.run.run_id}
                style={{ flexWrap: "wrap", gap: 12 }}
              >
                <div className="wb-setting-meta" style={{ minWidth: 200, flex: "1 1 240px" }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>{x.run.title || x.run.run_id}</strong>
                    {ev && (
                      <span className={"wb-badge " + (ev.trusted ? "is-ok" : "is-muted")}>
                        {ev.trust_label || (ev.trusted ? "✓ 验证可信" : "未确认验证")}
                      </span>
                    )}
                    <span className="wb-setting-key">{x.run.run_id}</span>
                  </div>
                  <p>
                    {x.run.phase_label || x.run.state}
                    {ev
                      ? ` · ${ev.metrics.turns} 回合 · ${ev.metrics.tool_calls} 次工具调用`
                      : ""}
                  </p>
                </div>

                {ev && ratio ? (
                  <div style={{ minWidth: 230, flex: "0 1 300px", marginLeft: "auto" }}>
                    <div
                      style={{
                        display: "flex",
                        alignItems: "center",
                        justifyContent: "flex-end",
                        gap: 8,
                        flexWrap: "wrap",
                      }}
                    >
                      <strong style={{ fontSize: 12.5 }}>
                        {ratio.total.toLocaleString()} tokens
                      </strong>
                      <span
                        className={
                          "wb-badge " + (ev.metrics.usage_estimated ? "is-warn" : "is-ok")
                        }
                      >
                        {ev.metrics.usage_estimated ? "估算" : "实测"}
                      </span>
                    </div>
                    <div
                      role="img"
                      aria-label={`输入 ${ratio.inputPct}%，输出 ${ratio.outputPct}%`}
                      style={{
                        display: "flex",
                        height: 6,
                        width: "100%",
                        borderRadius: 999,
                        overflow: "hidden",
                        background: "var(--wb-line)",
                        margin: "6px 0 4px",
                      }}
                    >
                      <div
                        style={{
                          width: `${ratio.inputPct}%`,
                          background: "var(--wb-action)",
                        }}
                      />
                      <div
                        style={{
                          width: `${ratio.outputPct}%`,
                          background: "var(--wb-moss, var(--wb-success))",
                        }}
                      />
                    </div>
                    <div
                      style={{
                        display: "flex",
                        justifyContent: "space-between",
                        fontSize: 11,
                        color: "var(--wb-muted)",
                      }}
                    >
                      <span>
                        输入 {ev.metrics.input_tokens.toLocaleString()} ({ratio.inputPct}%)
                      </span>
                      <span>
                        输出 {ev.metrics.output_tokens.toLocaleString()} ({ratio.outputPct}%)
                      </span>
                    </div>
                  </div>
                ) : (
                  <span className="wb-badge is-muted">暂无可核实的用量</span>
                )}
              </div>
            );
          })}

          {!loading && !rows.length && (
            <div className="wb-setting-row">
              <div className="wb-setting-meta">
                <strong>暂无可展示的运行记录</strong>
                <p>当前项目本地 Event Journal 中尚未产生可读取的 Run 证据。</p>
              </div>
            </div>
          )}
        </div>

        <div className="wb-actions" style={{ marginTop: 12 }}>
          <button
            type="button"
            className="wb-secondary"
            onClick={() => setLimit((n) => n + 20)}
            disabled={loading}
          >
            多读取 20 条 Run
          </button>
        </div>

        <p className="wb-settings-footnote">
          数据源：本地 SQLite Event Journal 的单次运行证据。价格、缓存优惠和模型计费规则缺失时，费用诚实显示为未知，绝不伪造账单。
        </p>
      </section>
    </div>
  );
}
