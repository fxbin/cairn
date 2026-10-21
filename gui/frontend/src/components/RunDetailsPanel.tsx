import type { ErrorBody, RunHistoryView, StreamEvent } from "../desktop/types";
import type { PanelId } from "./panelTypes";
import { SettingsPanel } from "./SettingsPanel";

/** 运行详情/恢复区：流式事件时间线与设置入口。 */
export function RunDetailsPanel({
  workspacePath,
  selectedRun,
  streamEvents,
  panel,
  onPanel,
  onError,
}: {
  workspacePath: string;
  selectedRun: RunHistoryView | null;
  streamEvents: StreamEvent[];
  panel: PanelId;
  onPanel: (panel: PanelId) => void;
  onError: (error: ErrorBody) => void;
}) {
  return (
    <section className="panel flex min-h-0 flex-col p-3">
      <div className="mb-2 flex items-center gap-2">
        <h2 className="text-sm font-semibold">运行详情</h2>
        <div className="ml-auto flex gap-1">
          {(
            [
              ["run", "运行"],
              ["files", "文件"],
              ["settings", "设置"],
            ] as const
          ).map(([id, label]) => (
            <button
              key={id}
              type="button"
              className={`btn ${panel === id ? "btn-primary" : ""}`}
              onClick={() => onPanel(id)}
            >
              {label}
            </button>
          ))}
        </div>
      </div>
      {panel === "settings" && (
        <SettingsPanel workspacePath={workspacePath} onError={onError} />
      )}
      {panel !== "settings" && (
        <>
          {!selectedRun && (
            <div className="muted text-xs" data-testid="run-empty">
              选择左侧运行以查看事件时间线。
            </div>
          )}
          {selectedRun && (
            <div className="muted text-xs" data-testid="run-selected">
              运行 {selectedRun.run_id} · {selectedRun.phase || selectedRun.state}
            </div>
          )}
          <div className="scroll-y mt-2 min-h-0 flex-1 space-y-1" data-testid="stream-events">
            {streamEvents.length === 0 && <div className="muted text-xs">暂无增量事件。</div>}
            {streamEvents.map((event, index) => (
              <div
                key={`${event.run_id || "run"}-${event.seq ?? index}`}
                className="rounded-md border border-[var(--c-border)] px-2 py-1 text-xs"
              >
                <div className="flex justify-between gap-2">
                  <span className="font-medium">{event.kind}</span>
                  <span className="muted">
                    {event.run_id || "—"} · seq {event.seq ?? "—"}
                  </span>
                </div>
                {event.event && (
                  <div className="muted">
                    {event.event.type}
                    {event.event.operation_id ? ` · ${event.event.operation_id}` : ""}
                  </div>
                )}
                {event.status?.response_preview && (
                  <div className="mt-1 line-clamp-2">{event.status.response_preview}</div>
                )}
              </div>
            ))}
          </div>
        </>
      )}
    </section>
  );
}
