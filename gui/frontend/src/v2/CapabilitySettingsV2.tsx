import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";

/** 配置源与运行事实仍由 Go 端管理；这里不展示模拟能力。 */
type Skill = {name: string; scope?: string; description?: string; disabled?: boolean};
type Hook = {id: string; event: string; enabled: boolean; actions: Array<{kind: string}>};
type Memory = {id: string; kind: string; status: string; trusted_verification: boolean};
type Command = {name: string; description: string; scope: string};
type Tab = "skills" | "mcp" | "memory" | "hooks" | "commands" | "instructions";
const tabs: Array<[Tab, string]> = [["skills","Skills"],["mcp","MCP"],["memory","记忆"],["hooks","Hooks"],["commands","命令"],["instructions","项目指令"]];

export default function CapabilitySettingsV2({workspace, notify}:{workspace:string;notify:(message:string)=>void}) {
  const [tab,setTab]=useState<Tab>("skills");
  const [skills,setSkills]=useState<Skill[]>([]);
  const [hooks,setHooks]=useState<Hook[]>([]);
  const [memories,setMemories]=useState<Memory[]>([]);
  const [commands,setCommands]=useState<Command[]>([]);
  const [instructions,setInstructions]=useState<Array<{path:string;excerpt?:string}>>([]);
  const [mcp,setMcp]=useState<unknown>(null);
  const [version,setVersion]=useState(0);
  const [busy,setBusy]=useState(false);
  const [source,setSource]=useState("");
  const [scope,setScope]=useState("workspace");
  useEffect(()=>{
    if(!workspace)return;
    let live=true;
    void Promise.allSettled([
      desktop.listSkills({workspace}),desktop.listHooks(workspace),desktop.listMemories({workspace}),
      desktop.listCommands(),desktop.listProjectInstructions(workspace),desktop.mcpStatus({workspace}),
    ]).then((results)=>{
      if(!live)return;
      if(results[0].status==="fulfilled")setSkills(Array.isArray(results[0].value)?results[0].value as Skill[]:[]);
      if(results[1].status==="fulfilled")setHooks(results[1].value);
      if(results[2].status==="fulfilled")setMemories(results[2].value);
      if(results[3].status==="fulfilled")setCommands(results[3].value);
      if(results[4].status==="fulfilled")setInstructions(results[4].value.documents);
      if(results[5].status==="fulfilled")setMcp(results[5].value);
      if(results.some(x=>x.status==="rejected"))notify("部分能力状态读取失败，未将错误伪装成空列表。");
    });
    return ()=>{live=false;};
  },[workspace,version,notify]);
  const action=async(fn:()=>Promise<unknown>)=>{
    if(busy)return;
    setBusy(true);
    try{await fn();setVersion(v=>v+1);notify("更改已提交至本地配置。");}
    catch(e){notify(e instanceof Error?e.message:String(e));}
    finally{setBusy(false);}
  };
  const empty=(message:string)=><p className="v2-hint">{message}</p>;
  return <div className="v2-capabilities" data-testid="v2-capabilities">
    <div className="v2-capability-tabs">{tabs.map(([id,label])=><button key={id} className={tab===id?"current":""} onClick={()=>setTab(id)}>{label}</button>)}</div>
    <div className="v2-capability-body">
      {tab==="skills"&&<><p>真实 Skill Store；来源不等于授权，启用不会自动执行脚本。</p>
        <div className="v2-inline-form"><input aria-label="技能来源" placeholder="明确的技能来源路径" value={source} onChange={e=>setSource(e.target.value)}/><select value={scope} aria-label="技能作用域" onChange={e=>setScope(e.target.value)}><option value="workspace">当前项目</option><option value="user">当前用户</option></select><button disabled={busy||!source.trim()} onClick={()=>void action(async()=>{await desktop.installSkill(workspace,scope,source.trim());setSource("");})}>安装</button></div>
        {skills.map(item=><div className="v2-setting-row" key={item.scope+item.name}><div><strong>{item.name}</strong><p>{item.description||"无描述"} · {item.scope||"未知作用域"}</p></div><button disabled={busy||!item.scope} onClick={()=>void action(()=>desktop.setSkillEnabled(workspace,item.scope!,item.name,Boolean(item.disabled)))}>{item.disabled?"启用":"停用"}</button></div>)}
        {!skills.length&&empty("暂无已安装的 Skills。")}</>}
      {tab==="mcp"&&<><p>MCP 状态来自原生诊断。服务连接不意味着工具已被授权。</p><div className="v2-setting-row"><strong>可见服务器</strong><span>{Array.isArray(mcp)?mcp.length+" 个":"详细状态需诊断"}</span></div><p>此界面不回显敏感环境变量，服务器安装与权限配置继续由明确的配置入口管理。</p></>}
      {tab==="memory"&&<><p>记忆候选独立于 Journal，不能视作事实或自动学习结论。</p>{memories.map(x=><div className="v2-setting-row" key={x.id}><div><strong>{x.kind} · {x.id}</strong><p>{x.status}</p></div><span>{x.trusted_verification?"可信验证":"尚无可信验证"}</span></div>)}{!memories.length&&empty("当前工作区尚无候选记忆。")}<button className="v2-secondary" disabled={busy} onClick={()=>void action(()=>desktop.consolidateMemories({workspace}))}>整理候选</button></>}
      {tab==="hooks"&&<><p>Hooks 受运行时事件与动作白名单约束。</p>{hooks.map(x=><div className="v2-setting-row" key={x.id}><div><strong>{x.event}</strong><p>{x.actions.map(a=>a.kind).join("、")}</p></div><button disabled={busy||!x.enabled} onClick={()=>void action(()=>desktop.disableHook(x.id))}>{x.enabled?"停用":"已停用"}</button></div>)}{!hooks.length&&empty("没有已注册的 Hook。")}</>}
      {tab==="commands"&&<><p>只展示与 CLI 同源的内置命令，不虚构自定义命令能力。</p>{commands.map(x=><div className="v2-setting-row" key={x.name}><div><strong>{x.name}</strong><p>{x.description}</p></div><span>{x.scope}</span></div>)}</>}
      {tab==="instructions"&&<><p>AGENTS.md 为发现到的项目/用户规约，不执行文档内命令。</p>{instructions.map(x=><div className="v2-setting-row" key={x.path}><div><strong>{x.path}</strong><p>{x.excerpt||"无摘要"}</p></div></div>)}{!instructions.length&&empty("没有检测到项目规约。")}</>}
    </div>
  </div>;
}
