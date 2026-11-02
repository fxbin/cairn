/**
 * V2 文件面板：工作区文件树浏览、受控预览与打开位置。
 * 预览经路径与敏感边界，二进制/截断/拒绝原因如实标注；打开位置失败关闭、
 * 错误可见，不可用目标禁用，不伪造成功状态。
 * Author: fxbin
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { desktop } from "../desktop/client";
import type { FileNode, FilePreview } from "../desktop/types";

/** 可打开的宿主目标投影，与 Go 侧 OpenLocationTarget 字段一致。 */
interface OpenLocationTarget {
  id: string;
  label: string;
  available: boolean;
}

/** 以 1024 进制换算可读大小，仅用于展示。 */
function formatBytes(size: number): string {
  if (!Number.isFinite(size) || size < 0) return "未知大小";
  if (size < 1024) return size + " B";
  if (size < 1024 * 1024) return (size / 1024).toFixed(1) + " KB";
  return (size / (1024 * 1024)).toFixed(1) + " MB";
}

/** 递归渲染文件树；目录可折叠，文件点击后进入预览。 */
function TreeNode({ node, depth, selected, expanded, onToggleDir, onOpenFile }: {
  node: FileNode;
  depth: number;
  selected: string;
  expanded: Set<string>;
  onToggleDir: (relative: string) => void;
  onOpenFile: (relative: string) => void;
}) {
  const isDir = node.kind === "dir" || node.kind === "directory";
  const open = expanded.has(node.relative);
  return <div className="v2-tree-node">
    <button className={(isDir ? "" : "v2-tree-file ") + (selected === node.relative ? "current" : "")}
      style={{ paddingLeft: 8 + depth * 13 }}
      title={node.relative}
      onClick={() => (isDir ? onToggleDir(node.relative) : onOpenFile(node.relative))}>
      <span className="v2-tree-mark" aria-hidden="true">{isDir ? (open ? "▾" : "▸") : "·"}</span>
      <span className="v2-tree-name">{node.name}</span>
    </button>
    {isDir && open && node.children?.map((child) => <TreeNode key={child.relative} node={child} depth={depth + 1}
      selected={selected} expanded={expanded} onToggleDir={onToggleDir} onOpenFile={onOpenFile}/>)}
  </div>;
}

