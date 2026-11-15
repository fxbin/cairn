/**
 * INPUT: workspace 路径、ChangeSet 变更列表、选中文件与 Diff 文本及审查备注回调。
 * OUTPUT: 连续多文件 Diff 卡片流与右侧可筛选层级变更目录树的一体化审查视图。
 * POS: studio/frontend/src/workbench 变更审查子视图，由 SidecarWorkbench 承载渲染。
 *
 * Author: fxbin
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { desktop } from "../desktop/client";
import type { ChangeEntry, ChangeSet } from "../desktop/types";
import DiffViewer, { countDiffStatsFromText, type DiffViewMode } from "./DiffViewer";

const DIFF_VIEW_MODE_STORAGE_KEY = "cairn.gui.diffViewModeV2";
const TREE_OPEN_STORAGE_KEY = "cairn.gui.changesTreeOpen";
const EAGER_DIFF_FILE_LIMIT = 24;

type ReviewScope = "all" | "unstaged" | "staged";

/**
 * DirTreeNode 表示右侧「已更改的文件」面板中的目录层级节点。
 */
interface DirTreeNode {
  path: string;
  label: string;
  dirs: DirTreeNode[];
  files: ChangeEntry[];
}

/**
 * 读取本地持久化的代码差异视图模式，默认采用阅读密度更舒适的内联统一视图（unified）。
 */
function readSavedDiffMode(): DiffViewMode {
  if (typeof window === "undefined") return "unified";
  try {
    const raw = localStorage.getItem(DIFF_VIEW_MODE_STORAGE_KEY);
    if (raw === "split" || raw === "unified") return raw;
  } catch {
    // 忽略本地存储受限场景。
  }
  return "unified";
}

/**
 * 读取本地持久化的右侧变更文件树展开状态，默认展开。
 */
function readSavedTreeOpen(): boolean {
  if (typeof window === "undefined") return true;
  try {
    const raw = localStorage.getItem(TREE_OPEN_STORAGE_KEY);
    if (raw === "0") return false;
  } catch {
    // 忽略本地存储受限场景。
  }
  return true;
}

/**
 * 根据文件扩展名推导紧凑语言徽标文本与色彩语义分类。
 */
function fileLangMeta(relative: string): { tag: string; tone: string } {
  const lower = relative.toLowerCase();
  const ext = lower.includes(".") ? lower.slice(lower.lastIndexOf(".") + 1) : "";
  switch (ext) {
    case "go":
      return { tag: "go", tone: "go" };
    case "tsx":
      return { tag: "tsx", tone: "ts" };
    case "ts":
      return { tag: "ts", tone: "ts" };
    case "jsx":
    case "js":
    case "mjs":
      return { tag: "js", tone: "js" };
    case "css":
    case "scss":
      return { tag: "css", tone: "css" };
    case "json":
      return { tag: "{}", tone: "json" };
    case "md":
      return { tag: "md", tone: "md" };
    case "yaml":
    case "yml":
      return { tag: "yml", tone: "yml" };
    case "html":
      return { tag: "<>", tone: "html" };
    default:
      return { tag: "··", tone: "plain" };
  }
}

/**
 * 拆分相对路径为目录前缀（带末尾斜杠）与文件名，便于头部弱化目录、突出文件名。
 */
function splitDirAndBase(relative: string): { dirPrefix: string; baseName: string } {
  const normalized = relative.replace(/\\/g, "/");
  const idx = normalized.lastIndexOf("/");
  if (idx < 0) return { dirPrefix: "", baseName: normalized };
  return {
    dirPrefix: normalized.slice(0, idx + 1),
    baseName: normalized.slice(idx + 1),
  };
}

/**
 * 将扁平变更条目构建为层级目录树，并对深层单子目录链做适度折叠，防止右侧窄树横向溢出。
 */
