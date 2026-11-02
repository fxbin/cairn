import {beforeEach, describe, expect, it, vi} from "vitest";
import {act, fireEvent, render, screen, waitFor} from "@testing-library/react";

const bridge = vi.hoisted(() => ({
  listFileTree: vi.fn(), previewFile: vi.fn(), openPathInEditor: vi.fn(), listOpenLocations: vi.fn(), openInLocation: vi.fn(),
  listProviderPresets: vi.fn(), runProviderSetup: vi.fn(), probeProvider: vi.fn(), confirmProviderAdmission: vi.fn(), useProviderPreset: vi.fn(),
}));
vi.mock("../desktop/client", () => ({desktop:bridge}));
import FilesPanelV2 from "../v2/FilesPanelV2";
import ProviderSettingsV2 from "../v2/ProviderSettingsV2";
import SessionRowV2 from "../v2/SessionRowV2";
import type {RunHistoryView} from "../desktop/types";

const fileTree = {
  name:"demo", relative:".", path:"/tmp/demo",kind:"dir",
  children:[
    {name:"main.go",relative:"main.go",path:"/tmp/demo/main.go",kind:"file",size:2048},
    {name:"icon.png",relative:"icon.png",path:"/tmp/demo/icon.png",kind:"file",size:800}
  ],
};
const run:RunHistoryView={
 run_id:"r1",session_id:"s1",task:"测试代码",title:"测试代码",
 pinned:false,archived:false,state:"completed",phase:"completed",
 waiting_approval:false,waiting_reconcile:false,created_at:"",updated_at:"",
};
beforeEach(()=>{
  vi.clearAllMocks();
  bridge.listFileTree.mockResolvedValue(fileTree);
  bridge.previewFile.mockImplementation(async (_:string,path:string)=>path==="icon.png"
    ?{relative:path,path:"/tmp/demo/icon.png",size:800,language:"png",binary:true,truncated:false}
    :{relative:path,path:"/tmp/demo/main.go",size:2048,language:"go",binary:false,truncated:true,text:"package main"});
  bridge.openPathInEditor.mockResolvedValue({ok:true});
  bridge.listOpenLocations.mockResolvedValue([]);
  bridge.openInLocation.mockResolvedValue({ok:true});
  bridge.listProviderPresets.mockResolvedValue([]);
  bridge.runProviderSetup.mockResolvedValue({config_path:"/tmp/demo/.agent/config.yaml",notes:[],probe_ran:false});
  bridge.probeProvider.mockResolvedValue({result:"inconclusive",model:"m",tool_roundtrip:"not_invoked"});
  bridge.confirmProviderAdmission.mockResolvedValue({ok:true});
  bridge.useProviderPreset.mockResolvedValue({ok:true});
});

