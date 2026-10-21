import type { ErrorBody } from "../desktop/types";

/** 全局错误横幅：可定位、可关闭，并展示稳定错误码。 */
export function ErrorBanner({
  error,
  onDismiss,
}: {
  error: ErrorBody | null;
  onDismiss: () => void;
}) {
  if (!error) {
    return null;
  }
  return (
    <div
      role="alert"
      className="flex items-start justify-between gap-3 border-b border-[var(--c-danger)] bg-[#2a1b1b] px-4 py-2 text-sm"
      data-testid="error-banner"
    >
      <div>
        <div className="font-medium text-[var(--c-danger)]">{error.message}</div>
        <div className="muted mt-0.5 text-xs">
          错误码 {error.code}
          {error.run_id ? ` · 运行 ${error.run_id}` : ""}
          {error.operation_id ? ` · 操作 ${error.operation_id}` : ""}
        </div>
      </div>
      <button type="button" className="btn" onClick={onDismiss} aria-label="关闭错误提示">
        关闭
      </button>
    </div>
  );
}
