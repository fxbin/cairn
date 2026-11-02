import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { FileNode, FilePreview } from "../desktop/types";

function humanSize(bytes?: number): string {
  if (bytes === undefined || bytes < 0 || !Number.isFinite(bytes)) return "未知";
  if (bytes < 1024) return String(bytes) + " B";
  return (bytes / 1024).toFixed(1) + " KB";
}

function TreeNode({ node, onSelect, level = 0 }: { node: FileNode; onSelect: (relative: string) => void; level?: number }) {
  const [open, setOpen] = useState(level < 2);
  return <div>
    {node.kind === "dir" ? <><button className="v2-file-entry" style={{paddingLeft: 12 + level * 13}} onClick={() => setOpen(!open)}>
      <span>{open ? "⌄" : "›"} ▤</span><span>{node.name}</span>
    </button>{open && node.children?.map((child) => <TreeNode key={child.relative} node={child} level={level+1} onSelect={onSelect}/>)}</>
    : <button className="v2-file-entry" style={{paddingLeft: 12 + level * 13}} onClick={() => onSelect(node.relative)}><span>◇</span><span>{node.name}</span><small>{humanSize(node.size)}</small></button>}
  </div>;
}

/** Files are a read-only view; external editor action goes through the validated desktop bridge. */
export default function FilesPanelV2({ workspace, notify }: {workspace:string; notify:(message:string)=>void}) {
  const [tree, setTree] = useState<FileNode | null>(null);
  const [preview, setPreview] = useState<FilePreview | null>(null);
  const [selected, setSelected] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [locations, setLocations] = useState<Array<{id:string;label:string;available:boolean}>>([]);
  useEffect(() => {
    let current = true;
    setTree(null); setPreview(null); setSelected(""); setLoading(true);
    void desktop.listFileTree(workspace).then((result) => { if(current) setTree(result); })
      .catch((e) => { if(current) notify(e instanceof Error ? e.message : String(e)); })
      .finally(() => { if(current) setLoading(false); });
    return () => { current = false; };
  }, [workspace, notify]);
  useEffect(() => {let alive=true;void desktop.listOpenLocations(workspace).then(items=>{if(alive)setLocations(items);}).catch(()=>{if(alive)setLocations([]);});return()=>{alive=false;};},[workspace]);
  useEffect(() => {
    let active = true;
    if (!selected) { setPreview(null); return; }
    setPreview(null);
    void desktop.previewFile(workspace, selected).then((result) => { if(active) setPreview(result); })
      .catch((e) => { if(active) notify(e instanceof Error ? e.message : String(e)); });
    return () => { active = false; };
  }, [workspace, selected, notify]);
  const open = async (target?:string) => {
    if (!selected || busy) return;
    setBusy(true);
    try { if(target) await desktop.openInLocation(workspace,selected,target); else await desktop.openPathInEditor(workspace, selected); notify("已提交外部编辑器打开请求。"); }
    catch (e) {notify(e instanceof Error ? e.message : String(e));}
    finally { setBusy(false); }
  };
  return <div className="v2-files" data-testid="v2-files">
    <div className="v2-files-tree"><div className="v2-files-heading">项目文件</div>
      {loading && <div className="v2-hint">读取文件树…</div>}
      {tree && <TreeNode node={tree} onSelect={setSelected}/>}
      {!loading && !tree && <div className="v2-hint">没有可读取的文件。</div>}
    </div>
    <div className="v2-files-preview">
      {selected ? <><div className="v2-diff-title">{selected}
        <div className="v2-file-open-actions"><button aria-label="在编辑器中打开" disabled={busy} onClick={() => void open()}>在编辑器中打开</button>{locations.filter(x=>x.available).map(location=><button key={location.id} disabled={busy} onClick={()=>void open(location.id)}>用 {location.label} 打开</button>)}</div>
      </div>
        {!preview && <div className="v2-hint">正在读取文件预览…</div>}
        {preview && <><div className="v2-file-meta" data-testid="v2-file-meta">{preview.language || "未知语言"} · {humanSize(preview.size)} · {preview.truncated ? "内容已截断" : "完整预览"}{preview.binary ? " · 二进制文件" : ""}</div>
          {preview.binary ? <div className="v2-empty" data-testid="v2-binary">二进制文件，不显示文本内容。</div> : <pre className="v2-code">{preview.text || ""}</pre>}
          {preview.reason && <p className="v2-hint">{preview.reason}</p>}
        </>}
      </> : <div className="v2-empty">从左侧选择文件查看。</div>}
    </div>
  </div>;
}
