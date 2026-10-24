/**
 * 打开位置菜单：探测宿主目标后打开工作区或相对路径。
 * 与 Finder / 终端 / VS Code / Cursor / IDEA / Ghostty 对齐。
 */
import { useEffect, useRef, useState } from "react";
import { desktop } from "../desktop/client";

export function OpenLocationMenu({
  workspace,
  relative,
  label = "打开位置",
}: {
  workspace: string;
  relative?: string;
  label?: string;
}) {
  const [open, setOpen] = useState(false);
  const [targets, setTargets] = useState<Array<{ id: string; label: string; available: boolean }>>([]);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open || targets.length) return;
    void desktop.listOpenLocations(workspace).then(setTargets).catch(() => setTargets([]));
  }, [open, targets.length, workspace]);

  useEffect(() => {
    if (!open) return;
    const onDoc = (event: MouseEvent) => {
      if (ref.current && !ref.current.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  return (
    <div ref={ref} style={{ position: "relative", display: "inline-block" }}>
      <button type="button" className="btn-sm" data-testid="open-location" onClick={() => setOpen((v) => !v)}>
        {label} ▾
      </button>
      {open && (
        <div
          data-testid="open-location-menu"
          style={{
            position: "absolute",
            right: 0,
            top: "100%",
            zIndex: 20,
            minWidth: 160,
            marginTop: 4,
            background: "var(--ds-bg-2)",
            border: "1px solid var(--c-border-strong)",
            borderRadius: 8,
            boxShadow: "var(--shadow)",
            padding: 4,
          }}
        >
          {targets.map((target) => (
            <button
              key={target.id}
              type="button"
              className="btn-sm"
              disabled={!target.available}
              style={{ width: "100%", justifyContent: "flex-start", border: "none", background: "transparent" }}
              data-testid={`open-location-${target.id}`}
              onClick={() => {
                setOpen(false);
                void desktop.openInLocation(workspace, relative || ".", target.id).catch(() => undefined);
              }}
            >
              {target.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
