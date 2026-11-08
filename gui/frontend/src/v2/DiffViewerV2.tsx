/**
 * 统一 Diff 阅读器：把原始 Git 文本拆为可浏览的差异行，不改变工作区。
 * 行级评论是本地 review metadata；真正修改仍必须由后续 Agent Run 执行。
 * Author: fxbin
 */
import { useEffect, useMemo, useState } from "react";
import { desktop } from "../desktop/client";

type Line = { key: number; kind: "add" | "delete" | "context" | "header"; number: number | null; text: string };

function linesOf(diff: string): Line[] {
  let oldNo = 0;
  let newNo = 0;
  return diff.split("\n").map((text, index) => {
    const hunk = text.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (hunk) {
      oldNo = Number(hunk[1]);
      newNo = Number(hunk[2]);
      return { key: index, kind: "header", number: null, text };
    }
    if (/^(diff --git |index |--- |\+\+\+ |new file mode |deleted file mode |\\ No newline)/.test(text)) return { key: index, kind: "header", number: null, text };
    if (text.startsWith("+")) return { key: index, kind: "add", number: newNo++, text };
    if (text.startsWith("-")) return { key: index, kind: "delete", number: oldNo++, text };
    if (oldNo > 0 && newNo > 0) { oldNo += 1; return { key: index, kind: "context", number: newNo++, text }; }
    return { key: index, kind: "header", number: null, text };
  });
}

export default function DiffViewerV2({ text, relative, workspace, onComment }: {
  text: string;
  relative: string;
  workspace: string;
  onComment: (line: string, body: string) => Promise<void>;
}) {
  const [selected, setSelected] = useState<number | null>(null);
  const [note, setNote] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [existing, setExisting] = useState<Array<{id:string;relative:string;line?:string;body:string}>>([]);
  useEffect(()=>{let active=true;void desktop.listReviewNotes(workspace,relative).then(data=>{if(active)setExisting(data);}).catch(()=>{if(active)setExisting([]);});return()=>{active=false;};},[workspace,relative]);
  const lines = useMemo(() => linesOf(text), [text]);
  return <div className="v2-diff-viewer">
    <div className="v2-diff-lines">
      {lines.map((line) => <div key={line.key} className={"v2-diff-row v2-diff-" + line.kind}>
        <button aria-label={line.number === null ? "差异标头" : "为第 " + line.number + " 行添加意见"} disabled={line.number === null} onClick={() => { setSelected(line.number); setError(""); }} className="v2-diff-line-number">{line.number ?? ""}</button>
        <code>{line.text || " "}</code>
      </div>)}
    </div>
    {existing.length>0 && <div className="v2-review-notes" data-testid="v2-review-notes"><strong>已保存的审查备注</strong>{existing.map(item=><p key={item.id}>第 {item.line || "?"} 行 · {item.body}</p>)}</div>}
    {selected !== null && <div className="v2-line-comment">
      <div><strong>{relative}</strong><span>第 {selected} 行</span><button aria-label="关闭批注编辑" onClick={() => setSelected(null)}>×</button></div>
      <textarea aria-label="行级审查意见" value={note} onChange={(event) => setNote(event.target.value)} placeholder="描述需要进一步检查或修改的地方…" />
      {error && <p role="alert">{error}</p>}
      <div className="v2-actions"><button className="v2-primary" disabled={!note.trim() || saving} onClick={() => {
        if (!note.trim()) return;
        setSaving(true);
        void onComment(String(selected), note.trim()).then(() => { setNote(""); setSelected(null); }).catch((err) => setError(err instanceof Error ? err.message : String(err))).finally(() => setSaving(false));
      }}>{saving ? "保存中…" : "保存并带回会话"}</button><button onClick={() => setSelected(null)}>取消</button></div>
    </div>}
  </div>;
}
