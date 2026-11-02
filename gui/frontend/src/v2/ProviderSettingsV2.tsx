import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { ProviderProbeView } from "../desktop/types";

/** Provider 的连接、探测和准入是三种独立操作。探测通过不代表自动准入。 */
export default function ProviderSettingsV2({ workspace, notify }: { workspace: string; notify: (message: string) => void }) {
  const [presets, setPresets] = useState<Array<{ name: string; model: string; protocol: string; active: boolean; admission: string }>>([]);
  const [model, setModel] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [keyEnv, setKeyEnv] = useState("");
  const [reason, setReason] = useState("");
  const [probe, setProbe] = useState<ProviderProbeView | null>(null);
  const [busy, setBusy] = useState(false);
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let alive = true;
    void desktop.listProviderPresets({workspace}).then((items) => { if(alive)setPresets(items); })
      .catch((e) => { if(alive)notify(e instanceof Error?e.message:String(e)); });
    return () => {alive = false;};
  },[workspace,revision,notify]);
  const perform = async (action:()=>Promise<unknown>, notice:string) => {
    if (busy) return;
    setBusy(true);
    try { await action(); notify(notice); setRevision(n=>n+1); }
    catch (e) { notify(e instanceof Error?e.message:String(e)); }
    finally { setBusy(false); }
  };
  const validEnv = /^[A-Z_][A-Z0-9_]*$/.test(keyEnv);
  const canAdmit = probe?.result === "pass" && !!probe.evidence_fingerprint && !!probe.config_fingerprint &&
    !!probe.request_profile_fingerprint && probe.tool_roundtrip === "ok";
  return <div className="v2-provider-settings" data-testid="v2-provider-settings">
    <p>模型连接首先需要真实探测与准入。切换预设只影响下一次 Run，不改变已冻结运行。</p>
    {presets.map((item)=><div className="v2-setting-row" key={item.name}><div><strong>{item.name}</strong><p>{item.protocol} · {item.model} · {item.admission}</p></div><button disabled={busy||item.active} onClick={()=>void perform(()=>desktop.useProviderPreset(workspace,item.name),"预设已切换，仅影响后续运行。")}>{item.active?"当前连接":"启用预设"}</button></div>)}
    <h3>连接配置</h3>
    <div className="v2-form-grid">
      <label>Base URL<input aria-label="Provider Base URL" placeholder="https://…/v1" value={baseURL} onChange={(e)=>setBaseURL(e.target.value)}/></label>
      <label>模型 ID<input aria-label="Provider 模型 ID" value={model} onChange={(e)=>setModel(e.target.value)} placeholder="模型名称"/></label>
      <label>API Key 环境变量名称<input aria-label="API Key 环境变量名称" value={keyEnv} onChange={(e)=>setKeyEnv(e.target.value)} placeholder="MY_API_KEY"/></label>
    </div>
    <p className="v2-hint">这里只填写环境变量名，绝不粘贴或保存 API Key 明文。不覆盖已有配置。</p>
    <div className="v2-actions">
      <button disabled={busy||!baseURL.trim()||!model.trim()||!validEnv} onClick={()=>void perform(async()=>{
        await desktop.runProviderSetup({workspace,model:model.trim(),base_url:baseURL.trim(),api_key_env:keyEnv.trim(),overwrite:false,user_scope:false});
        setProbe(null);
      },"已生成连接配置；仍需独立探测和准入。")}>生成配置</button>
      <button disabled={busy||!workspace} onClick={()=>void perform(async()=>{
        const result=await desktop.probeProvider({workspace});
        setProbe(result);
      },"探测已完成，请根据回执判定是否准入。")}>探测当前 Provider</button>
    </div>
    {probe&&<div className="v2-provider-probe" role="status">
      <strong>探测结果：{probe.result}</strong>
      <p>模型：{probe.model||"未知"} · 工具回环：{probe.tool_roundtrip||"未报告"} · 用量流：{probe.usage_streamed?"已报告":"未知/未报告"}</p>
      {!canAdmit&&<p>当前证据不满足准入确认条件，不提供授权按钮。</p>}
      {canAdmit&&<><label>准入决定理由<textarea aria-label="准入决定理由" value={reason} onChange={(e)=>setReason(e.target.value)} placeholder="说明实测范围和你确认准入的依据…" /></label><button className="v2-primary" disabled={busy||!reason.trim()} onClick={()=>void perform(async()=>{
        await desktop.confirmProviderAdmission({workspace,evidence_fingerprint:probe.evidence_fingerprint!,config_fingerprint:probe.config_fingerprint!,request_profile_fingerprint:probe.request_profile_fingerprint!,reason:reason.trim()});
        setProbe(null);setReason("");
      },"用户确认准入已记录。")}>明确确认准入</button></>}
    </div>}
  </div>;
}