function buildChangedDirTree(entries: ChangeEntry[]): DirTreeNode {
  interface MutableDir {
    name: string;
    path: string;
    dirs: Map<string, MutableDir>;
    files: ChangeEntry[];
  }

  const root: MutableDir = { name: "", path: "", dirs: new Map(), files: [] };

  for (const entry of entries) {
    const parts = entry.relative.replace(/\\/g, "/").split("/").filter(Boolean);
    if (parts.length <= 1) {
      root.files.push(entry);
      continue;
    }
    let cursor = root;
    let currentPath = "";
    for (let i = 0; i < parts.length - 1; i += 1) {
      const seg = parts[i];
      currentPath = currentPath ? `${currentPath}/${seg}` : seg;
      let child = cursor.dirs.get(seg);
      if (!child) {
        child = { name: seg, path: currentPath, dirs: new Map(), files: [] };
        cursor.dirs.set(seg, child);
      }
      cursor = child;
    }
    cursor.files.push(entry);
  }

  const finalize = (node: MutableDir, depth: number): DirTreeNode => {
    let cur = node;
    let label = cur.name;
    // 当目录深度超过 1 层且仅包含唯一子目录、无直接文件时合并展示（例如 studio/frontend/src）。
    while (label && depth >= 1 && cur.files.length === 0 && cur.dirs.size === 1) {
      const next = Array.from(cur.dirs.values())[0];
      // 保留最末一级父目录独立展开（如 workbench），让目录层次清晰又不挤占横向空间。
      if (next.dirs.size === 0 && next.files.length > 0) {
        break;
      }
      label = `${label}/${next.name}`;
      cur = next;
    }

    const dirs = Array.from(cur.dirs.values())
      .sort((a, b) => a.name.localeCompare(b.name))
      .map((d) => finalize(d, label ? depth + 1 : depth));

    const files = [...cur.files].sort((a, b) => a.relative.localeCompare(b.relative));
    return {
      path: cur.path,
      label,
      dirs,
      files,
    };
  };

  return finalize(root, 0);
}

/**
 * 读取指定变更条目在给定范围下的差异文本，兼容未跟踪纯文本预览回退。
 */
async function fetchEntryDiff(
  workspace: string,
  entry: ChangeEntry,
  scope: ReviewScope,
): Promise<string> {
  try {
    if (scope === "all") {
      return await desktop.diffFile(workspace, entry.relative);
    }
    return await desktop.diffFile(workspace, entry.relative, scope);
  } catch (err) {
    if (entry.status !== "untracked" || scope === "staged") {
      throw err;
    }
    if (typeof desktop.previewFile !== "function") return "";
    const preview = await desktop.previewFile(workspace, entry.relative);
    if (preview.binary) {
      return "Binary files differ";
    }
    if (!preview.text) return "";
    const lines = preview.text.replace(/\n$/, "").split("\n");
    return [
      `diff --git a/${entry.relative} b/${entry.relative}`,
      "--- /dev/null",
      `+++ b/${entry.relative}`,
      `@@ -0,0 +1,${lines.length} @@`,
      ...lines.map((line) => `+${line}`),
    ].join("\n");
  }
}

/**
 * 判断路径是否为自动生成绑定、构建产物或锁文件，连续流初始默认折叠以提升主文件阅读聚焦度。
 */
function isGeneratedOrLockFile(relative: string): boolean {
  const lower = relative.replace(/\\/g, "/").toLowerCase();
  return (
    lower.includes("/bindings/") ||
    lower.startsWith("bindings/") ||
    lower.endsWith("package-lock.json") ||
    lower.endsWith("pnpm-lock.yaml") ||
    lower.endsWith("yarn.lock") ||
    lower.endsWith("go.sum")
  );
}

/**
 * ChangesReviewPanelProps 描述代码变更审查面板所需的输入状态与交互回调。
 */
export interface ChangesReviewPanelProps {
  workspace: string;
  changes: ChangeSet | null;
  selectedFile: string;
  activeDiff: string;
  onChooseFile: (relative: string) => void;
  onOpenFileTab: (relative: string) => void;
  onSaveComment: (line: string, body: string, relativeOverride?: string) => Promise<void>;
  onRefreshChanges?: () => Promise<void> | void;
}