describe("V2 files parity with former InspectorRail",()=>{
 it("renders a file tree and preview with language, bytes and truncated flag", async()=>{
  render(<FilesPanelV2 workspace="/tmp/demo" notify={vi.fn()}/>);
  fireEvent.click(await screen.findByRole("button",{name:/main.go/}));
  expect(await screen.findByText("package main")).toBeInTheDocument();
  expect(screen.getByTestId("v2-file-meta")).toHaveTextContent("go");
  expect(screen.getByTestId("v2-file-meta")).toHaveTextContent("内容已截断");
  fireEvent.click(screen.getByRole("button",{name:"在编辑器中打开"}));
  await waitFor(()=>expect(bridge.openPathInEditor).toHaveBeenCalledWith("/tmp/demo","main.go"));
 });
 it("lists only supported editor locations and routes the action through the desktop bridge",async()=>{
  bridge.listOpenLocations.mockResolvedValue([{id:"vscode",label:"VS Code",available:true},{id:"missing",label:"Uninstalled",available:false}]);
  render(<FilesPanelV2 workspace="/tmp/demo" notify={vi.fn()}/>);
  fireEvent.click(await screen.findByRole("button",{name:/main.go/}));
  fireEvent.click(await screen.findByRole("button",{name:"用 VS Code 打开"}));
  await waitFor(()=>expect(bridge.openInLocation).toHaveBeenCalledWith("/tmp/demo","main.go","vscode"));
  expect(screen.queryByRole("button",{name:"用 Uninstalled 打开"})).not.toBeInTheDocument();
 });
 it("marks binary files and does not expose a text preview",async()=>{
  render(<FilesPanelV2 workspace="/tmp/demo" notify={vi.fn()}/>);
  fireEvent.click(await screen.findByRole("button",{name:/icon.png/}));
  expect(await screen.findByTestId("v2-binary")).toHaveTextContent("二进制文件");
  expect(screen.getByTestId("v2-file-meta")).toHaveTextContent("二进制文件");
 });
 it("rejects stale file responses after a workspace switch",async()=>{
  let finish!: (r:unknown)=>void;
  bridge.previewFile.mockReturnValueOnce(new Promise(resolve=>{finish=resolve;}));
  const notify=vi.fn();
  const view=render(<FilesPanelV2 workspace="/tmp/demo" notify={notify}/>);
  fireEvent.click(await screen.findByRole("button",{name:/main.go/}));
  view.rerender(<FilesPanelV2 workspace="/tmp/other" notify={notify}/>);
  await act(async()=>finish({relative:"main.go",path:"/tmp/demo/main.go",language:"go",size:9,binary:false,truncated:false,text:"FOREIGN_WORKSPACE_SECRET"}));
  expect(screen.queryByText("FOREIGN_WORKSPACE_SECRET")).not.toBeInTheDocument();
 });
});
describe("V2 session management parity",()=>{
 it("renames via an explicit action and preserves run ID",async()=>{
  const onRename=vi.fn(async()=>{});
  render(<SessionRowV2 run={run} selected={false} onSelect={vi.fn()} onRename={onRename} onFlags={vi.fn()} onFork={vi.fn()}/>);
  fireEvent.click(screen.getByRole("button",{name:"管理会话 r1"}));
  fireEvent.click(screen.getByRole("button",{name:"重命名"}));
  fireEvent.change(screen.getByRole("textbox",{name:"会话名称"}),{target:{value:"新的名称"}});
  fireEvent.click(screen.getByRole("button",{name:"保存"}));
  await waitFor(()=>expect(onRename).toHaveBeenCalledWith("新的名称"));
 });
 it("offers explicit pin/archive and distinct fork actions",async()=>{
  const onFlags=vi.fn(async()=>{}),onFork=vi.fn();
  render(<SessionRowV2 run={run} selected onSelect={vi.fn()} onRename={vi.fn()} onFlags={onFlags} onFork={onFork}/>);
  fireEvent.click(screen.getByRole("button",{name:"管理会话 r1"}));
  fireEvent.click(screen.getByRole("button",{name:"固定会话"}));
  await waitFor(()=>expect(onFlags).toHaveBeenCalledWith(true,false));
  fireEvent.click(screen.getByRole("button",{name:"管理会话 r1"}));
  fireEvent.click(screen.getByRole("button",{name:"从此 Run 创建分支"}));
  expect(onFork).toHaveBeenCalledOnce();
 });
});
describe("V2 Provider safety parity",()=>{
 it("requires a valid environment VARIABLE name instead of exposing a key body",()=>{
  render(<ProviderSettingsV2 workspace="/tmp/demo" notify={vi.fn()}/>);
  fireEvent.change(screen.getByRole("textbox",{name:"Provider Base URL"}),{target:{value:"http://127.0.0.1/v1"}});
  fireEvent.change(screen.getByRole("textbox",{name:"Provider 模型 ID"}),{target:{value:"local-model"}});
  fireEvent.change(screen.getByRole("textbox",{name:"API Key 环境变量名称"}),{target:{value:"sk-plaintext-secret"}});
  fireEvent.click(screen.getByRole("button",{name:"生成配置"}));
  expect(bridge.runProviderSetup).not.toHaveBeenCalled();
  expect(screen.getByRole("button",{name:"生成配置"})).toBeDisabled();
 });
 it("shows every setup catalog warning instead of treating a mismatched gateway price as verified",async()=>{
  bridge.runProviderSetup.mockResolvedValue({config_path:"/tmp/demo/.agent/config.yaml",catalog_matched:false,probe_ran:false,notes:["目录命中 2 个候选，取第一个","定价可能属于其他网关"]});
  render(<ProviderSettingsV2 workspace="/tmp/demo" notify={vi.fn()}/>);
  fireEvent.change(screen.getByRole("textbox",{name:"Provider Base URL"}),{target:{value:"https://provider.invalid/v1"}});
  fireEvent.change(screen.getByRole("textbox",{name:"Provider 模型 ID"}),{target:{value:"glm-4.6"}});
  fireEvent.change(screen.getByRole("textbox",{name:"API Key 环境变量名称"}),{target:{value:"MODEL_API_KEY"}});
  fireEvent.click(screen.getByRole("button",{name:"生成配置"}));
  const evidence=await screen.findByTestId("v2-provider-setup-evidence");
  expect(evidence).toHaveTextContent("目录未可靠命中");
  expect(evidence).toHaveTextContent("目录命中 2 个候选");
  expect(evidence).toHaveTextContent("定价可能属于其他网关");
 });
 it("configuration creation does NOT imply probe passed or admission",async()=>{
  const notify=vi.fn();
  render(<ProviderSettingsV2 workspace="/tmp/demo" notify={notify}/>);
  fireEvent.change(screen.getByRole("textbox",{name:"Provider Base URL"}),{target:{value:"http://127.0.0.1/v1"}});
  fireEvent.change(screen.getByRole("textbox",{name:"Provider 模型 ID"}),{target:{value:"local-model"}});
  fireEvent.change(screen.getByRole("textbox",{name:"API Key 环境变量名称"}),{target:{value:"MODEL_API_KEY"}});
  fireEvent.click(screen.getByRole("button",{name:"生成配置"}));
  await waitFor(()=>expect(bridge.runProviderSetup).toHaveBeenCalledWith(expect.objectContaining({workspace:"/tmp/demo",model:"local-model",api_key_env:"MODEL_API_KEY",overwrite:false})));
  expect(bridge.confirmProviderAdmission).not.toHaveBeenCalled();
 });
 it("only allows admission after a passed tool roundtrip + matching evidence fingerprints and human reason",async()=>{
  bridge.probeProvider.mockResolvedValue({result:"pass",model:"m",tool_roundtrip:"ok",usage_streamed:true,evidence_fingerprint:"ev",config_fingerprint:"cfg",request_profile_fingerprint:"profile"});
  render(<ProviderSettingsV2 workspace="/tmp/demo" notify={vi.fn()}/>);
  fireEvent.click(screen.getByRole("button",{name:"探测当前 Provider"}));
  const button=await screen.findByRole("button",{name:"明确确认准入"});
  expect(button).toBeDisabled();
  fireEvent.change(screen.getByRole("textbox",{name:"准入决定理由"}),{target:{value:"确认两轮工具调用"}});
  fireEvent.click(button);
  await waitFor(()=>expect(bridge.confirmProviderAdmission).toHaveBeenCalledWith({workspace:"/tmp/demo",reason:"确认两轮工具调用",evidence_fingerprint:"ev",config_fingerprint:"cfg",request_profile_fingerprint:"profile"}));
 });
 it("does not expose admission on inconclusive tool probe",async()=>{
  render(<ProviderSettingsV2 workspace="/tmp/demo" notify={vi.fn()}/>);
  fireEvent.click(screen.getByRole("button",{name:"探测当前 Provider"}));
  await screen.findByText(/探测结果：inconclusive/);
  expect(screen.queryByRole("button",{name:"明确确认准入"})).not.toBeInTheDocument();
 });
 it("ignores stale probe results from the previous workspace",async()=>{
  let finish!:(result:unknown)=>void;
  bridge.probeProvider.mockImplementation(()=>new Promise(resolve=>{finish=resolve;}));
  const notify=vi.fn(),view=render(<ProviderSettingsV2 workspace="/tmp/demo" notify={notify}/>);
  fireEvent.click(screen.getByRole("button",{name:"探测当前 Provider"}));
  view.rerender(<ProviderSettingsV2 workspace="/tmp/other" notify={notify}/>);
  await act(async()=>finish({result:"pass",model:"m",tool_roundtrip:"ok",evidence_fingerprint:"old",config_fingerprint:"old",request_profile_fingerprint:"old"}));
  expect(screen.queryByRole("button",{name:"明确确认准入"})).not.toBeInTheDocument();
  expect(bridge.confirmProviderAdmission).not.toHaveBeenCalled();
 });
});
