import type { RecentProject, RunHistoryView, WorkspaceStatus } from "../desktop/types";

/** 项目/会话侧栏：最近项目、工作区状态与运行列表。 */
export function Sidebar({
  workspace,
  recent,
  runs,
  selectedRunId,
  loading,
  workspacePath,
  onWorkspaceChange,
  onSelectRun,
  onRefresh,
}: {
  workspace: WorkspaceStatus | null;
  recent: RecentProject[];
  runs: RunHistoryView[];
  selectedRunId: string | null;
  loading: boolean;
  workspacePath: string;
  onWorkspaceChange: (path: string) => void;
  onSelectRun: (runId: string) => void;
  onRefresh: () => void;
}) {
  return (
    <aside className="flex w-72 shrink-0 flex-col gap-3 border-r border-[var(--c-border)] bg-[var(--c-bg-elevated)] p-3">
      <div>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-sm font-semibold">项目</h2>
          <button type="button" className="btn" onClick={onRefresh} disabled={loading || !workspacePath}>
            刷新
          </button>
        </div>
        <label className="muted mb-1 block text-xs" htmlFor="workspace-path">
          工作区路径
        </label>
        <input
          id="workspace-path"
          className="input"
          value={workspacePath}
          onChange={(event) => onWorkspaceChange(event.target.value)}
          placeholder="/path/to/workspace"
          spellCheck={false}
        />
        {workspace && (
          <div className="muted mt-2 space-y-1 text-xs">
            <div data-testid="workspace-path-label">{workspace.path}</div>
            <div>
              配置：{workspace.config_loaded ? "已加载" : "未加载"}
              {workspace.branch ? ` · 分支 ${workspace.branch}` : ""}
              {workspace.dirty ? " · 有未提交改动" : ""}
            </div>
            {workspace.config_error && (
              <div className="text-[var(--c-warning)]">配置错误：{workspace.config_error}</div>
            )}
          </div>
        )}
      </div>

      <div>
        <h3 className="muted mb-2 text-xs font-semibold uppercase tracking-wide">最近项目</h3>
        <ul className="space-y-1" data-testid="recent-projects">
          {recent.length === 0 && <li className="muted text-xs">暂无最近项目</li>}
          {recent.map((item) => (
            <li key={item.path}>
              <button
                type="button"
                className={`w-full rounded-md px-2 py-1 text-left text-xs hover:bg-[var(--c-bg-panel)] ${
                  item.path === workspacePath ? "bg-[var(--c-bg-panel)]" : ""
                }`}
                onClick={() => onWorkspaceChange(item.path)}
              >
                <div className="truncate">{item.name}</div>
                <div className="muted truncate text-[11px]">{item.path}</div>
              </button>
            </li>
          ))}
        </ul>
      </div>

      <div className="flex min-h-0 flex-1 flex-col">
        <h3 className="muted mb-2 text-xs font-semibold uppercase tracking-wide">会话 / 运行</h3>
        <div className="scroll-y min-h-0 flex-1 space-y-1" data-testid="run-list">
          {loading && <div className="muted text-xs">加载中…</div>}
          {!loading && runs.length === 0 && (
            <div className="muted text-xs" data-testid="empty-runs">
              暂无运行历史。在右侧输入任务以开始。
            </div>
          )}
          {!loading &&
            runs.map((run) => (
              <button
                key={run.run_id}
                type="button"
                className={`w-full rounded-md border px-2 py-1.5 text-left text-xs ${
                  run.run_id === selectedRunId
                    ? "border-[var(--c-accent)] bg-[var(--c-bg-panel)]"
                    : "border-transparent hover:bg-[var(--c-bg-panel)]"
                }`}
                onClick={() => onSelectRun(run.run_id)}
              >
                <div className="truncate font-medium">{run.title}</div>
                <div className="muted flex justify-between gap-2">
                  <span>{run.phase || run.state}</span>
                  <span>
                    {run.waiting_approval ? "待审批" : run.waiting_reconcile ? "待对账" : run.state}
                  </span>
                </div>
              </button>
            ))}
        </div>
      </div>
    </aside>
  );
}
