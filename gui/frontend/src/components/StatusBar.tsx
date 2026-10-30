import type { HealthResult } from "../desktop/types";

/**
 * 状态行：桥接健康的可见出口。
 *
 * 健康结果由桌面服务下发（桥接可用性与只读环境摘要）。应用此前取到结果却不渲染，
 * 桥接版本与连通状态没有出口，这里把它放进侧栏底部常驻可见的位置。
 * 未连通时如实说明，不拿版本号之类的占位内容冒充已连接。
 */
export function StatusBar({ health }: { health: HealthResult | null }) {
  const label = !health ? "桥接未连接" : health.ok ? `桥接 ${health.version}` : "桥接不可用";
  return (
    <div className="status-line" data-testid="bridge-status">
      {label}
    </div>
  );
}
