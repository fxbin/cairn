/**
 * INPUT: 会话态势（ChangeSet、EvidenceView、Timeline、SessionTree、队列计数）与多标签交互回调。
 * OUTPUT: 可拖拽宽度的右侧多标签伴随工作台（启动台、变更审查、项目文件、单文件标签、证据、子智能体与队列）。
 * POS: studio/frontend/src/workbench 右侧主伴随工作台容器，只读消费 desktop 桥接与 Journal 投影。
 *
 * Author: fxbin
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { desktop } from "../desktop/client";
import type {
  ChangeSet,
  EvidenceView,
  FileNode,
  FilePreview,
  RunHistoryView,
  SessionTreeView,
} from "../desktop/types";
import type {
  ContextCompactionView,
  SubagentView,
  TimelineItem,
  TimelineKind,
} from "../desktop/timeline";
import ChangesReviewPanel from "./ChangesReviewPanel";
import FilesPanel from "./FilesPanel";
import OpenLocationIcon from "./OpenLocationIcon";
import { QueuePanel } from "./QueuePanel";

/**
 * SidecarTabKind 定义右侧工作台支持的标签页类型。
 */
export type SidecarTabKind =
  | "workbench"
  | "changes"
  | "files"
  | "file"
  | "evidence"
  | "subagents"
  | "queue";

/**
 * SidecarTab 描述右侧工作台中单个打开标签页的标识与标题元数据。
 */
export interface SidecarTab {
  id: string;
  kind: SidecarTabKind;
  title: string;
  filePath?: string;
}

/**
 * OpenLocationTarget 描述工作区可用的外部打开目标。
 */
export interface OpenLocationTarget {
  id: string;
  label: string;
  available: boolean;
}

/**
 * FlatFileItem 描述扁平化后的工作区文件索引项，供启动台快速检索跳转。
 */
interface FlatFileItem {
  name: string;
  relative: string;
  dir: string;
  size?: number;
}

const SIDECAR_WIDTH_STORAGE_KEY = "cairn.gui.sidecarWidth";
const DEFAULT_SIDECAR_WIDTH = 500;
const DEFAULT_REVIEW_SIDECAR_WIDTH = 840;
const MIN_SIDECAR_WIDTH = 360;
const MAX_SIDECAR_WIDTH = 1120;

/**
 * 读取本地持久化的右侧工作台宽度像素值，未自定义时返回 null 以便按标签类型自适应默认宽度。
 */
function readSavedSidecarWidth(): number | null {
  if (typeof window === "undefined") return null;
  try {
    const rawStr = localStorage.getItem(SIDECAR_WIDTH_STORAGE_KEY);
    if (!rawStr) return null;
    const raw = Number(rawStr);
    if (Number.isFinite(raw) && raw >= MIN_SIDECAR_WIDTH && raw <= MAX_SIDECAR_WIDTH) {
      return Math.round(raw);
    }
  } catch {
    // 忽略本地存储受限场景。
  }
  return null;
}

/**
 * 将字节数格式化为紧凑可读体积字符串。
 */
function formatBytes(bytes?: number): string {
  if (bytes === undefined || bytes < 0 || !Number.isFinite(bytes)) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * 递归扁平化工作区文件树，仅收集文件节点并限制最大扫描条数以保证响应流畅。
 */
function flattenFileTree(node: FileNode | null, limit = 1500): FlatFileItem[] {
  if (!node) return [];
  const items: FlatFileItem[] = [];
  const stack: FileNode[] = [node];
  while (stack.length > 0 && items.length < limit) {
    const current = stack.pop()!;
    if (current.kind === "dir") {
      const children = current.children || [];
      for (let i = children.length - 1; i >= 0; i -= 1) {
        stack.push(children[i]);
      }
      continue;
    }
    if (current.relative && current.relative !== ".") {
      const slashIdx = current.relative.lastIndexOf("/");
      items.push({
        name: current.name,
        relative: current.relative,
        dir: slashIdx > 0 ? current.relative.slice(0, slashIdx) : "",
        size: current.size,
      });
    }
  }
  return items;
}

/**
 * 构建内置功能面板对应的标准标签页对象。
 */
export function buildBuiltinSidecarTab(kind: Exclude<SidecarTabKind, "file">): SidecarTab {
  switch (kind) {
    case "workbench":
      return { id: "workbench", kind: "workbench", title: "从这里开始" };
    case "changes":
      return { id: "changes", kind: "changes", title: "变更" };
    case "files":
      return { id: "files", kind: "files", title: "项目文件" };
    case "evidence":
      return { id: "evidence", kind: "evidence", title: "验证与证据" };
    case "subagents":
      return { id: "subagents", kind: "subagents", title: "子智能体" };
    case "queue":
      return { id: "queue", kind: "queue", title: "候选队列" };
  }
}

/**
 * 构建单文件独立只读预览标签页对象。
 */
export function buildFileSidecarTab(relativePath: string): SidecarTab {
  const clean = relativePath.trim();
  const base = clean.split("/").filter(Boolean).pop() || clean;
  return {
    id: `file:${clean}`,
    kind: "file",
    title: base,
    filePath: clean,
  };
}

/**
 * 渲染标签页与启动台卡片共用的轻量线框图标。
 */
function SidecarKindIcon({ kind }: { kind: SidecarTabKind | "fork" | "terminal" }) {
  switch (kind) {
    case "workbench":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <rect x="2" y="2.5" width="5" height="5" rx="1.2" />
          <rect x="9" y="2.5" width="5" height="5" rx="1.2" />
          <rect x="2" y="9" width="12" height="4.5" rx="1.2" />
        </svg>
      );
    case "changes":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <rect x="2.5" y="2.2" width="11" height="11.6" rx="1.8" />
          <path d="M6.2 6h3.6M8 4.2v3.6M6.2 10.6h3.6" />
        </svg>
      );
    case "files":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M2.3 4.3c0-.7.5-1.3 1.2-1.3h2.6l1.3 1.5h5.1c.7 0 1.2.5 1.2 1.2v6.1c0 .7-.5 1.2-1.2 1.2H3.5c-.7 0-1.2-.5-1.2-1.2V4.3Z" />
        </svg>
      );
    case "file":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M4 2.2h5.2L12.5 5.5v8.3H4a1 1 0 0 1-1-1V3.2a1 1 0 0 1 1-1Z" />
          <path d="M9 2.3v3.3h3.3" />
        </svg>
      );
    case "evidence":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M8 2.2 12.8 4v3.8c0 3.1-2.1 5.1-4.8 6-2.7-.9-4.8-2.9-4.8-6V4L8 2.2Z" />
          <path d="m6 8.1 1.4 1.4L10.3 6.6" />
        </svg>
      );
    case "subagents":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <circle cx="8" cy="3.5" r="1.6" />
          <circle cx="4" cy="12" r="1.6" />
          <circle cx="12" cy="12" r="1.6" />
          <path d="M8 5.1v2.6M8 7.7 4.6 10.5M8 7.7l3.4 2.8" />
        </svg>
      );
    case "fork":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M3.2 3.5h9.6a1.4 1.4 0 0 1 1.4 1.4v5.2a1.4 1.4 0 0 1-1.4 1.4H6.4L3.2 13.8V11.5a1.4 1.4 0 0 1-1.4-1.4V4.9a1.4 1.4 0 0 1 1.4-1.4Z" />
          <path d="M6 7.5h4" />
        </svg>
      );
    case "terminal":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <rect x="2" y="2.6" width="12" height="10.8" rx="1.8" />
          <path d="m5 6.2 2.2 1.8L5 9.8M8.5 10h2.6" />
        </svg>
      );
    case "queue":
      return (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M2.5 4.2h11M2.5 8h11M2.5 11.8h7" />
          <path d="m11.5 10.3 2 1.5-2 1.5" />
        </svg>
      );
  }
}

