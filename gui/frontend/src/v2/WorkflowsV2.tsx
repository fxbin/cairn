import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";

type Workflow = {id:string;name:string;steps:Array<{task:string;model?:string;mode?:string}>};

/** Linear workflows are explicitly approved actions. A preview is not a completed run. */
export default function WorkflowsV2({workspace, notify}:{workspace:string;notify:(message:string)=>void}) {
 const [items,setItems]=useState<Workflow[]>([]);
 const [id,setId]=useState("");
 const [name,setName]=useState("");
 const [steps,setSteps]=useState<Array<{task:string;model?:string;mode?:string}>>([{task:""}]);
 const [busy,setBusy]=useState(false);
 const [revision,setRevision]=useState(0);
 useEffect(()=>{
   let active=true;
   void desktop.listWorkflows(workspace).then(data=>{if(active)setItems(data);})
     .catch(e=>{if(active)notify(e instanceof Error?e.message:String(e));});
   return ()=>{active=false;};
 },[workspace,revision,notify]);
 const edit=(item?:Workflow)=>{
   setId(item?.id||"");setName(item?.name||"");setSteps(item?item.steps.map(x=>({...x})):[{task:""}]);
 };
 const perform=async(job:()=>Promise<unknown>,message:string)=>{
   if(busy)return;setBusy(true);
   try{await job();setRevision(v=>v+1);notify(message);}
   catch(e){notify(e instanceof Error?e.message:String(e));}
   finally{setBusy(false);}
 };
 const valid=name.trim().length>0&&steps.length>0&&steps.length<=8&&steps.every(x=>x.task.trim().length>0);
 return <div className="v2-workflows" data-testid="v2-workflows">
  <p>工作流步骤按顺序运行；预演只检查执行计划，真实运行仍使用运行时的权限、验证与恢复合同。</p>
  <div className="v2-data-list">{items.map(item=><div key={item.id}><div><strong>{item.name}</strong><p>{item.steps.length} 个步骤</p></div><div className="v2-actions"><button onClick={()=>edit(item)}>编辑</button><button disabled={busy} onClick={()=>void perform(()=>desktop.runWorkflow(workspace,item.id,true),"已提交预演，未开始真实运行。")}>预演</button><button disabled={busy} onClick={()=>{
    if(!window.confirm("确认运行工作流？每个步骤都受现有权限和验证合同约束。"))return;
    void perform(()=>desktop.runWorkflow(workspace,item.id,false),"工作流运行请求已受理；请检查相应 Run 的 Journal 状态。");
  }}>运行</button></div></div>)}</div>
  <div className="v2-workflow-editor">
   <div className="v2-toolbar-lite"><h3>{id?"编辑工作流":"新工作流"}</h3><button onClick={()=>edit()}>新建</button></div>
   <label>工作流名称<input aria-label="工作流名称" value={name} onChange={e=>setName(e.target.value)} placeholder="例如：检查 → 修复 → 验证"/></label>
   <div className="v2-workflow-steps">{steps.map((step,i)=><div key={i}>
      <span>{i+1}</span><textarea aria-label={"第"+(i+1)+"步"} value={step.task} onChange={e=>setSteps(current=>current.map((x,j)=>i===j?{...x,task:e.target.value}:x))} placeholder="填写当前步骤的具体目标…"/>
      <button aria-label={"删除第"+(i+1)+"步"} disabled={steps.length===1} onClick={()=>setSteps(current=>current.filter((_,j)=>i!==j))}>×</button>
    </div>)}</div>
   <button className="v2-secondary" disabled={steps.length>=8} onClick={()=>setSteps(current=>current.length<8?current.concat({task:""}):current)}>＋ 添加步骤（最多 8 步）</button>
   <button className="v2-primary" disabled={busy||!valid} onClick={()=>void perform(async()=>{
      await desktop.saveWorkflow({id:id||undefined,workspace,name:name.trim(),steps:steps.map(x=>({...x,task:x.task.trim()}))});
      edit();
   },"工作流已保存到当前工作区。")}>保存工作流</button>
  </div>
 </div>;
}