export function FilePanelV2({ workspace, seed, onError }: {
  workspace: string;
  /** 从「检查变更」等入口带入的初始预览相对路径；空串表示无预置。 */
  seed: string;
  onError: (message: string) => void;
}) {
  const [tree, setTree] = useState<FileNode | null>(null);
  const [treeLoading, setTreeLoading] = useState(false);
  const [relative, setRelative] = useState("");
  const [preview, setPreview] = useState<FilePreview | null>(null);
  const [previewFailed, setPreviewFailed] = useState(false);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [locations, setLocations] = useState<OpenLocationTarget[]>([]);
  const [locationsOpen, setLocationsOpen] = useState(false);
  const [status, setStatus] = useState("");
  const generationRef = useRef(0);

  const toggleDir = useCallback((dirRelative: string) => {
    setExpanded((current) => {
      const next = new Set(current);
      if (next.has(dirRelative)) next.delete(dirRelative);
      else next.add(dirRelative);
      return next;
    });
  }, []);

  const openPreview = useCallback(async (target: string) => {
    if (!workspace || !target) return;
    const generation = ++generationRef.current;
    setRelative(target);
    setPreview(null);
    setPreviewFailed(false);
    setPreviewLoading(true);
    try {
      const result = await desktop.previewFile(workspace, target);
      if (generation !== generationRef.current) return;
      setPreview(result);
    } catch (err) {
      if (generation !== generationRef.current) return;
      setPreview(null);
      setPreviewFailed(true);
      onError(err instanceof Error ? err.message : String(err));
    } finally {
      if (generation === generationRef.current) setPreviewLoading(false);
    }
  }, [workspace, onError]);

  useEffect(() => {
    if (!workspace) return;
    const generation = ++generationRef.current;
    setTree(null);
    setTreeLoading(true);
    void desktop.listFileTree(workspace).then((root) => {
      if (generation !== generationRef.current) return;
      setTree(root);
      setTreeLoading(false);
      // 根目录默认展开一层，深级仍由用户显式展开。
      setExpanded((current) => {
        const next = new Set(current);
        next.add(root.relative);
        return next;
      });
    }).catch((err) => {
      if (generation !== generationRef.current) return;
      setTreeLoading(false);
      onError(err instanceof Error ? err.message : String(err));
    });
  }, [workspace, onError]);

  // 预置预览：展开祖先目录并加载目标文件，路径不可预览时按事实失败。
  useEffect(() => {
    if (!workspace || !seed) return;
    setExpanded((current) => {
      const next = new Set(current);
      const parts = seed.split("/");
      for (let index = 1; index < parts.length; index += 1) next.add(parts.slice(0, index).join("/"));
      return next;
    });
    void openPreview(seed);
  }, [workspace, seed, openPreview]);

  const openInEditor = async () => {
    if (!workspace || !relative) return;
    try {
      await desktop.openPathInEditor(workspace, relative);
      setStatus("已请求系统编辑器打开：" + relative);
    } catch (err) { onError(err instanceof Error ? err.message : String(err)); }
  };

  const toggleLocations = async () => {
    const next = !locationsOpen;
    setLocationsOpen(next);
    if (!next || locations.length) return;
    try { setLocations(await desktop.listOpenLocations(workspace)); }
    catch (err) { setLocations([]); onError(err instanceof Error ? err.message : String(err)); }
  };

  const openInTarget = async (target: OpenLocationTarget) => {
    if (!target.available) return;
    try {
      await desktop.openInLocation(workspace, relative || ".", target.id);
      setStatus("已在 " + target.label + " 打开：" + (relative || "工作区根目录"));
    } catch (err) { onError(err instanceof Error ? err.message : String(err)); }
    finally { setLocationsOpen(false); }
  };

  return <div className="v2-panel-body v2-file-panel" data-testid="v2-file-panel">
    <div className="v2-label">Workspace Files</div>
    <h2>文件</h2>
    <p>预览经路径与敏感边界；二进制或超大内容会被截断或拒绝，均如实标注。</p>
    <div className="v2-file-toolbar">
      <button className="v2-secondary" disabled={!relative} onClick={() => void openInEditor()}>在编辑器打开</button>
      <div className="v2-open-location">
        <button className="v2-secondary" aria-expanded={locationsOpen} onClick={() => void toggleLocations()}>打开位置 ▾</button>
        {locationsOpen && <div className="v2-open-location-menu" data-testid="v2-open-location-menu">
          {locations.length ? locations.map((target) => <button key={target.id} disabled={!target.available}
            data-testid={"v2-open-location-" + target.id} title={target.available ? target.label : target.label + "（不可用）"}
            onClick={() => void openInTarget(target)}>{target.label}{target.available ? "" : "（不可用）"}</button>)
            : <div className="v2-hint">没有可用目标；探测失败或宿主缺失时不伪造入口。</div>}
        </div>}
      </div>
    </div>
    <div className="v2-file-tree">
      {treeLoading && <div className="v2-hint">文件树读取中…</div>}
      {tree && <TreeNode node={tree} depth={0} selected={relative} expanded={expanded} onToggleDir={toggleDir} onOpenFile={(target) => void openPreview(target)}/>}
      {tree && tree.reason && <div className="v2-hint">文件树受限：{tree.reason}</div>}
      {!tree && !treeLoading && <div className="v2-empty">当前工作区没有可浏览的文件树。</div>}
    </div>
    <div className="v2-file-preview">
      <div className="v2-diff-title">{relative || "选择文件后预览"}</div>
      {previewLoading && <div className="v2-hint">预览读取中…</div>}
      {preview && <div className="v2-preview-meta" data-testid="v2-preview-meta">
        {[
          preview.language || null,
          formatBytes(preview.size),
          preview.binary ? "二进制未展示" : null,
          preview.truncated ? "内容已截断" : null,
          !preview.binary && !preview.text ? (preview.reason || "空文件") : preview.reason || null,
        ].filter((item): item is string => Boolean(item)).join(" · ")}
      </div>}
      {preview && !preview.binary && preview.text && <pre>{preview.text}</pre>}
      {preview && preview.binary && <div className="v2-empty">该文件为二进制内容，按安全边界不展示原文。</div>}
      {preview && !preview.binary && !preview.text && <div className="v2-empty">{preview.reason || "文件没有可展示的文本内容。"}</div>}
      {previewFailed && <div className="v2-empty" role="alert">无法预览该文件：请求被拒绝或读取失败，详见页面提示。</div>}
    </div>
    {status && <p className="v2-file-status" role="status">{status}</p>}
  </div>;
}