/**
 * 单文件独立标签只读预览视图：展示路径、元信息、行号代码与外部编辑器打开入口。
 */
function SingleFileTabView({
  workspace,
  relativePath,
  openLocations,
  notify,
}: {
  workspace: string;
  relativePath: string;
  openLocations: OpenLocationTarget[];
  notify: (message: string) => void;
}) {
  const [preview, setPreview] = useState<FilePreview | null>(null);
  const [previewError, setPreviewError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onDocDown = (event: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) {
        setMenuOpen(false);
      }
    };
    document.addEventListener("mousedown", onDocDown);
    return () => document.removeEventListener("mousedown", onDocDown);
  }, []);

  useEffect(() => {
    let active = true;
    setPreview(null);
    setPreviewError("");
    setLoading(true);
    if (typeof desktop.previewFile !== "function") {
      setLoading(false);
      return () => {
        active = false;
      };
    }
    void desktop
      .previewFile(workspace, relativePath)
      .then((result) => {
        if (active) setPreview(result);
      })
      .catch((err) => {
        if (!active) return;
        const msg = err instanceof Error ? err.message : String(err);
        if (msg.includes("sensitive") || msg.includes("ignored")) {
          setPreviewError("该文件属于敏感凭据或受忽略路径，按安全边界禁止在工作台直接展示明文。");
        } else {
          setPreviewError(msg);
          notify(msg);
        }
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [workspace, relativePath, notify]);

  const lines = useMemo(() => {
    if (!preview || preview.binary || !preview.text) return [];
    return preview.text.split("\n");
  }, [preview]);

  const handleOpenExternal = async (locationId?: string) => {
    if (busy) return;
    setBusy(true);
    setMenuOpen(false);
    try {
      if (locationId && typeof desktop.openInLocation === "function") {
        await desktop.openInLocation(workspace, relativePath, locationId);
      } else if (typeof desktop.openPathInEditor === "function") {
        await desktop.openPathInEditor(workspace, relativePath);
      }
      notify("已提交外部编辑器打开请求。");
    } catch (err) {
      notify(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const handleCopy = async () => {
    if (!preview?.text) return;
    try {
      await navigator.clipboard.writeText(preview.text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      notify("复制文件内容失败。");
    }
  };

  return (
    <div className="wb-sidecar-file-view">
      <div className="wb-fv-topbar">
        <div className="wb-fv-topbar-left">
          <div className="wb-fv-crumb" title={relativePath}>
            <SidecarKindIcon kind="file" />
            <span className="wb-fv-crumb-path">{relativePath}</span>
          </div>
        </div>
        {!previewError && (
          <div className="wb-fv-topbar-actions">
            {preview && !preview.binary && preview.text && (
              <button type="button" className="wb-fv-ghost-btn" onClick={() => void handleCopy()}>
                {copied ? "已复制" : "复制"}
              </button>
            )}
            {typeof desktop.openPathInEditor === "function" && (
              <div ref={menuRef} className="wb-fv-ext-split">
                <button
                  type="button"
                  className="wb-fv-ghost-btn"
                  disabled={busy}
                  onClick={() => void handleOpenExternal()}
                >
                  <OpenLocationIcon id="default" size={13} />
                  <span>外部打开</span>
                </button>
                {openLocations.length > 0 && (
                  <button
                    type="button"
                    className="wb-fv-ghost-btn is-caret"
                    aria-label="选择外部打开应用"
                    aria-expanded={menuOpen}
                    disabled={busy}
                    onClick={() => setMenuOpen((v) => !v)}
                  >
                    ▾
                  </button>
                )}
                {openLocations.length > 0 && (
                  <div className={"wb-fv-open-menu" + (menuOpen ? " is-open" : "")}>
                    {openLocations.map((loc) => (
                      <button
                        key={loc.id}
                        type="button"
                        className="wb-fv-open-menu-item"
                        disabled={busy}
                        onClick={() => void handleOpenExternal(loc.id)}
                      >
                        <OpenLocationIcon id={loc.id} size={14} />
                        <span>用 {loc.label} 打开</span>
                      </button>
                    ))}
                  </div>
                )}
              </div>
            )}
          </div>
        )}
      </div>

      {loading && <div className="wb-fv-empty">正在读取文件内容…</div>}

      {!loading && previewError && (
        <div className="wb-fv-protected-card" role="status">
          <strong>受安全边界保护，不直接显示正文</strong>
          <p>{previewError}</p>
        </div>
      )}

      {!loading && preview && (
        <>
          <div className="wb-sidecar-file-meta">
            <span>{preview.language || "文本"}</span>
            <span>·</span>
            <span>{formatBytes(preview.size) || "0 B"}</span>
            <span>·</span>
            <span>{preview.truncated ? "内容已截断" : "完整只读预览"}</span>
          </div>
          {preview.binary ? (
            <div className="wb-panel-body">
              <div className="wb-empty">二进制文件，不直接展示文本内容；可使用上方按钮在外部应用中打开。</div>
            </div>
          ) : (
            <div className="wb-sidecar-code-scroll">
              <table className="wb-sidecar-code-table">
                <tbody>
                  {lines.map((line, idx) => (
                    <tr key={idx}>
                      <td className="wb-sidecar-line-no">{idx + 1}</td>
                      <td className="wb-sidecar-line-code">{line || " "}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {preview.reason && <p className="wb-hint" style={{ padding: "8px 16px" }}>{preview.reason}</p>}
        </>
      )}
    </div>
  );
}

/**
 * SidecarWorkbenchProps 描述右侧多标签伴随工作台所需的全部投影数据与事件回调。
 */
export interface SidecarWorkbenchProps {
  workspace: string;
  runId: string | null;
  selectedRun?: RunHistoryView;
  runStatusText: string;
  tabs: SidecarTab[];
  activeTabId: string;
  expanded: boolean;
  omniboxFocusRevision: number;
  changes: ChangeSet | null;
  selectedFile: string;
  diff: string;
  evidence: EvidenceView | null;
  compactions: ContextCompactionView[];
  timelineKind: TimelineKind;
  filteredTimeline: TimelineItem[];
  recentTimeline: TimelineItem[];
  eventHashBySeq: Map<number, string>;
  subagents: SubagentView[];
  sessionTree: SessionTreeView | null;
  pendingQueueCount: number;
  queueRevision: number;
  openLocations: OpenLocationTarget[];
  onActivateTab: (tabId: string) => void;
  onCloseTab: (tabId: string) => void;
  onOpenNewLauncherTab: () => void;
  onSelectBuiltinPanel: (kind: Exclude<SidecarTabKind, "file">) => void;
  onOpenFileTab: (relativePath: string) => void;
  onInspectChangedFile: (relativePath: string) => void;
  onChooseDiffFile: (relativePath: string) => void;
  onSaveDiffComment: (line: string, body: string, relativeOverride?: string) => Promise<void>;
  onRefreshChanges?: () => Promise<void> | void;
  onChangeTimelineKind: (kind: TimelineKind) => void;
  onOpenChildRun: (childRunId: string) => void;
  onForkCurrentRun: () => void;
  onOpenWorkspaceInLocation: (locationId?: string) => void;
  onRunFromQueue: (newRunId: string) => void;
  onToggleExpanded: () => void;
  onCloseSidecar: () => void;
  notify: (message: string) => void;
}

/**
 * SidecarWorkbench 渲染右侧并列多标签工作台，承载启动台、代码审查、文件、证据、子运行与队列。
 */
export default function SidecarWorkbench({
  workspace,
  runId,
  selectedRun,
  runStatusText,
  tabs,
  activeTabId,
  expanded,
  omniboxFocusRevision,
  changes,
  selectedFile,
  diff,
  evidence,
  compactions,
  timelineKind,
  filteredTimeline,
  recentTimeline,
  eventHashBySeq,
  subagents,
  sessionTree,
  pendingQueueCount,
  queueRevision,
  openLocations,
  onActivateTab,
  onCloseTab,
  onOpenNewLauncherTab,
  onSelectBuiltinPanel,
  onOpenFileTab,
  onInspectChangedFile,
  onChooseDiffFile,
  onSaveDiffComment,
  onRefreshChanges,
  onChangeTimelineKind,
  onOpenChildRun,
  onForkCurrentRun,
  onOpenWorkspaceInLocation,
  onRunFromQueue,
  onToggleExpanded,
  onCloseSidecar,
  notify,
}: SidecarWorkbenchProps) {
  const [customWidth, setCustomWidth] = useState<number | null>(readSavedSidecarWidth);
  const [resizing, setResizing] = useState(false);
  const [quickQuery, setQuickQuery] = useState("");
  const [quickFocused, setQuickFocused] = useState(false);
  const [quickIndex, setQuickIndex] = useState(0);
  const [flatFiles, setFlatFiles] = useState<FlatFileItem[] | null>(null);
  const [loadingFiles, setLoadingFiles] = useState(false);
  const asideRef = useRef<HTMLElement>(null);
  const omniboxRef = useRef<HTMLInputElement>(null);

  const activeTab = useMemo(
    () => tabs.find((item) => item.id === activeTabId) || tabs[0] || buildBuiltinSidecarTab("workbench"),
    [tabs, activeTabId],
  );

  // 切换工作区时清空缓存的快速文件索引与检索词，防止跨工作区残留。
  useEffect(() => {
    setFlatFiles(null);
    setQuickQuery("");
    setQuickIndex(0);
    setLoadingFiles(false);
  }, [workspace]);

  // 响应 ⌘P 或新建标签页时的搜索框自动聚焦信号。
  useEffect(() => {
    if (omniboxFocusRevision > 0 && activeTab.kind === "workbench") {
      omniboxRef.current?.focus();
      omniboxRef.current?.select();
    }
  }, [omniboxFocusRevision, activeTab.kind]);

  // 仅在用户在启动台聚焦搜索框或输入关键字时按需拉取文件树，避免无关挂载开销。
  useEffect(() => {
    if (
      activeTab.kind !== "workbench" ||
      (!quickFocused && !quickQuery.trim()) ||
      flatFiles !== null ||
      loadingFiles ||
      !workspace ||
      typeof desktop.listFileTree !== "function"
    ) {
      return;
    }
    let alive = true;
    setLoadingFiles(true);
    void Promise.resolve(desktop.listFileTree(workspace))
      .then((tree) => {
        if (alive) setFlatFiles(flattenFileTree(tree));
      })
      .catch(() => {
        if (alive) setFlatFiles([]);
      })
      .finally(() => {
        if (alive) setLoadingFiles(false);
      });
    return () => {
      alive = false;
    };
  }, [activeTab.kind, quickFocused, quickQuery, flatFiles, loadingFiles, workspace]);

  const quickMatches = useMemo(() => {
    const q = quickQuery.trim().toLowerCase();
    if (!flatFiles || !q) return [];
    const scored: Array<{ item: FlatFileItem; score: number }> = [];
    for (const item of flatFiles) {
      const nameLower = item.name.toLowerCase();
      const relLower = item.relative.toLowerCase();
      if (nameLower === q) {
        scored.push({ item, score: 0 });
      } else if (nameLower.startsWith(q)) {
        scored.push({ item, score: 1 });
      } else if (nameLower.includes(q)) {
        scored.push({ item, score: 2 });
      } else if (relLower.includes(q)) {
        scored.push({ item, score: 3 });
      }
    }
    scored.sort((a, b) => a.score - b.score || a.item.relative.length - b.item.relative.length);
    return scored.slice(0, 8).map((entry) => entry.item);
  }, [flatFiles, quickQuery]);

  useEffect(() => {
    setQuickIndex(0);
  }, [quickQuery]);

  const isWideCodeTab =
    activeTab.kind === "changes" || activeTab.kind === "files" || activeTab.kind === "file";

  const effectiveDefaultWidth = isWideCodeTab
    ? DEFAULT_REVIEW_SIDECAR_WIDTH
    : DEFAULT_SIDECAR_WIDTH;

  const resolvedWidth = customWidth ?? effectiveDefaultWidth;

  const startResize = (event: React.PointerEvent<HTMLDivElement>) => {
    if (expanded) return;
    event.preventDefault();
    const startX = event.clientX;
    const startWidth = asideRef.current?.offsetWidth || resolvedWidth;
    setResizing(true);

    const computeMaxAllowed = () => {
      const frameWidth =
        asideRef.current?.parentElement?.clientWidth || Math.max(760, window.innerWidth - 260);
      return Math.min(
        MAX_SIDECAR_WIDTH,
        Math.max(MIN_SIDECAR_WIDTH, frameWidth - 392),
      );
    };

    const onMove = (moveEvent: PointerEvent) => {
      const maxAllowed = computeMaxAllowed();
      const nextWidth = Math.min(
        maxAllowed,
        Math.max(MIN_SIDECAR_WIDTH, Math.round(startWidth + (startX - moveEvent.clientX))),
      );
      setCustomWidth(nextWidth);
    };

    const onUp = (upEvent: PointerEvent) => {
      setResizing(false);
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      const maxAllowed = computeMaxAllowed();
      const finalWidth = Math.min(
        maxAllowed,
        Math.max(MIN_SIDECAR_WIDTH, Math.round(startWidth + (startX - upEvent.clientX))),
      );
      setCustomWidth(finalWidth);
      try {
        localStorage.setItem(SIDECAR_WIDTH_STORAGE_KEY, String(finalWidth));
      } catch {
        // 忽略存储异常。
      }
    };

    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
  };

  const preferredTerminal = useMemo(
    () =>
      openLocations.find((loc) => loc.id === "ghostty") ||
      openLocations.find((loc) => loc.id === "terminal") ||
      null,
    [openLocations],
  );

  const changesCount = changes?.entries.length ?? 0;
  const branchChildren = runId && sessionTree ? sessionTree.nodes[runId]?.children || [] : [];

  const asideStyle = expanded
    ? undefined
    : ({
        "--wb-sidecar-w":
          customWidth === null && isWideCodeTab
            ? `clamp(540px, 56vw, ${resolvedWidth}px)`
            : `${resolvedWidth}px`,
      } as React.CSSProperties);

  return (
    <aside
      ref={asideRef}
      className={
        "wb-inspector wb-sidecar" +
        (expanded ? " is-wide" : "") +
        (resizing ? " is-resizing" : "")
      }
      style={asideStyle}
      aria-label="会话右侧工作台"
      data-testid="rail-body"
    >
      {!expanded && (
        <div
          className="wb-sidecar-sash"
          role="separator"
          aria-orientation="vertical"
          aria-label="调整右侧工作台宽度"
          onPointerDown={startResize}
        />
      )}

      {/* 顶部浏览器式多标签栏 */}
      <div className="wb-sidecar-tabbar">
        <div className="wb-sidecar-tabs" role="tablist" aria-label="右侧工作台标签页">
          {tabs.map((tab) => {
            const isActive = tab.id === activeTab.id;
            const badge =
              tab.kind === "changes" && changesCount > 0
                ? String(changesCount)
                : tab.kind === "queue" && pendingQueueCount > 0
                  ? String(pendingQueueCount)
                  : tab.kind === "subagents" && subagents.length > 0
                    ? String(subagents.length)
                    : "";
            const canCloseTab = !(tabs.length === 1 && tab.kind === "workbench");
            return (
              <div
                key={tab.id}
                className={"wb-sidecar-tab" + (isActive ? " is-active" : "")}
                onAuxClick={(e) => {
                  if (e.button === 1 && canCloseTab) {
                    e.preventDefault();
                    onCloseTab(tab.id);
                  }
                }}
              >
                <button
                  type="button"
                  role="tab"
                  aria-selected={isActive}
                  className="wb-sidecar-tab-trigger"
                  title={tab.filePath || tab.title}
                  onClick={() => onActivateTab(tab.id)}
                >
                  <SidecarKindIcon kind={tab.kind} />
                  <span className="wb-sidecar-tab-title">{tab.title}</span>
                  {badge && <span className="wb-sidecar-tab-badge">{badge}</span>}
                </button>
                {canCloseTab && (
                  <button
                    type="button"
                    className="wb-sidecar-tab-close"
                    aria-label={`关闭标签页 ${tab.title}`}
                    onClick={(e) => {
                      e.stopPropagation();
                      onCloseTab(tab.id);
                    }}
                  >
                    ×
                  </button>
                )}
              </div>
            );
          })}
          <button
            type="button"
            className="wb-sidecar-new-tab"
            aria-label="新建工作台标签页"
            title="从这里开始 (新标签页)"
            onClick={onOpenNewLauncherTab}
          >
            +
          </button>
        </div>

        <div className="wb-sidecar-window-actions">
          <button
            type="button"
            className="wb-sidecar-win-btn"
            title={expanded ? "恢复标准分栏宽度" : "宽屏展开工作台"}
            aria-label={expanded ? "恢复标准分栏宽度" : "宽屏展开工作台"}
            onClick={onToggleExpanded}
          >
            {expanded ? (
              <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <path d="M6.5 3v3.5H3M9.5 13V9.5H13M3 6.5 6.5 3M13 9.5 9.5 13" />
              </svg>
            ) : (
              <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <path d="M10 3h3v3M6 13H3v-3M13 3 9.2 6.8M3 13l3.8-3.8" />
              </svg>
            )}
          </button>
          <button
            type="button"
            className="wb-sidecar-win-btn"
            aria-label="关闭右侧面板"
            title="收起右侧工作台 (⌘B)"
            onClick={onCloseSidecar}
          >
            ×
          </button>
        </div>
      </div>

      {/* 标签页一：「从这里开始」能力启动台 + 实时上下文聚合 */}
      {activeTab.kind === "workbench" && (
        <div className="wb-panel-body wb-launcher-body">
          {/* 顶部快速文件检索 Omnibox */}
          {typeof desktop.listFileTree === "function" && (
            <div className="wb-launcher-omnibox-wrap">
              <div className="wb-launcher-omnibox">
                <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true">
                  <circle cx="7" cy="7" r="4.6" />
                  <path d="m10.5 10.5 3.1 3.1" />
                </svg>
                <input
                  ref={omniboxRef}
                  type="search"
                  aria-label="快速搜索或打开工作区文件"
                  value={quickQuery}
                  placeholder="搜索工作区文件名或路径，回车直接新标签打开…"
                  onFocus={() => setQuickFocused(true)}
                  onBlur={() => {
                    window.setTimeout(() => setQuickFocused(false), 150);
                  }}
                  onChange={(e) => setQuickQuery(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "ArrowDown" && quickMatches.length > 0) {
                      e.preventDefault();
                      setQuickIndex((i) => (i + 1) % quickMatches.length);
                    } else if (e.key === "ArrowUp" && quickMatches.length > 0) {
                      e.preventDefault();
                      setQuickIndex((i) => (i - 1 + quickMatches.length) % quickMatches.length);
                    } else if (e.key === "Enter" && quickMatches[quickIndex]) {
                      e.preventDefault();
                      const chosen = quickMatches[quickIndex];
                      setQuickQuery("");
                      onOpenFileTab(chosen.relative);
                    }
                  }}
                />
                <kbd className="wb-launcher-kbd">⌘P</kbd>
              </div>
              {quickQuery.trim() && (
                <div className="wb-launcher-omnibox-results" role="listbox" aria-label="文件匹配结果">
                  {loadingFiles && <div className="wb-launcher-omni-empty">正在读取工作区文件索引…</div>}
                  {!loadingFiles && quickMatches.length === 0 && (
                    <div className="wb-launcher-omni-empty">未找到匹配「{quickQuery.trim()}」的工作区文件</div>
                  )}
                  {!loadingFiles &&
                    quickMatches.map((item, idx) => (
                      <button
                        key={item.relative}
                        type="button"
                        className={"wb-launcher-omni-item" + (idx === quickIndex ? " is-active" : "")}
                        onMouseDown={(e) => e.preventDefault()}
                        onClick={() => {
                          setQuickQuery("");
                          onOpenFileTab(item.relative);
                        }}
                      >
                        <SidecarKindIcon kind="file" />
                        <span className="wb-launcher-omni-name">{item.name}</span>
                        {item.dir && <span className="wb-launcher-omni-dir">{item.dir}</span>}
                        {item.size !== undefined && (
                          <small className="wb-launcher-omni-size">{formatBytes(item.size)}</small>
                        )}
                      </button>
                    ))}
                </div>
              )}
            </div>
          )}

          {/* 核心工具入口列表（融合 Trae / GLM / Codex 启动台语法） */}
          <div className="wb-launcher-section">
            <div className="wb-launcher-heading">
              <span>从这里开始</span>
              {runId && <small>{selectedRun?.title || runId} · {runStatusText}</small>}
            </div>

            <div className="wb-launcher-cards">
              <button
                type="button"
                className="wb-launcher-card"
                onClick={() => onSelectBuiltinPanel("changes")}
              >
                <span className="wb-launcher-card-icon">
                  <SidecarKindIcon kind="changes" />
                </span>
                <span className="wb-launcher-card-copy">
                  <strong>
                    代码审查
                    {changesCount > 0 && <em className="wb-launcher-pill">{changesCount} 项变更</em>}
                  </strong>
                  <small>查看工作区差异、区分 Agent 修改并添加行级审查批注</small>
                </span>
                <kbd className="wb-launcher-kbd">⌃⇧G</kbd>
              </button>

              <button
                type="button"
                className="wb-launcher-card"
                onClick={() => onSelectBuiltinPanel("files")}
              >
                <span className="wb-launcher-card-icon">
                  <SidecarKindIcon kind="files" />
                </span>
                <span className="wb-launcher-card-copy">
                  <strong>项目文件</strong>
                  <small>浏览目录树、只读安全预览或拆分为独立文件标签</small>
                </span>
                <kbd className="wb-launcher-kbd">⌘P</kbd>
              </button>

              <button
                type="button"
                className="wb-launcher-card"
                onClick={() => {
                  if (runId) {
                    onSelectBuiltinPanel("evidence");
                  } else {
                    notify("请先在左侧选择一个已有会话，或发起新任务后查看运行证据。");
                  }
                }}
              >
                <span className="wb-launcher-card-icon">
                  <SidecarKindIcon kind="evidence" />
                </span>
                <span className="wb-launcher-card-copy">
                  <strong>
                    任务摘要与验证证据
                    {runId && evidence && (
                      <em
                        className={
                          "wb-launcher-pill " +
                          (evidence.trusted && evidence.verification?.verification_ok
                            ? "is-ok"
                            : "is-warn")
                        }
                      >
                        {evidence.trusted && evidence.verification?.verification_ok ? "Verifier 可信" : "待核验"}
                      </em>
                    )}
                  </strong>
                  <small>核查 Verifier 证据、Token 用量、压缩检查点与 Journal 哈希链</small>
                </span>
                <kbd className="wb-launcher-kbd">⌘E</kbd>
              </button>

              <button
                type="button"
                className="wb-launcher-card"
                onClick={() => {
                  if (runId) {
                    onForkCurrentRun();
                  } else {
                    onSelectBuiltinPanel("queue");
                  }
                }}
              >
                <span className="wb-launcher-card-icon">
                  <SidecarKindIcon kind="fork" />
                </span>
                <span className="wb-launcher-card-copy">
                  <strong>
                    {runId ? "侧边分支对话" : "候选任务队列"}
                    {!runId && pendingQueueCount > 0 && (
                      <em className="wb-launcher-pill">{pendingQueueCount} 待确认</em>
                    )}
                  </strong>
                  <small>
                    {runId
                      ? "基于当前 Run 创建独立分支追问或试探方案，不污染原会话主线"
                      : "查看 Ctrl/Cmd+Enter 提交的候选队列，经人工确认后启动"}
                  </small>
                </span>
                <kbd className="wb-launcher-kbd">{runId ? "⌘⇧S" : "队列"}</kbd>
              </button>

              {preferredTerminal && (
                <button
                  type="button"
                  className="wb-launcher-card"
                  onClick={() => onOpenWorkspaceInLocation(preferredTerminal.id)}
                >
                  <span className="wb-launcher-card-icon">
                    <OpenLocationIcon id={preferredTerminal.id} size={18} />
                  </span>
                  <span className="wb-launcher-card-copy">
                    <strong>用 {preferredTerminal.label} 打开终端</strong>
                    <small>定位到当前工作区启动外部终端，保持宿主与 Agent 授权边界分离</small>
                  </span>
                  <span className="wb-launcher-ext" aria-hidden="true">↗</span>
                </button>
              )}
            </div>
          </div>

          {/* 实时态势一：工作区变更汇总与快捷跳转（保留 data-testid="change-summary" 合同） */}
          <div className="wb-launcher-section">
            <div className="wb-launcher-heading">
              <span>工作区变更</span>
              <small>
                {changes ? `${changes.entries.length} 个文件（实时状态）` : "读取中…"}
              </small>
            </div>
            {changes && (
              <p className="wb-launcher-stat-line" data-testid="change-summary">
                新增 {changes.added} · 修改 {changes.modified} · 删除 {changes.deleted} · 未跟踪{" "}
                {changes.untracked}
              </p>
            )}
            {changes && changes.entries.length > 0 && (
              <div className="wb-launcher-list">
                {changes.entries.slice(0, 6).map((entry) => (
                  <div className="wb-launcher-list-row" key={entry.relative}>
                    <button
                      type="button"
                      className="wb-launcher-list-main"
                      onClick={() => onInspectChangedFile(entry.relative)}
                    >
                      <span className={"wb-diff-status status-" + entry.status}>{entry.status}</span>
                      <span className="wb-launcher-list-path">{entry.relative}</span>
                      <em className={"wb-owner-tag " + (entry.agent_owned ? "is-agent" : "is-local")}>
                        {entry.agent_owned ? "Agent 修改" : "本地已有"}
                      </em>
                    </button>
                    <button
                      type="button"
                      className="wb-launcher-mini-btn"
                      title="在独立标签页预览全文"
                      onClick={() => onOpenFileTab(entry.relative)}
                    >
                      预览
                    </button>
                  </div>
                ))}
                {changes.entries.length > 6 && (
                  <button
                    type="button"
                    className="wb-launcher-more"
                    onClick={() => onSelectBuiltinPanel("changes")}
                  >
                    查看全部 {changes.entries.length} 个变更文件 →
                  </button>
                )}
              </div>
            )}
          </div>

          {/* 实时态势二：子智能体委派与关联分支（仅当当前会话存在数据时展示） */}
          {runId && (subagents.length > 0 || branchChildren.length > 0) && (
            <div className="wb-launcher-section">
              <div className="wb-launcher-heading">
                <span>子智能体与分支</span>
                <button
                  type="button"
                  className="wb-launcher-link"
                  onClick={() => onSelectBuiltinPanel("subagents")}
                >
                  展开详情 →
                </button>
              </div>
              <div className="wb-launcher-list">
                {subagents.slice(0, 4).map((sub) => {
                  const done = sub.state === "completed";
                  return (
                    <button
                      key={sub.childRunId}
                      type="button"
                      className="wb-launcher-sub-row"
                      onClick={() => onOpenChildRun(sub.childRunId)}
                    >
                      <span className={"wb-badge " + (done ? "is-ok" : "is-warn")}>
                        {!done && <i className="wb-pulse-dot" aria-hidden="true" />}
                        {sub.state}
                      </span>
                      <span className="wb-launcher-list-path">{sub.task || sub.childRunId}</span>
                      <small>{sub.toolCalls > 0 ? `${sub.toolCalls} 次调用` : "查看"}</small>
                    </button>
                  );
                })}
                {branchChildren.slice(0, 3).map((childId) => {
                  const childNode = sessionTree?.nodes[childId];
                  return (
                    <button
                      key={childId}
                      type="button"
                      className="wb-launcher-sub-row"
                      onClick={() => onOpenChildRun(childId)}
                    >
                      <span className="wb-badge is-muted">分支</span>
                      <span className="wb-launcher-list-path">{childNode?.task || childId}</span>
                      <small>{childNode?.state || "打开"}</small>
                    </button>
                  );
                })}
              </div>
            </div>
          )}

          {/* 实时态势三：当前会话最近 Journal 领域事件（对应 Antigravity Background Tasks 态势区） */}
          {runId && recentTimeline.length > 0 && (
            <div className="wb-launcher-section">
              <div className="wb-launcher-heading">
                <span>最近领域事件</span>
                <button
                  type="button"
                  className="wb-launcher-link"
                  onClick={() => onSelectBuiltinPanel("evidence")}
                >
                  完整时间线 →
                </button>
              </div>
              <div className="wb-launcher-events">
                {recentTimeline.slice(-5).reverse().map((item) => (
                  <div className={`wb-launcher-event tone-${item.tone}`} key={item.id}>
                    <div className="wb-launcher-event-top">
                      <strong>#{item.seq} · {item.label}</strong>
                    </div>
                    {item.detail && <p>{item.detail}</p>}
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
      )}

      {/* 标签页二：项目文件树浏览与内建阅读器 */}
      {activeTab.kind === "files" && (
        <FilesPanel
          workspace={workspace}
          notify={notify}
          onOpenInTab={(rel) => onOpenFileTab(rel)}
          autoSelectFirstFile
        />
      )}

      {/* 标签页三：单文件独立标签只读预览 */}
      {activeTab.kind === "file" && activeTab.filePath && (
        <SingleFileTabView
          key={`${workspace}:${activeTab.filePath}`}
          workspace={workspace}
          relativePath={activeTab.filePath}
          openLocations={openLocations}
          notify={notify}
        />
      )}

      {/* 标签页四：候选队列 */}
      {activeTab.kind === "queue" && (
        <QueuePanel
          workspace={workspace}
          revision={queueRevision}
          onRun={onRunFromQueue}
          onError={notify}
        />
      )}

      {/* 标签页五：子智能体与会话分支 */}
      {activeTab.kind === "subagents" && (
        <div className="wb-panel-body">
          <div className="wb-label">Agent Delegate & Branches</div>
          <h2>子智能体与会话分支</h2>
          {runId ? (
            <>
              <div className="wb-sidecar-fork-callout">
                <div>
                  <strong>基于当前 Run 开启独立分支对话</strong>
                  <p>从 #{runId} 的快照派生新分支追问或验证备选方案，不会续接或篡改原会话历史。</p>
                </div>
                <button type="button" className="wb-secondary" onClick={onForkCurrentRun}>
                  创建分支对话
                </button>
              </div>
              <p className="wb-hint" style={{ marginTop: 12 }}>
                会话树：{sessionTree ? Object.keys(sessionTree.nodes).length + " 个关联 Run" : "暂无可用投影"}
              </p>
              <div className="wb-tree-card">
                <div>
                  <span className="wb-badge is-accent">当前 Run</span>
                  <strong>{runId}</strong>
                </div>
                <p>{selectedRun?.title || selectedRun?.task || "当前会话任务"}</p>
                {subagents.length > 0 ? (
                  <div className="wb-tree-children">
                    {subagents.map((child) => {
                      const isDone = child.state === "completed";
                      return (
                        <div className="wb-tree-card" key={child.childRunId}>
                          <div>
                            <span className={"wb-badge " + (isDone ? "is-ok" : "is-warn")}>
                              {!isDone && <i className="wb-pulse-dot" aria-hidden="true" />}
                              {child.state}
                            </span>
                            <strong>{child.childRunId}</strong>
                          </div>
                          {child.task && <p>{child.task}</p>}
                          <p>
                            {child.outcome || "尚无完成结论"} ·{" "}
                            {child.usageReported
                              ? `${child.toolCalls} 次工具调用 · 输入 ${child.promptTokens.toLocaleString()} / 输出 ${child.completionTokens.toLocaleString()} tokens`
                              : "用量未回报"}
                          </p>
                          <button
                            type="button"
                            className="wb-secondary"
                            onClick={() => onOpenChildRun(child.childRunId)}
                          >
                            查看子运行
                          </button>
                        </div>
                      );
                    })}
                  </div>
                ) : (
                  <div className="wb-empty">当前 Run 没有已记录的子智能体委派。</div>
                )}
              </div>
              {branchChildren.length > 0 && (
                <>
                  <div className="wb-panel-separator" />
                  <div className="wb-label">后续与分支运行</div>
                  <div className="wb-tree-children">
                    {branchChildren.map((childId) => {
                      const childNode = sessionTree?.nodes[childId];
                      return (
                        <div className="wb-tree-card" key={childId}>
                          <div>
                            <span className="wb-badge is-muted">
                              {childNode?.phase || childNode?.state || "关联"}
                            </span>
                            <strong>{childId}</strong>
                          </div>
                          {childNode?.task && <p>{childNode.task}</p>}
                          <button
                            type="button"
                            className="wb-secondary"
                            onClick={() => onOpenChildRun(childId)}
                          >
                            打开关联 Run
                          </button>
                        </div>
                      );
                    })}
                  </div>
                </>
              )}
            </>
          ) : (
            <div className="wb-empty">请先在左侧选择一个会话，以查看其子智能体委派树或创建分支对话。</div>
          )}
        </div>
      )}

      {/* 标签页六：变更审查工作区（连续 Diff 卡片流 + 右侧层级变更目录树） */}
      {activeTab.kind === "changes" && (
        <ChangesReviewPanel
          workspace={workspace}
          changes={changes}
          selectedFile={selectedFile}
          activeDiff={diff}
          onChooseFile={onChooseDiffFile}
          onOpenFileTab={onOpenFileTab}
          onSaveComment={onSaveDiffComment}
          onRefreshChanges={onRefreshChanges}
        />
      )}

      {/* 标签页七：验证与证据 + Event Journal 哈希时间线 */}
      {activeTab.kind === "evidence" && (
        <div className="wb-panel-body">
          {!runId ? (
            <div className="wb-empty">请先选择一个会话以读取其 Verifier 证据与 Event Journal 时间线。</div>
          ) : (
            <>
              {!evidence ? (
                <p>暂无已读取的证据。</p>
              ) : (
                <div data-testid="result-layers">
                  <div
                    className={
                      "wb-trust-banner " +
                      (evidence.trusted && evidence.verification?.verification_ok
                        ? "is-trusted"
                        : "is-unverified")
                    }
                  >
                    <div>
                      <div className="wb-label">
                        运行终态：{evidence.state_label || evidence.state}
                        {evidence.outcome_label || evidence.outcome
                          ? ` · ${evidence.outcome_label || evidence.outcome}`
                          : ""}
                      </div>
                      <strong>
                        验证可信度：{evidence.trust_label || (evidence.trusted ? "可信" : "未确认")}
                        {evidence.verification
                          ? ` (verification_ok: ${String(Boolean(evidence.verification.verification_ok))}${evidence.verification.verification_seq ? ` · seq #${evidence.verification.verification_seq}` : ""})`
                          : ""}
                      </strong>
                      <p>运行完成与验证可信严格分离；无 Verifier 证据绝不宣称已验证。</p>
                    </div>
                  </div>
                  <div className="wb-metric-grid">
                    <div>
                      <span>输入 Token</span>
                      <strong>{(evidence.metrics?.input_tokens ?? 0).toLocaleString()}</strong>
                    </div>
                    <div>
                      <span>输出 Token</span>
                      <strong>{(evidence.metrics?.output_tokens ?? 0).toLocaleString()}</strong>
                    </div>
                    <div>
                      <span>回合 / 工具调用</span>
                      <strong>
                        {evidence.metrics?.turns ?? 0} 回合 · {evidence.metrics?.tool_calls ?? 0} 次调用
                      </strong>
                      <small className="wb-metric-sub">
                        turns {evidence.metrics?.turns ?? 0}
                      </small>
                    </div>
                      <div>
                        <span>上下文压缩</span>
                        <strong>{evidence.metrics?.context_compactions ?? compactions.length} 次</strong>
                      </div>
                    </div>
                  <p className="wb-hint">
                    {evidence.metrics?.usage_estimated
                      ? "以上用量包含估算数据。"
                      : "用量来自已记录的运行证据。"}
                    费用不可用时不做推断。
                  </p>
                </div>
              )}
              {compactions.length > 0 && (
                <>
                  <div className="wb-panel-separator" />
                  <div className="wb-label">上下文压缩检查点</div>
                  {compactions.map((item) => (
                    <div className="wb-compaction-card" key={item.seq}>
                      <div>
                        <strong>
                          #{item.seq} · 触发 {item.trigger}
                        </strong>
                        <span className="wb-badge is-accent">{item.strategy}</span>
                      </div>
                      <p>
                        {item.unitsBefore !== null || item.unitsAfter !== null
                          ? `估算单位 ${item.unitsBefore ?? "—"} → ${item.unitsAfter ?? "—"} · `
                          : ""}
                        折叠 {item.compactedTurns} 个回合
                        {item.lastProviderPromptTokens > 0
                          ? ` · 最近 Prompt ${item.lastProviderPromptTokens.toLocaleString()} tokens`
                          : ""}
                      </p>
                      {item.summaryPreview && <p>{item.summaryPreview}</p>}
                    </div>
                  ))}
                </>
              )}
              <div className="wb-panel-separator" />
              <div className="wb-label">Event Journal 时间线</div>
              <div className="wb-timeline-filters" role="group" aria-label="时间线筛选">
                {(
                  [
                    ["all", "全部"],
                    ["tool", "工具"],
                    ["subagent", "子运行"],
                    ["compaction", "压缩"],
                  ] as const
                ).map(([kind, label]) => (
                  <button
                    key={kind}
                    type="button"
                    className={timelineKind === kind ? "current" : ""}
                    onClick={() => onChangeTimelineKind(kind)}
                  >
                    {label}
                  </button>
                ))}
              </div>
              {filteredTimeline.length > 0 ? (
                <div className="wb-timeline-list">
                  {filteredTimeline.map((item) => {
                    const hash = eventHashBySeq.get(item.seq);
                    return (
                      <div className={`wb-timeline-item tone-${item.tone}`} key={item.id}>
                        <div>
                          <strong>
                            #{item.seq} · {item.label}
                          </strong>
                          {hash && (
                            <span className="wb-hash-pill" title={hash}>
                              {hash.slice(0, 10)}
                            </span>
                          )}
                        </div>
                        {item.detail && <p>{item.detail}</p>}
                      </div>
                    );
                  })}
                </div>
              ) : (
                <p className="wb-hint">当前筛选条件下暂无已记录的领域事件。</p>
              )}
            </>
          )}
        </div>
      )}
    </aside>
  );
}
