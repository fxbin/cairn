import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody } from "../desktop/types";

/** 设置面板：Provider/MCP/Skills 只读状态与主题偏好入口。 */
export function SettingsPanel({
  workspacePath,
  onError,
}: {
  workspacePath: string;
  onError: (error: ErrorBody) => void;
}) {
  const [summary, setSummary] = useState<string>("加载中…");

  useEffect(() => {
    if (!workspacePath) {
      setSummary("未选择工作区");
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const doctor = await desktop.doctor({ workspace: workspacePath });
        if (!cancelled) {
          setSummary(JSON.stringify(doctor, null, 2).slice(0, 2000));
        }
      } catch (error) {
        if (!cancelled) {
          setSummary("诊断不可用");
          onError(
            error && typeof error === "object" && "body" in error
              ? (error as { body: ErrorBody }).body
              : { code: "internal", message: String(error) },
          );
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [workspacePath, onError]);

  return (
    <div className="scroll-y min-h-0 flex-1 text-xs" data-testid="settings-panel">
      <div className="muted mb-2">生效配置、Provider、预算与 Skills 状态（只读投影）。</div>
      <pre className="whitespace-pre-wrap rounded-md bg-[var(--c-bg)] p-2">{summary}</pre>
    </div>
  );
}
