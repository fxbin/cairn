import { useState } from "react";
import type { RunHistoryView } from "../desktop/types";

/** Session metadata actions are explicit: a fork creates a NEW session, never silently continues. */
export default function SessionRowV2({run, selected, onSelect, onRename, onFlags, onFork}:{
  run: RunHistoryView;
  selected:boolean;
  onSelect:()=>void;
  onRename:(title:string)=>Promise<void>;
  onFlags:(pinned:boolean, archived:boolean)=>Promise<void>;
  onFork:()=>void;
}) {
  const [menu, setMenu] = useState(false);
  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState(run.title || run.task);
  const [busy, setBusy] = useState(false);
  const act=async(fn:()=>Promise<void>)=>{if(busy)return;setBusy(true);try{await fn();setMenu(false);setEditing(false);}finally{setBusy(false);}};
  return <div className={"v2-session-row"+(selected?" current":"")} data-testid="v2-session-row">
    {editing ? <div className="v2-session-rename">
      <input aria-label="会话名称" value={title} onChange={e=>setTitle(e.target.value)} onKeyDown={e=>{if(e.key==="Enter"&&title.trim())void act(()=>onRename(title.trim()));if(e.key==="Escape")setEditing(false);}}/>
      <button disabled={!title.trim()||busy} onClick={()=>void act(()=>onRename(title.trim()))}>保存</button>
      <button onClick={()=>setEditing(false)}>取消</button>
    </div> : <>
      <button className="v2-session-open" onClick={onSelect} aria-label={"打开会话 "+(run.title||run.task||run.run_id)}>
        {run.pinned && <span title="已固定">◆</span>}
        <span className="v2-session-name">{run.title||run.task||run.run_id}</span>
        {(run.waiting_approval||run.waiting_reconcile)&&<span className="v2-dot-warning" title="需要处理"/>}
      </button>
      <button className="v2-session-more" aria-label={"管理会话 "+run.run_id} aria-expanded={menu} onClick={()=>setMenu(x=>!x)}>···</button>
      {menu&&<div className="v2-session-menu">
        <button onClick={()=>{setTitle(run.title||run.task);setEditing(true);setMenu(false);}}>重命名</button>
        <button disabled={busy||!run.session_id} onClick={()=>void act(()=>onFlags(!run.pinned,run.archived))}>{run.pinned?"取消固定":"固定会话"}</button>
        <button disabled={busy||!run.session_id} onClick={()=>void act(()=>onFlags(run.pinned,!run.archived))}>{run.archived?"取消归档":"归档会话"}</button>
        <button disabled={!run.session_id} onClick={()=>{onFork();setMenu(false);}}>从此 Run 创建分支</button>
      </div>}
    </>}
  </div>;
}