/**
 * 渲染左侧连续 Diff 流 + 右侧可筛选层级变更文件树的一体化审查视图。
 */
export default function ChangesReviewPanel({
  workspace,
  changes,
  selectedFile,
  activeDiff,
  onChooseFile,
  onOpenFileTab,
  onSaveComment,
  onRefreshChanges,
}: ChangesReviewPanelProps) {
  const [viewMode, setViewMode] = useState<DiffViewMode>(readSavedDiffMode);
  const [treeOpen, setTreeOpen] = useState<boolean>(readSavedTreeOpen);
  const [scope, setScope] = useState<ReviewScope>("all");
  const [filterQuery, setFilterQuery] = useState("");
  const [groupOpen, setGroupOpen] = useState(true);
  const [collapsedDirs, setCollapsedDirs] = useState<Record<string, boolean>>({});
  const [collapsedCards, setCollapsedCards] = useState<Record<string, boolean>>({});
  const [diffByFile, setDiffByFile] = useState<Record<string, string>>({});
  const [loadingByFile, setLoadingByFile] = useState<Record<string, boolean>>({});
  const [copiedPath, setCopiedPath] = useState("");
  const [refreshing, setRefreshing] = useState(false);
  const [scrollActiveFile, setScrollActiveFile] = useState("");

  const streamRef = useRef<HTMLDivElement>(null);
  const cardRefs = useRef<Map<string, HTMLElement>>(new Map());
  const requestedSetRef = useRef<Set<string>>(new Set());

  // 切换工作区或比较范围时重置缓存的各文件差异映射。
  useEffect(() => {
    requestedSetRef.current.clear();
    setDiffByFile({});
    setLoadingByFile({});
    setCollapsedCards({});
  }, [workspace, scope]);

  // 同步父组件已获取的当前选中文件 Diff（保证单文件切换与测试零额外请求）。
  useEffect(() => {
    if (scope === "all" && selectedFile && activeDiff !== "") {
      requestedSetRef.current.add(`all:${selectedFile}`);
      setDiffByFile((prev) =>
        prev[selectedFile] === activeDiff ? prev : { ...prev, [selectedFile]: activeDiff },
      );
    }
  }, [scope, selectedFile, activeDiff]);

  const entries = useMemo(() => {
    const raw = changes?.entries || [];
    if (scope === "staged") {
      return raw.filter((item) => item.status === "staged" || item.status === "mixed" || item.status === "renamed");
    }
    if (scope === "unstaged") {
      return raw.filter((item) => item.status !== "staged");
    }
    return raw;
  }, [changes, scope]);

  const filteredEntries = useMemo(() => {
    const q = filterQuery.trim().toLowerCase();
    if (!q) return entries;
    return entries.filter((entry) => entry.relative.toLowerCase().includes(q));
  }, [entries, filterQuery]);

  const loadSingleEntryDiff = useCallback(
    async (entry: ChangeEntry, force = false) => {
      if (!workspace || typeof desktop.diffFile !== "function") return;
      const cacheKey = `${scope}:${entry.relative}`;
      if (!force && requestedSetRef.current.has(cacheKey)) return;
      requestedSetRef.current.add(cacheKey);
      setLoadingByFile((prev) => ({ ...prev, [entry.relative]: true }));
      try {
        const text = await fetchEntryDiff(workspace, entry, scope);
        setDiffByFile((prev) => ({ ...prev, [entry.relative]: text }));
      } catch {
        setDiffByFile((prev) => ({ ...prev, [entry.relative]: "" }));
      } finally {
        setLoadingByFile((prev) => ({ ...prev, [entry.relative]: false }));
      }
    },
    [workspace, scope],
  );

  // 自动预加载前 N 个变更文件的 Diff，形成无需逐个点击即可顺滑通读的连续审查流。
  useEffect(() => {
    const eagerSlice = filteredEntries.slice(0, EAGER_DIFF_FILE_LIMIT);
    for (const entry of eagerSlice) {
      // 默认 all 范围下，当前 selectedFile 已由上层发起请求，避免重复触发同参调用。
      if (scope === "all" && entry.relative === selectedFile) {
        continue;
      }
      void loadSingleEntryDiff(entry);
    }
  }, [filteredEntries, scope, selectedFile, loadSingleEntryDiff]);

  // 当切换到 staged / unstaged 范围时，也为当前选中文件拉取对应范围的 Diff。
  useEffect(() => {
    if (scope === "all") return;
    const match = entries.find((item) => item.relative === selectedFile) || entries[0];
    if (match) {
      void loadSingleEntryDiff(match);
    }
  }, [scope, entries, selectedFile, loadSingleEntryDiff]);

  const statsByFile = useMemo(() => {
    const map = new Map<string, { add: number; del: number }>();
    for (const entry of entries) {
      const rawText =
        scope === "all" && entry.relative === selectedFile && activeDiff
          ? activeDiff
          : diffByFile[entry.relative] || "";
      if (scope === "all" && ((entry.additions ?? 0) > 0 || (entry.deletions ?? 0) > 0)) {
        map.set(entry.relative, {
          add: entry.additions ?? 0,
          del: entry.deletions ?? 0,
        });
      } else if (rawText) {
        const parsed = countDiffStatsFromText(rawText);
        map.set(entry.relative, { add: parsed.additions, del: parsed.deletions });
      } else {
        map.set(entry.relative, {
          add: entry.additions ?? 0,
          del: entry.deletions ?? 0,
        });
      }
    }
    return map;
  }, [entries, scope, selectedFile, activeDiff, diffByFile]);

  const totalStats = useMemo(() => {
    if (
      scope === "all" &&
      changes &&
      ((changes.total_additions ?? 0) > 0 || (changes.total_deletions ?? 0) > 0)
    ) {
      return {
        add: changes.total_additions ?? 0,
        del: changes.total_deletions ?? 0,
      };
    }
    let add = 0;
    let del = 0;
    for (const entry of entries) {
      const s = statsByFile.get(entry.relative);
      if (s) {
        add += s.add;
        del += s.del;
      }
    }
    return { add, del };
  }, [scope, changes, entries, statsByFile]);

  const dirTree = useMemo(() => buildChangedDirTree(filteredEntries), [filteredEntries]);

  const projectName = useMemo(() => {
    const base = (changes?.root || workspace || "").replace(/\\/g, "/");
    return base.split("/").filter(Boolean).pop() || "工作区";
  }, [changes?.root, workspace]);

  const activeHighlightFile = scrollActiveFile || selectedFile || filteredEntries[0]?.relative || "";

  const updateViewMode = (next: DiffViewMode) => {
    setViewMode(next);
    try {
      localStorage.setItem(DIFF_VIEW_MODE_STORAGE_KEY, next);
    } catch {
      // 忽略存储受限异常。
    }
  };

  const toggleTreeOpen = () => {
    setTreeOpen((prev) => {
      const next = !prev;
      try {
        localStorage.setItem(TREE_OPEN_STORAGE_KEY, next ? "1" : "0");
      } catch {
        // 忽略存储受限异常。
      }
      return next;
    });
  };

  const isCardCollapsed = useCallback(
    (relative: string) => {
      const explicit = collapsedCards[relative];
      if (explicit !== undefined) return explicit;
      if (relative === selectedFile) return false;
      return isGeneratedOrLockFile(relative);
    },
    [collapsedCards, selectedFile],
  );

  const allCollapsed = useMemo(
    () =>
      filteredEntries.length > 0 &&
      filteredEntries.every((entry) => isCardCollapsed(entry.relative)),
    [filteredEntries, isCardCollapsed],
  );

  const toggleCollapseAll = () => {
    const next: Record<string, boolean> = {};
    for (const entry of filteredEntries) {
      next[entry.relative] = !allCollapsed;
    }
    setCollapsedCards(next);
  };

  const handleRefresh = async () => {
    if (refreshing) return;
    setRefreshing(true);
    requestedSetRef.current.clear();
    try {
      await onRefreshChanges?.();
      for (const entry of filteredEntries.slice(0, EAGER_DIFF_FILE_LIMIT)) {
        void loadSingleEntryDiff(entry, true);
      }
    } finally {
      setRefreshing(false);
    }
  };

  const handleSelectTreeFile = (entry: ChangeEntry) => {
    setScrollActiveFile(entry.relative);
    setCollapsedCards((prev) =>
      prev[entry.relative] === false ? prev : { ...prev, [entry.relative]: false },
    );
    onChooseFile(entry.relative);
    void loadSingleEntryDiff(entry);
    const el = cardRefs.current.get(entry.relative);
    if (el && typeof el.scrollIntoView === "function") {
      el.scrollIntoView({ behavior: "smooth", block: "start" });
    }
  };

  const handleStreamScroll = () => {
    const container = streamRef.current;
    if (!container) return;
    const containerTop = container.getBoundingClientRect().top;
    let bestRel = "";
    let bestOffset = Number.POSITIVE_INFINITY;

    for (const entry of filteredEntries) {
      const el = cardRefs.current.get(entry.relative);
      if (!el) continue;
      const rect = el.getBoundingClientRect();
      const dist = Math.abs(rect.top - containerTop - 12);
      if (rect.bottom > containerTop + 24 && dist < bestOffset) {
        bestOffset = dist;
        bestRel = entry.relative;
      }
    }
    if (bestRel && bestRel !== scrollActiveFile) {
      setScrollActiveFile(bestRel);
    }
  };

  const copyRelativePath = async (relative: string) => {
    try {
      await navigator.clipboard.writeText(relative);
      setCopiedPath(relative);
      window.setTimeout(() => {
        setCopiedPath((cur) => (cur === relative ? "" : cur));
      }, 1400);
    } catch {
      // 忽略剪贴板受限异常。
    }
  };

  const renderTreeFileRow = (entry: ChangeEntry, depth: number) => {
    const { baseName } = splitDirAndBase(entry.relative);
    const lang = fileLangMeta(entry.relative);
    const stat = statsByFile.get(entry.relative);
    const isCurrent = activeHighlightFile === entry.relative;

    return (
      <button
        key={entry.relative}
        type="button"
        title={`${entry.relative}${entry.agent_owned ? " · Agent 修改" : ""}`}
        style={{ paddingLeft: `${10 + depth * 12}px` }}
        className={"wb-cr-tree-file" + (isCurrent ? " is-active" : "")}
        onClick={() => handleSelectTreeFile(entry)}
      >
        <span className={`wb-cr-lang-tag tone-${lang.tone}`}>{lang.tag}</span>
        <span className="wb-cr-tree-filename">{baseName}</span>
        <span className="wb-cr-tree-stats">
          {stat && stat.add > 0 && <em className="is-add">+{stat.add}</em>}
          {stat && stat.del > 0 && <em className="is-del">-{stat.del}</em>}
          {entry.status === "untracked" && (
            <span className="wb-cr-new-badge" title="新增未跟踪文件">
              +
            </span>
          )}
          {entry.status === "deleted" && (!stat || stat.del === 0) && (
            <em className="is-del">已删</em>
          )}
          <i
            className={"wb-cr-status-dot" + (entry.agent_owned ? " is-agent" : "")}
            aria-hidden="true"
          />
        </span>
      </button>
    );
  };

  const renderDirNode = (node: DirTreeNode, depth: number): React.ReactNode => {
    if (!node.label) {
      return (
        <>
          {node.dirs.map((child) => renderDirNode(child, 0))}
          {node.files.map((file) => renderTreeFileRow(file, 0))}
        </>
      );
    }

    const isCollapsed = Boolean(collapsedDirs[node.path]);
    return (
      <div key={node.path} className="wb-cr-tree-dir-group">
        <button
          type="button"
          style={{ paddingLeft: `${8 + depth * 12}px` }}
          className="wb-cr-tree-dir"
          onClick={() =>
            setCollapsedDirs((prev) => ({ ...prev, [node.path]: !prev[node.path] }))
          }
        >
          <span className="wb-cr-chevron" aria-hidden="true">
            {isCollapsed ? "▸" : "▾"}
          </span>
          <span className="wb-cr-tree-dirname" title={node.path}>
            {node.label}
          </span>
        </button>
        {!isCollapsed && (
          <div className="wb-cr-tree-children">
            {node.dirs.map((child) => renderDirNode(child, depth + 1))}
            {node.files.map((file) => renderTreeFileRow(file, depth + 1))}
          </div>
        )}
      </div>
    );
  };

  return (
    <div className="wb-cr-shell">
      {/* 顶部 Codex 风格单行变更概览与图标工具栏 */}
      <div className="wb-cr-topbar">
        <div className="wb-cr-topbar-left">
          <span className="wb-cr-repo-name">{projectName}</span>

          <label className="wb-cr-scope-pill">
            <select
              aria-label="变更范围"
              value={scope}
              onChange={(e) => setScope(e.target.value as ReviewScope)}
            >
              <option value="all">未提交变更</option>
              <option value="unstaged">未暂存</option>
              <option value="staged">已暂存</option>
            </select>
            <span className="wb-cr-scope-caret" aria-hidden="true">
              ▾
            </span>
          </label>

          {(totalStats.add > 0 || totalStats.del > 0) && (
            <span className="wb-cr-total-stats" aria-label="总增删行数">
              <strong className="is-add">+{totalStats.add}</strong>
              <strong className="is-del">-{totalStats.del}</strong>
            </span>
          )}

          {changes?.branch && (
            <span className="wb-cr-branch-flow" title="当前 Git 分支与对比基线">
              <span className="wb-cr-branch-name">{changes.branch}</span>
              {changes.base_branch &&
                changes.base_branch !== changes.branch &&
                changes.base_branch !== `origin/${changes.branch}` && (
                  <>
                    <span className="wb-cr-branch-arrow" aria-hidden="true">
                      →
                    </span>
                    <span className="wb-cr-branch-base">{changes.base_branch}</span>
                  </>
                )}
            </span>
          )}
        </div>

        <div className="wb-cr-topbar-actions">
          <div className="wb-cr-icon-seg" role="group" aria-label="代码差异视图模式">
            <button
              type="button"
              className={"wb-cr-icon-btn" + (viewMode === "unified" ? " is-active" : "")}
              aria-pressed={viewMode === "unified"}
              title="内联单栏视图（代码宽幅不折行）"
              aria-label="内联单栏视图"
              onClick={() => updateViewMode("unified")}
            >
              <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" aria-hidden="true">
                <rect x="2.2" y="2.5" width="11.6" height="11" rx="1.8" />
                <path d="M5 6h6M5 8.5h6M5 11h4" />
              </svg>
            </button>
            <button
              type="button"
              className={"wb-cr-icon-btn" + (viewMode === "split" ? " is-active" : "")}
              aria-pressed={viewMode === "split"}
              title="左右双栏并排对比"
              aria-label="左右双栏对比"
              onClick={() => updateViewMode("split")}
            >
              <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" aria-hidden="true">
                <rect x="2" y="2.5" width="12" height="11" rx="1.8" />
                <path d="M8 2.5v11M4.2 6h2.2M9.6 6h2.2M4.2 9.2h2.2M9.6 9.2h2.2" />
              </svg>
            </button>
          </div>

          <button
            type="button"
            className="wb-cr-icon-btn"
            title={allCollapsed ? "展开全部变更文件" : "折叠全部变更文件"}
            aria-label={allCollapsed ? "展开全部变更文件" : "折叠全部变更文件"}
            onClick={toggleCollapseAll}
          >
            <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="m5.2 5.8 2.8-2.6 2.8 2.6M5.2 10.2 8 12.8l2.8-2.6" />
            </svg>
          </button>

          <button
            type="button"
            className="wb-cr-icon-btn"
            disabled={refreshing}
            title="刷新工作区变更"
            aria-label="刷新工作区变更"
            onClick={() => void handleRefresh()}
          >
            <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M13.2 8a5.2 5.2 0 1 1-1.3-3.4M13.2 3v2.6h-2.6" />
            </svg>
          </button>

          <button
            type="button"
            className={"wb-cr-icon-btn" + (treeOpen ? " is-active" : "")}
            aria-pressed={treeOpen}
            title={treeOpen ? "收起右侧已更改文件树" : "展开右侧已更改文件树"}
            aria-label={treeOpen ? "收起右侧已更改文件树" : "展开右侧已更改文件树"}
            onClick={toggleTreeOpen}
          >
            <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" aria-hidden="true">
              <rect x="2" y="2.5" width="12" height="11" rx="1.8" />
              <path d="M10.2 2.5v11" />
            </svg>
          </button>
        </div>
      </div>

      {/* 主体左右双区：左侧连续文件 Diff 卡片流 + 右侧层级变更目录树 */}
      <div className={"wb-cr-body" + (!treeOpen ? " is-tree-collapsed" : "")}>
        <div
          ref={streamRef}
          className="wb-cr-stream"
          onScroll={handleStreamScroll}
        >
          {filteredEntries.length === 0 ? (
            <div className="wb-cr-empty-state">
              {entries.length === 0
                ? "当前工作区在所选范围内没有文件变更。"
                : `没有匹配「${filterQuery.trim()}」的变更文件。`}
            </div>
          ) : (
            filteredEntries.map((entry) => {
              const { dirPrefix, baseName } = splitDirAndBase(entry.relative);
              const lang = fileLangMeta(entry.relative);
              const stat = statsByFile.get(entry.relative);
              const isCollapsed = isCardCollapsed(entry.relative);
              const rawText =
                scope === "all" && entry.relative === selectedFile && activeDiff
                  ? activeDiff
                  : diffByFile[entry.relative];
              const isLoading =
                Boolean(loadingByFile[entry.relative]) ||
                (rawText === undefined &&
                  scope === "all" &&
                  entry.relative === selectedFile &&
                  activeDiff === "");

              return (
                <section
                  key={entry.relative}
                  ref={(node) => {
                    if (node) cardRefs.current.set(entry.relative, node);
                    else cardRefs.current.delete(entry.relative);
                  }}
                  className={
                    "wb-cr-card" +
                    (activeHighlightFile === entry.relative ? " is-focused" : "") +
                    (isCollapsed ? " is-collapsed" : "")
                  }
                >
                  <header className="wb-cr-card-head">
                    <button
                      type="button"
                      className="wb-cr-card-title-btn"
                      title={entry.relative}
                      onClick={() => {
                        const willExpand = isCollapsed;
                        setCollapsedCards((prev) => ({
                          ...prev,
                          [entry.relative]: !isCollapsed,
                        }));
                        if (willExpand) {
                          void loadSingleEntryDiff(entry);
                        }
                      }}
                    >
                      <span className={`wb-cr-lang-tag tone-${lang.tone}`}>{lang.tag}</span>
                      <span className="wb-cr-card-path">
                        {dirPrefix && <span className="wb-cr-card-dir">{dirPrefix}</span>}
                        <strong className="wb-cr-card-base">{baseName}</strong>
                      </span>
                      {entry.agent_owned && (
                        <em className="wb-owner-tag is-agent">Agent</em>
                      )}
                    </button>

                    <div className="wb-cr-card-meta">
                      {stat && (stat.add > 0 || stat.del > 0) && (
                        <span className="wb-cr-card-stats">
                          {stat.add > 0 && <em className="is-add">+{stat.add}</em>}
                          {stat.del > 0 && <em className="is-del">-{stat.del}</em>}
                        </span>
                      )}

                      <button
                        type="button"
                        className="wb-cr-card-icon-btn"
                        title={copiedPath === entry.relative ? "已复制路径" : "复制相对路径"}
                        aria-label={`复制路径 ${entry.relative}`}
                        onClick={() => void copyRelativePath(entry.relative)}
                      >
                        {copiedPath === entry.relative ? (
                          <span style={{ fontSize: 11, fontWeight: 700 }}>✓</span>
                        ) : (
                          <svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                            <rect x="5.2" y="5.2" width="8.3" height="8.3" rx="1.5" />
                            <path d="M3.5 10.8H3a1.5 1.5 0 0 1-1.5-1.5V3A1.5 1.5 0 0 1 3 1.5h6.3A1.5 1.5 0 0 1 10.8 3v.5" />
                          </svg>
                        )}
                      </button>

                      <button
                        type="button"
                        className="wb-cr-card-icon-btn"
                        title="在独立标签页查看完整文件"
                        aria-label={`预览完整文件 ${entry.relative}`}
                        onClick={() => onOpenFileTab(entry.relative)}
                      >
                        <svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                          <path d="M9.5 2.5h4v4M13.3 2.7 7.8 8.2" />
                          <path d="M12.5 9.2v3.3a1 1 0 0 1-1 1h-8a1 1 0 0 1-1-1v-8a1 1 0 0 1 1-1h3.3" />
                        </svg>
                      </button>

                      <button
                        type="button"
                        className="wb-cr-card-icon-btn"
                        aria-label={isCollapsed ? `展开 ${entry.relative}` : `折叠 ${entry.relative}`}
                        onClick={() => {
                          const willExpand = isCollapsed;
                          setCollapsedCards((prev) => ({
                            ...prev,
                            [entry.relative]: !isCollapsed,
                          }));
                          if (willExpand) {
                            void loadSingleEntryDiff(entry);
                          }
                        }}
                      >
                        <span className="wb-cr-chevron" aria-hidden="true">
                          {isCollapsed ? "▸" : "⌄"}
                        </span>
                      </button>
                    </div>
                  </header>

                  {!isCollapsed && (
                    <div className="wb-cr-card-body">
                      {rawText === undefined ? (
                        isLoading ? (
                          <div className="wb-cr-empty-diff">正在读取文件差异…</div>
                        ) : (
                          <div className="wb-cr-empty-diff">
                            <button
                              type="button"
                              className="wb-file-open-btn"
                              onClick={() => void loadSingleEntryDiff(entry, true)}
                            >
                              加载此文件差异
                            </button>
                          </div>
                        )
                      ) : (
                        <DiffViewer
                          text={rawText}
                          relative={entry.relative}
                          workspace={workspace}
                          viewMode={viewMode}
                          maxInitialLines={180}
                          onComment={(line, body) =>
                            onSaveComment(line, body, entry.relative)
                          }
                        />
                      )}
                    </div>
                  )}
                </section>
              );
            })
          )}
        </div>

        {/* 右侧：可搜索层级变更文件树（「已更改的文件」） */}
        {treeOpen && (
          <aside className="wb-cr-sidebar" aria-label="已更改的文件目录树">
            <div className="wb-cr-search-box">
              <svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true">
                <circle cx="7" cy="7" r="4.5" />
                <path d="m10.4 10.4 3 3" />
              </svg>
              <input
                type="search"
                aria-label="筛选文件"
                value={filterQuery}
                placeholder="筛选文件..."
                onChange={(e) => setFilterQuery(e.target.value)}
              />
            </div>

            <div className="wb-cr-tree-scroll">
              <button
                type="button"
                className="wb-cr-group-head"
                onClick={() => setGroupOpen((open) => !open)}
              >
                <span className="wb-cr-chevron" aria-hidden="true">
                  {groupOpen ? "▾" : "▸"}
                </span>
                <span>已更改的文件</span>
                <small>{filteredEntries.length}</small>
              </button>

              {groupOpen && (
                <div className="wb-cr-tree-root">
                  {filteredEntries.length === 0 ? (
                    <div className="wb-cr-tree-empty">暂无匹配的变更文件</div>
                  ) : (
                    renderDirNode(dirTree, 0)
                  )}
                </div>
              )}
            </div>
          </aside>
        )}
      </div>
    </div>
  );
}
