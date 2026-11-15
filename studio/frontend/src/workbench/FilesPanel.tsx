/**
 * 项目文件只读浏览面板：提供有界文件树快速过滤与安全预览，外部编辑器打开动作走 desktop bridge。
 * Author: fxbin
 */
import { useEffect, useMemo, useState } from "react";
import { desktop } from "../desktop/client";
import type { FileNode, FilePreview } from "../desktop/types";
import OpenLocationIcon from "./OpenLocationIcon";

/**
 * 将字节大小格式化为人类可读的单位字符串。
 */
function humanSize(bytes?: number): string {
  if (bytes === undefined || bytes < 0 || !Number.isFinite(bytes)) return "未知";
  if (bytes < 1024) return String(bytes) + " B";
  return (bytes / 1024).toFixed(1) + " KB";
}

/**
 * 按关键字在客户端递归过滤文件树节点，保留名称或相对路径匹配的目录与文件分支。
 */
function filterFileTree(node: FileNode, rawQuery: string): FileNode | null {
  const query = rawQuery.trim().toLowerCase();
  if (!query) return node;
  if (node.kind !== "dir") {
    const target = (node.name + " " + node.relative).toLowerCase();
    return target.includes(query) ? node : null;
  }
  const dirSelfMatched = node.relative !== "." && (node.name + " " + node.relative).toLowerCase().includes(query);
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
 * 统计文件树节点中包含的普通文件总数。
 */
function countFiles(node: FileNode | null): number {
  if (!node) return 0;
  if (node.kind !== "dir") return 1;
  return (node.children || []).reduce((sum, child) => sum + countFiles(child), 0);
}

/**
 * 递归渲染单个目录或文件树节点，支持过滤态自动展开与当前选中态视觉指示。
 */
function TreeNode({
  node,
  selected,
  forceOpen,
  onSelect,
  level = 0,
}: {
  node: FileNode;
  selected: string;
  forceOpen: boolean;
  onSelect: (relative: string) => void;
  level?: number;
}) {
  const [open, setOpen] = useState(level < 2);
  const expanded = forceOpen || open;

  if (node.kind === "dir") {
    const childCount = node.children?.length || 0;
    return (
      <div>
        <button
          type="button"
          className="wb-file-entry"
          style={{ paddingLeft: 12 + level * 14, fontWeight: level === 0 ? 650 : 550 }}
          onClick={() => setOpen(!open)}
        >
          <span>{expanded ? "⌄" : "›"} ▤</span>
          <span>{node.name}</span>
          <small>{childCount}</small>
        </button>
        {expanded && node.children && node.children.length > 0 && (
          <div style={{ borderLeft: level >= 0 ? "1px dashed var(--wb-line)" : undefined, marginLeft: level === 0 ? 18 : 0 }}>
            {node.children.map((child) => (
              <TreeNode
                key={child.relative}
                node={child}
                selected={selected}
                forceOpen={forceOpen}
                level={level + 1}
                onSelect={onSelect}
              />
            ))}
          </div>
        )}
      </div>
    );
  }

  const isSelected = selected === node.relative;
  return (
    <div>
      <button
        type="button"
        className="wb-file-entry"
        style={{
          paddingLeft: 12 + level * 14,
          background: isSelected ? "var(--wb-accent-soft, var(--wb-side))" : undefined,
          color: isSelected ? "var(--wb-action)" : undefined,
          fontWeight: isSelected ? 650 : undefined,
        }}
        onClick={() => onSelect(node.relative)}
      >
        <span>◇</span>
        <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{node.name}</span>
        <small>{humanSize(node.size)}</small>
      </button>
    </div>
  );
}

/**
 * 项目文件只读视图；外部编辑器动作经已校验的 desktop bridge 分发，切换工作区时丢弃过期响应。
 */
export default function FilesPanel({
  workspace,
  notify,
}: {
  workspace: string;
  notify: (message: string) => void;
}) {
  const [tree, setTree] = useState<FileNode | null>(null);
  const [preview, setPreview] = useState<FilePreview | null>(null);
  const [selected, setSelected] = useState("");
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [locations, setLocations] = useState<Array<{ id: string; label: string; available: boolean }>>([]);

  useEffect(() => {
    let current = true;
    setTree(null);
    setPreview(null);
    setSelected("");
    setQuery("");
    setLoading(true);
    void desktop
      .listFileTree(workspace)
      .then((result) => {
        if (current) setTree(result);
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
  }, [workspace, notify]);

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
      return;
    }
    setPreview(null);
    void desktop
      .previewFile(workspace, selected)
      .then((result) => {
        if (active) setPreview(result);
      })
      .catch((e) => {
        if (active) notify(e instanceof Error ? e.message : String(e));
      });
    return () => {
      active = false;
    };
  }, [workspace, selected, notify]);

  const filteredTree = useMemo(() => (tree ? filterFileTree(tree, query) : null), [tree, query]);
  const visibleCount = useMemo(() => countFiles(filteredTree), [filteredTree]);

  const open = async (target?: string) => {
    if (!selected || busy) return;
    setBusy(true);
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

  return (
    <div className="wb-files" data-testid="files-panel">
      <div className="wb-files-tree">
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, padding: "10px 12px 6px" }}>
          <div className="wb-files-heading" style={{ padding: 0 }}>项目文件</div>
          {tree && <span className="wb-badge is-muted">{visibleCount} 个文件</span>}
        </div>
        <div style={{ padding: "0 10px 8px" }}>
          <input
            type="search"
            aria-label="过滤项目文件"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="按文件名或路径快速过滤…"
            style={{
              width: "100%",
              padding: "6px 10px",
              fontSize: 12,
              borderRadius: 8,
              border: "1px solid var(--wb-line)",
              background: "var(--wb-card, var(--wb-paper))",
              color: "var(--wb-title)",
              outline: "none",
            }}
          />
        </div>
        {loading && <div className="wb-hint">读取文件树…</div>}
        {filteredTree && (
          <TreeNode
            node={filteredTree}
            selected={selected}
            forceOpen={Boolean(query.trim())}
            onSelect={setSelected}
          />
        )}
        {!loading && tree && !filteredTree && <div className="wb-hint">未找到匹配「{query.trim()}」的文件。</div>}
        {!loading && !tree && <div className="wb-hint">没有可读取的文件。</div>}
      </div>
      <div className="wb-files-preview">
        {selected ? (
          <>
            <div className="wb-diff-title">
              <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{selected}</span>
              <div className="wb-file-open-actions" style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
                <button
                  type="button"
                  className="wb-file-open-btn"
                  aria-label="在编辑器中打开"
                  disabled={busy}
                  onClick={() => void open()}
                >
                  <OpenLocationIcon id="default" size={15} />
                  <span>在编辑器中打开</span>
                </button>
                {locations
                  .filter((x) => x.available)
                  .map((location) => (
                    <button
                      type="button"
                      className="wb-file-open-btn"
                      key={location.id}
                      disabled={busy}
                      onClick={() => void open(location.id)}
                    >
                      <OpenLocationIcon id={location.id} size={15} />
                      <span>用 {location.label} 打开</span>
                    </button>
                  ))}
              </div>
            </div>
            {!preview && <div className="wb-hint">正在读取文件预览…</div>}
            {preview && (
              <>
                <div className="wb-file-meta" data-testid="file-meta">
                  {preview.language || "未知语言"} · {humanSize(preview.size)} · {preview.truncated ? "内容已截断" : "完整预览"}
                  {preview.binary ? " · 二进制文件" : ""}
                </div>
                {preview.binary ? (
                  <div className="wb-empty" data-testid="binary-file">
                    二进制文件，不显示文本内容。
                  </div>
                ) : (
                  <pre className="wb-code">{preview.text || ""}</pre>
                )}
                {preview.reason && <p className="wb-hint">{preview.reason}</p>}
              </>
            )}
          </>
        ) : (
          <div className="wb-empty">从上方文件树选择文件查看只读预览。</div>
        )}
      </div>
    </div>
  );
}
