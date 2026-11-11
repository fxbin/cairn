import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { EvidenceView, RunHistoryView } from "../desktop/types";

type UsageRow = { run: RunHistoryView; evidence: EvidenceView | null; error?: string };
/** 用量从当前项目真实 Journal/Run Evidence 汇总；无报价就不计算费用。 */
export default function UsageV2({workspace,notify}:{workspace:string;notify:(message:string)=>void}) {
  const [rows,setRows]=useState<UsageRow[]>([]);
  const [loading,setLoading]=useState(false);
  const [limit,setLimit]=useState(20);
  useEffect(()=>{
    if(!workspace){setRows([]);return;}
    let active=true;
    setRows([]);
    setLoading(true);
    const run=async()=>{
      try {
        const all=await desktop.listRuns({workspace});
        const chosen=all.slice(0,limit);
        const loaded=await Promise.all(chosen.map(async(run)=>{
          try {const evidence=await desktop.evidence({workspace,run_id:run.run_id});return {run,evidence} as UsageRow;}
          catch(error){return {run,evidence:null,error:error instanceof Error?error.message:String(error)} as UsageRow;}
        }));
        if(active)setRows(loaded);
      }catch(error){if(active)notify(error instanceof Error?error.message:String(error));}
      finally{if(active)setLoading(false);}
    };
    void run();
    return()=>{active=false;};
  },[workspace,limit,notify]);
  const measured=rows.filter(x=>x.evidence!==null);
  const estimated=measured.some(x=>x.evidence?.metrics.usage_estimated);
  const input=measured.reduce((sum,x)=>sum+(x.evidence?.metrics.input_tokens||0),0);
  const output=measured.reduce((sum,x)=>sum+(x.evidence?.metrics.output_tokens||0),0);
  return <div className="v2-usage" data-testid="v2-usage">
    <p>数据源：当前项目 Journal 的单次运行证据。价格、缓存优惠和模型计费规则缺失时，费用显示为未知。</p>
    <div className="v2-usage-summary">
      <div><span>已读取 Run</span><strong>{measured.length}</strong></div>
      <div><span>输入 Token</span><strong>{input.toLocaleString()}</strong></div>
      <div><span>输出 Token</span><strong>{output.toLocaleString()}</strong></div>
      <div><span>费用</span><strong>未知</strong></div>
    </div>
    {estimated&&<p className="v2-hint">部分 Run 的用量是估算值；总和不能解释为 Provider 账单。</p>}
    {rows.some(x=>x.error)&&<p className="v2-hint">部分运行未能读取证据，未计入上方合计。</p>}
    {loading&&<p>读取中…</p>}
    <div className="v2-data-list">
      {rows.map(x=><div key={x.run.run_id}>
        <div><strong>{x.run.title||x.run.run_id}</strong><p>{x.run.phase_label||x.run.state}</p></div>
        <span>{x.evidence?((x.evidence.metrics.input_tokens+x.evidence.metrics.output_tokens).toLocaleString()+" tokens"+(x.evidence.metrics.usage_estimated?" · 估算":"")):"暂无可核实的用量"}</span>
      </div>)}
    </div>
    <button className="v2-secondary" onClick={()=>setLimit(n=>n+20)} disabled={loading}>多读取 20 条 Run</button>
  </div>;
}
