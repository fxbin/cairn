import type { HealthResult, WorkspaceStatus } from "../desktop/types";

/**
 * 底栏：健康状态、工作区与加载指示。
 *
 * 当前未接线：应用外壳没有底栏，App.tsx 虽然调用 desktop.health() 并存入
 * state.health，但界面从不渲染它，桥接版本与健康状态因此没有可见出口。
 * 保留本组件作为接线候选，接入位置（底栏常驻还是并入设置抽屉）属于产品范围决定，
 * 未确定前不擅自接线。
 */
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
