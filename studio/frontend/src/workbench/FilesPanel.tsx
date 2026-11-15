/**
 * 项目文件浏览与内建代码阅读器（FilesPanel）：采用左树右码分栏布局，
 * 点击任意可读源码直接在右侧全高阅读器中展示带行号正文与复制能力；
 * 外部编辑器动作收敛为右上角紧凑下拉菜单，敏感与受保护路径就地给出清晰说明。
 *
 * Author: fxbin
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { desktop } from "../desktop/client";
import type { FileNode, FilePreview } from "../desktop/types";
import OpenLocationIcon from "./OpenLocationIcon";

/**
 * 将字节体积格式化为紧凑可读字符串。
 */
function humanSize(bytes?: number): string {
  if (bytes === undefined || bytes < 0 || !Number.isFinite(bytes)) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * 根据文件名推导紧凑语言标识徽标与色彩分类。
 */
function fileExtBadge(name: string): { tag: string; tone: string } {
  const lower = name.toLowerCase();
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
 * 按关键字在客户端递归过滤文件树节点，保留名称或相对路径匹配的分支。
 */
function filterFileTree(node: FileNode, rawQuery: string): FileNode | null {
  const query = rawQuery.trim().toLowerCase();
  if (!query) return node;
  if (node.kind !== "dir") {
    const target = `${node.name} ${node.relative}`.toLowerCase();
    return target.includes(query) ? node : null;
  }
  const dirSelfMatched =
    node.relative !== "." && `${node.name} ${node.relative}`.toLowerCase().includes(query);
  if (dirSelfMatched) {
    return node;
  }
  const children = (node.children || [])
    .map((child) => filterFileTree(child, query))
    .filter((child): child is FileNode => child !== null);
  if (children.length > 0) {
    return { ...node, children };
  }
  return null;
}

/**
 * 统计文件树节点中包含的非目录条目总数。
 */
function countFiles(node: FileNode | null): number {
  if (!node) return 0;
  if (node.kind !== "dir") return 1;
  return (node.children || []).reduce((sum, child) => sum + countFiles(child), 0);
}

/**
 * 从文件树中挑选开箱默认预览的首选可读文本文件（优先 README / go.mod / AGENTS.md 等根文件）。
 */
function pickInitialReadableFile(root: FileNode | null): string {
  if (!root) return "";
  const topFiles = (root.children || []).filter(
    (item) => item.kind === "file" && !item.name.startsWith("."),
  );
  const preferredOrder = [
    "readme.md",
    "go.mod",
    "package.json",
    "makefile",
    "agents.md",
  ];
  for (const target of preferredOrder) {
    const hit = topFiles.find((f) => f.name.toLowerCase() === target);
    if (hit) return hit.relative;
  }
  if (topFiles.length > 0) return topFiles[0].relative;

  const stack: FileNode[] = [...(root.children || [])].reverse();
  while (stack.length > 0) {
    const cur = stack.pop()!;
    if (cur.kind === "file" && !cur.name.startsWith(".")) {
      return cur.relative;
    }
    if (cur.kind === "dir" && !cur.name.startsWith(".") && cur.children) {
      for (let i = cur.children.length - 1; i >= 0; i -= 1) {
        stack.push(cur.children[i]);
      }
    }
  }
  return "";
}

/**
 * 递归渲染单个目录或文件树节点；隐藏点目录默认折叠，避免敏感/配置目录干扰源码浏览。
 */
function TreeNode({
  node,
  selected,
  forceOpen,
  onSelect,
  onDoubleClickFile,
  level = 0,
}: {
  node: FileNode;
  selected: string;
  forceOpen: boolean;
  onSelect: (node: FileNode) => void;
  onDoubleClickFile?: (relative: string) => void;
  level?: number;
}) {
  const defaultExpanded = level === 0;
  const [open, setOpen] = useState(defaultExpanded);
  const expanded = forceOpen || open;

  if (node.kind === "dir") {
    const childCount = node.children?.length || 0;
    return (
      <div className="wb-fv-tree-group">
        <button
          type="button"
          className="wb-fv-tree-row is-dir"
          style={{ paddingLeft: `${8 + level * 12}px` }}
          onClick={() => setOpen((prev) => !prev)}
        >
          <span className="wb-cr-chevron" aria-hidden="true">
            {expanded ? "▾" : "▸"}
          </span>
          <svg
            width="13"
            height="13"
            viewBox="0 0 16 16"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.4"
            strokeLinecap="round"
            strokeLinejoin="round"
            className="wb-fv-folder-icon"
            aria-hidden="true"
          >
            <path d="M2.2 4.3c0-.6.5-1.1 1.1-1.1h2.7l1.2 1.4h5.5c.6 0 1.1.5 1.1 1.1v6c0 .6-.5 1.1-1.1 1.1H3.3c-.6 0-1.1-.5-1.1-1.1V4.3Z" />
          </svg>
          <span className="wb-fv-tree-name">{node.name}</span>
          <small className="wb-fv-tree-meta">{childCount}</small>
        </button>
        {expanded && node.children && node.children.length > 0 && (
          <div className="wb-fv-tree-children">
            {node.children.map((child) => (
              <TreeNode
                key={child.relative}
                node={child}
                selected={selected}
                forceOpen={forceOpen}
                level={level + 1}
                onSelect={onSelect}
                onDoubleClickFile={onDoubleClickFile}
              />
            ))}
          </div>
        )}
      </div>
    );
  }

  const isSelected = selected === node.relative;
  const isBlocked = node.kind === "blocked";
  const badge = fileExtBadge(node.name);
  const sizeText = isBlocked ? "受保护" : humanSize(node.size);

  return (
    <button
      type="button"
      className={
        "wb-fv-tree-row is-file" +
        (isSelected ? " is-active" : "") +
        (isBlocked ? " is-blocked" : "")
      }
      style={{ paddingLeft: `${12 + level * 12}px` }}
      title={isBlocked ? `${node.relative}（受安全边界保护）` : node.relative}
      onClick={() => onSelect(node)}
      onDoubleClick={() => {
        if (!isBlocked && onDoubleClickFile) {
          onDoubleClickFile(node.relative);
        }
      }}
    >
      <span className={`wb-cr-lang-tag tone-${badge.tone}`}>{badge.tag}</span>
      <span className="wb-fv-tree-name">{node.name}</span>
      {sizeText && <small className="wb-fv-tree-meta">{sizeText}</small>}
    </button>
  );
}

/**
 * 项目文件浏览与内建只读阅读器；外部编辑器动作收纳进紧凑下拉菜单，不遮挡文件内容。
 */
export default function FilesPanel({
  workspace,
  notify,
  onOpenInTab,
  autoSelectFirstFile = false,
}: {
  workspace: string;
  notify: (message: string) => void;
  onOpenInTab?: (relativePath: string) => void;
  autoSelectFirstFile?: boolean;
}) {
  const [tree, setTree] = useState<FileNode | null>(null);
  const [preview, setPreview] = useState<FilePreview | null>(null);
  const [selected, setSelected] = useState("");
  const [selectedNodeKind, setSelectedNodeKind] = useState("");
  const [selectedNodeReason, setSelectedNodeReason] = useState("");
  const [previewError, setPreviewError] = useState("");
  const [previewLoading, setPreviewLoading] = useState(false);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const [treeCollapsed, setTreeCollapsed] = useState(false);
  const [openMenuVisible, setOpenMenuVisible] = useState(false);
  const [locations, setLocations] = useState<
    Array<{ id: string; label: string; available: boolean }>
  >([]);
  const menuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onDocPointerDown = (event: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) {
        setOpenMenuVisible(false);
      }
    };
    document.addEventListener("mousedown", onDocPointerDown);
    return () => document.removeEventListener("mousedown", onDocPointerDown);
  }, []);

  useEffect(() => {
    let current = true;
    setTree(null);
    setPreview(null);
    setSelected("");
    setSelectedNodeKind("");
    setSelectedNodeReason("");
    setPreviewError("");
    setQuery("");
    setLoading(true);
    void desktop
      .listFileTree(workspace)
      .then((result) => {
        if (!current) return;
        setTree(result);
        if (autoSelectFirstFile) {
          const initial = pickInitialReadableFile(result);
          if (initial) {
            setSelected(initial);
            setSelectedNodeKind("file");
            setSelectedNodeReason("");
          }
        }
      })
      .catch((e) => {
        if (current) notify(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (current) setLoading(false);
      });
    return () => {
      current = false;
    };
  }, [workspace, notify, autoSelectFirstFile]);

  useEffect(() => {
    let alive = true;
    void desktop
      .listOpenLocations(workspace)
      .then((items) => {
        if (alive) setLocations(items);
      })
      .catch(() => {
        if (alive) setLocations([]);
      });
    return () => {
      alive = false;
    };
  }, [workspace]);

  useEffect(() => {
    let active = true;
    if (!selected) {
      setPreview(null);
      setPreviewError("");
      setPreviewLoading(false);
      return;
    }
    if (selectedNodeKind === "blocked") {
      setPreview(null);
      setPreviewLoading(false);
      setPreviewError(
        selectedNodeReason === "sensitive"
          ? "该文件属于敏感凭据或密钥文件（如 keys.env / .env），按安全边界禁止在工作台直接读取明文。"
          : "该路径受符号链接逃逸或工作区安全策略限制，不支持内联读取。",
      );
      return;
    }
    setPreview(null);
    setPreviewError("");
    setPreviewLoading(true);
    void desktop
      .previewFile(workspace, selected)
      .then((result) => {
        if (active) {
          setPreview(result);
          setPreviewError("");
        }
      })
      .catch((e) => {
        if (!active) return;
        const msg = e instanceof Error ? e.message : String(e);
        if (msg.includes("sensitive") || msg.includes("ignored")) {
          setPreviewError(
            "该文件位于受保护或忽略范围（如 .agent 内部配置或敏感凭据），按安全边界不在工作台内直接展示正文。",
          );
        } else {
          setPreviewError(msg);
          notify(msg);
        }
      })
      .finally(() => {
        if (active) setPreviewLoading(false);
      });
    return () => {
      active = false;
    };
  }, [workspace, selected, selectedNodeKind, selectedNodeReason, notify]);

  const filteredTree = useMemo(() => (tree ? filterFileTree(tree, query) : null), [tree, query]);
  const visibleCount = useMemo(() => countFiles(filteredTree), [filteredTree]);
  const availableLocations = useMemo(
    () => locations.filter((item) => item.available),
    [locations],
  );
  const codeLines = useMemo(() => {
    if (!preview || preview.binary || !preview.text) return [];
    return preview.text.replace(/\r\n/g, "\n").split("\n");
  }, [preview]);

  const open = async (target?: string) => {
    if (!selected || busy) return;
    setBusy(true);
    setOpenMenuVisible(false);
    try {
      if (target) {
        await desktop.openInLocation(workspace, selected, target);
      } else {
        await desktop.openPathInEditor(workspace, selected);
      }
      notify("已提交外部编辑器打开请求。");
    } catch (e) {
      notify(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const copyContent = async () => {
    if (!preview?.text) return;
    try {
      await navigator.clipboard.writeText(preview.text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1400);
    } catch {
      notify("复制文件内容失败。");
    }
  };

  const handleSelectNode = (node: FileNode) => {
    setSelected(node.relative);
    setSelectedNodeKind(node.kind);
    setSelectedNodeReason(node.reason || "");
    setOpenMenuVisible(false);
  };

  const selectedBadge = selected ? fileExtBadge(selected) : null;

  return (
    <div
      className={"wb-files wb-fv-shell" + (treeCollapsed ? " is-tree-collapsed" : "")}
      data-testid="files-panel"
    >
      {/* 左侧目录树导航栏 */}
      {!treeCollapsed && (
        <aside className="wb-fv-sidebar">
          <div className="wb-fv-sidebar-top">
            <span className="wb-fv-sidebar-title">项目文件</span>
            {tree && <small className="wb-fv-sidebar-count">{visibleCount}</small>}
          </div>

          <div className="wb-cr-search-box wb-fv-search">
            <svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true">
              <circle cx="7" cy="7" r="4.5" />
              <path d="m10.4 10.4 3 3" />
            </svg>
            <input
              type="search"
              aria-label="过滤项目文件"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="过滤文件名或路径..."
            />
          </div>

          <div className="wb-fv-tree-scroll">
            {loading && <div className="wb-hint">正在读取目录树…</div>}
            {filteredTree && (
              <TreeNode
                node={filteredTree}
                selected={selected}
                forceOpen={Boolean(query.trim())}
                onSelect={handleSelectNode}
                onDoubleClickFile={onOpenInTab}
              />
            )}
            {!loading && tree && !filteredTree && (
              <div className="wb-hint">未找到匹配「{query.trim()}」的文件。</div>
            )}
            {!loading && !tree && <div className="wb-hint">没有可读取的文件。</div>}
          </div>
        </aside>
      )}

      {/* 右侧全高内建文件内容阅读区 */}
      <section className="wb-fv-reader">
        <header className="wb-fv-topbar">
          <div className="wb-fv-topbar-left">
            <button
              type="button"
              className={"wb-cr-icon-btn" + (!treeCollapsed ? " is-active" : "")}
              title={treeCollapsed ? "展开文件目录树" : "收起文件目录树"}
              aria-label={treeCollapsed ? "展开文件目录树" : "收起文件目录树"}
              onClick={() => setTreeCollapsed((prev) => !prev)}
            >
              <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.45" strokeLinecap="round" aria-hidden="true">
                <rect x="2" y="2.5" width="12" height="11" rx="1.8" />
                <path d="M6.2 2.5v11" />
              </svg>
            </button>

            {selected ? (
              <div className="wb-fv-crumb" title={selected}>
                {selectedBadge && (
                  <span className={`wb-cr-lang-tag tone-${selectedBadge.tone}`}>
                    {selectedBadge.tag}
                  </span>
                )}
                <span className="wb-fv-crumb-path">{selected}</span>
              </div>
            ) : (
              <span className="wb-fv-crumb-placeholder">选择左侧文件直接阅读内容</span>
            )}
          </div>

          {selected && !previewError && (
            <div className="wb-fv-topbar-actions">
              {preview && !preview.binary && preview.text && (
                <button
                  type="button"
                  className="wb-fv-ghost-btn"
                  title="复制文件全文"
                  onClick={() => void copyContent()}
                >
                  {copied ? "已复制" : "复制"}
                </button>
              )}

              {onOpenInTab && (
                <button
                  type="button"
                  className="wb-fv-ghost-btn"
                  title="固定为独立文件标签页"
                  onClick={() => onOpenInTab(selected)}
                >
                  新标签
                </button>
              )}

              <div ref={menuRef} className="wb-fv-ext-split">
                <button
                  type="button"
                  className="wb-fv-ghost-btn"
                  aria-label="在编辑器中打开"
                  title="使用默认外部编辑器打开"
                  disabled={busy}
                  onClick={() => void open()}
                >
                  <OpenLocationIcon id="default" size={13} />
                  <span>外部打开</span>
                </button>
                {availableLocations.length > 0 && (
                  <button
                    type="button"
                    className="wb-fv-ghost-btn is-caret"
                    aria-label="选择外部打开方式"
                    aria-expanded={openMenuVisible}
                    disabled={busy}
                    onClick={() => setOpenMenuVisible((v) => !v)}
                  >
                    ▾
                  </button>
                )}

                {availableLocations.length > 0 && (
                  <div
                    className={
                      "wb-fv-open-menu" + (openMenuVisible ? " is-open" : "")
                    }
                  >
                    {availableLocations.map((location) => (
                      <button
                        type="button"
                        className="wb-fv-open-menu-item"
                        key={location.id}
                        disabled={busy}
                        onClick={() => void open(location.id)}
                      >
                        <OpenLocationIcon id={location.id} size={14} />
                        <span>用 {location.label} 打开</span>
                      </button>
                    ))}
                  </div>
                )}
              </div>
            </div>
          )}
        </header>

        <div className="wb-fv-body">
          {!selected && (
            <div className="wb-fv-empty">
              点击左侧文件树中的任意源码或文档，即可直接在此阅读完整内容（双击可拆分为独立标签页）。
            </div>
          )}

          {selected && previewLoading && !preview && !previewError && (
            <div className="wb-fv-empty">正在读取文件内容…</div>
          )}

          {selected && previewError && (
            <div className="wb-fv-protected-card" role="status">
              <strong>受安全边界保护，不直接显示正文</strong>
              <p>{previewError}</p>
            </div>
          )}

          {selected && preview && !previewError && (
            <>
              <div className="wb-fv-meta-bar" data-testid="file-meta">
                <span>{preview.language || "文本"}</span>
                <span>·</span>
                <span>{humanSize(preview.size) || "0 B"}</span>
                <span>·</span>
                <span>{preview.truncated ? "内容已截断" : "完整只读预览"}</span>
                {preview.binary && (
                  <>
                    <span>·</span>
                    <span>二进制文件</span>
                  </>
                )}
              </div>

              {preview.binary ? (
                <div className="wb-fv-empty" data-testid="binary-file">
                  二进制文件，不支持文本直接预览；如需查看可使用右上角「外部打开」。
                </div>
              ) : (
                <div className="wb-fv-code-scroll">
                  <table className="wb-fv-code-table">
                    <tbody>
                      {codeLines.map((line, index) => (
                        <tr key={index} className="wb-fv-code-row">
                          <td className="wb-fv-line-no">{index + 1}</td>
                          <td className="wb-fv-line-code">{line || " "}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}

              {preview.reason && !preview.binary && (
                <p className="wb-hint" style={{ padding: "8px 14px" }}>
                  {preview.reason}
                </p>
              )}
            </>
          )}
        </div>
      </section>
    </div>
  );
}
