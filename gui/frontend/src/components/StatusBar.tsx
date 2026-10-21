import type { HealthResult, WorkspaceStatus } from "../desktop/types";

/** 底栏：健康状态、工作区与加载指示。 */
export function StatusBar({
  health,
  workspace,
  loading,
}: {
  health: HealthResult | null;
  workspace: WorkspaceStatus | null;
  loading: boolean;
}) {
  return (
    <footer className="flex items-center gap-3 border-t border-[var(--c-border)] px-3 py-1.5 text-xs">
      <span className="font-medium">Cairn</span>
      <span className="muted">{health ? `桥接 ${health.version}` : "桥接未连接"}</span>
      {workspace && (
        <span className="muted truncate">
          {workspace.branch ? `${workspace.branch} · ` : ""}
          {workspace.path}
        </span>
      )}
      <span className="ml-auto muted">{loading ? "加载中…" : "就绪"}</span>
    </footer>
  );
}
