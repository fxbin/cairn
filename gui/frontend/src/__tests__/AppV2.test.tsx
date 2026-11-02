import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const mock = vi.hoisted(() => ({
  health: vi.fn(async () => ({ok:true})),
  listRecentProjects: vi.fn(),
  rememberProject: vi.fn(),
  observeWorkspace: vi.fn(),
  listRuns: vi.fn(),
  inspectRun: vi.fn(),
  pickDirectory: vi.fn(),
  startRun: vi.fn(),
  resumeRun: vi.fn(),
  resolveReconcile: vi.fn(),
  abortRun: vi.fn(),
  cancelRun: vi.fn(),
  submitQueue: vi.fn(),
  listQueue: vi.fn(),
  confirmQueue: vi.fn(),
  cancelQueue: vi.fn(),
  submitSteering: vi.fn(),
  listChanges: vi.fn(),
  diffFile: vi.fn(),
  evidence: vi.fn(),
  saveReviewNote: vi.fn(),
  listProviderPresets: vi.fn(),
  listWorkflows: vi.fn(),
  onStream: vi.fn((_handler?: (event: unknown) => void) => () => {}),
}));
vi.mock("../desktop/client", () => ({ desktop: mock }));
import AppV2 from "../v2/AppV2";
import { ReconciliationV2 } from "../v2/RecoveryV2";
import { QueuePanelV2 } from "../v2/QueuePanelV2";

const sampleRun = (state = "completed", reconcile = false) => ({
  run_id: "r1",
  session_id: "session1",
  task: "修复登陆",
  title: "修复登陆",
  state, phase: state,
  updated_at: "2026-11-02T00:00:00Z",
  created_at: "2026-11-02T00:00:00Z",
  waiting_approval: false,
  waiting_reconcile: reconcile,
  pinned: false, archived: false,
});

function setup() {
  mock.listRecentProjects.mockResolvedValue([{ path: "/tmp/cairn-a", name: "cairn-a", pinned: true, archived: false, last_used: "" }]);
  mock.rememberProject.mockResolvedValue({ ok: true });
  mock.observeWorkspace.mockResolvedValue({ path: "/tmp/cairn-a", exists: true, has_config: true, config_loaded: true, dirty: false });
  mock.listRuns.mockResolvedValue([sampleRun()]);
  mock.inspectRun.mockResolvedValue({ run_id: "r1", state: "completed", summary: {}, events: [] });
  mock.listChanges.mockResolvedValue({ root: "/tmp/cairn-a", entries: [{ relative: "README.md", status: "modified", agent_owned: true }], added: 0, deleted: 0, modified: 1, untracked: 0 });
  mock.diffFile.mockResolvedValue("diff --git a/README.md b/README.md\n@@ -1,1 +1,1 @@\n-old\n+new");
  mock.evidence.mockResolvedValue({ state: "completed", outcome: "ok", trusted: false, verification: { verification_ok: false }, metrics: { input_tokens: 123, output_tokens: 42, usage_estimated: true } });
  mock.listProviderPresets.mockResolvedValue([]);
  mock.listWorkflows.mockResolvedValue([]);
  mock.listQueue.mockResolvedValue([]);
  mock.saveReviewNote.mockResolvedValue({ ok: true });
  mock.onStream.mockReturnValue(() => {});
}

describe("Cairn Desktop 2.0 — real bridge contracts", () => {
  beforeEach(() => { vi.clearAllMocks(); setup(); });

  it("displays composer-first home with real workspace and no fake runs", async () => {
    render(<AppV2 />);
    expect(screen.getByTestId("cairn-v2-shell")).toBeInTheDocument();
    await waitFor(() => expect(mock.listRuns).toHaveBeenCalledWith({ workspace: "/tmp/cairn-a" }));
    expect(screen.getByRole("heading", { name: "今天想让 Cairn 做什么？" })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "描述任务" })).toBeInTheDocument();
  });

  it("preserves linked session ID when continuing a completed Run", async () => {
    mock.startRun.mockResolvedValue({ run_id: "r2", state: "starting" });
    render(<AppV2 />);
    await screen.findByRole("button", { name: /修复登陆/ });
    fireEvent.click(screen.getByRole("button", { name: /修复登陆/ }));
    fireEvent.change(screen.getByRole("textbox", { name: "描述任务" }), { target: { value: "写测试" } });
    fireEvent.click(screen.getByRole("button", { name: "提交任务" }));
    await waitFor(() => expect(mock.startRun).toHaveBeenCalledWith(expect.objectContaining({
      workspace: "/tmp/cairn-a", task: "写测试", session_id: "session1", prev_run_id: "r1", require_session_tail: true,
    })));
  });

  it("opens right-hand diff and saves a review note without executing it", async () => {
    render(<AppV2 />);
    await screen.findByRole("button", { name: /修复登陆/ });
    fireEvent.click(screen.getByRole("button", { name: /修复登陆/ }));
    fireEvent.click(screen.getByRole("button", { name: "检查变更" }));
    await waitFor(() => expect(mock.diffFile).toHaveBeenCalledWith("/tmp/cairn-a", "README.md"));
    const lineButton = (await screen.findAllByRole("button", { name: "为第 1 行添加意见" }))[0];
    fireEvent.click(lineButton);
    await userEvent.type(screen.getByRole("textbox", { name: "行级审查意见" }), "请核对变更");
    fireEvent.click(screen.getByRole("button", { name: "保存并带回会话" }));
    await waitFor(() => expect(mock.saveReviewNote).toHaveBeenCalledWith(expect.objectContaining({
      workspace: "/tmp/cairn-a", relative: "README.md", line: "1", body: "请核对变更",
    })));
    expect(mock.startRun).not.toHaveBeenCalled();
  });

  it("does not offer normal retry for a reconcile-required run", async () => {
    mock.listRuns.mockResolvedValue([sampleRun("waiting_reconcile", true)]);
    mock.inspectRun.mockResolvedValue({ run_id: "r1", state: "waiting_reconcile", events: [], summary: {} });
    render(<AppV2 />);
    await screen.findByRole("button", { name: /修复登陆/ });
    fireEvent.click(screen.getByRole("button", { name: /修复登陆/ }));
    expect(await screen.findByTestId("v2-reconcile")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "提交人工对账" })).toBeDisabled();
    expect(mock.resumeRun).not.toHaveBeenCalled();
  });
});

describe("V2 safety-first action panels", () => {
  beforeEach(() => { vi.clearAllMocks(); setup(); });

  it("requires operation, reason and evidence before recording a reconciliation", async () => {
    mock.resolveReconcile.mockResolvedValue({ state: "completed" });
    render(<ReconciliationV2 workspace="/tmp/cairn-a" runId="r1" events={[]} onDone={vi.fn()} onError={vi.fn()} />);
    const submit = screen.getByRole("button", { name: "提交人工对账" });
    expect(submit).toBeDisabled();
    fireEvent.change(screen.getByLabelText("操作 ID"), { target: { value: "op1" } });
    fireEvent.change(screen.getByRole("textbox", { name: "人工核查说明" }), { target: { value: "已检查本地文件" } });
    expect(submit).toBeDisabled();
    fireEvent.change(screen.getByRole("textbox", { name: "证据引用" }), { target: { value: "git:abcd" } });
    fireEvent.click(submit);
    await waitFor(() => expect(mock.resolveReconcile).toHaveBeenCalledWith({
      workspace: "/tmp/cairn-a", run_id: "r1", operation_id: "op1",
      verdict: "abort_run", note: "已检查本地文件", evidence: "git:abcd",
    }));
    expect(mock.resumeRun).not.toHaveBeenCalled();
  });

  it("only starts a pending queued task after explicit confirmation", async () => {
    mock.listQueue.mockResolvedValue([{ input_id: "q1", state: "pending", content_preview: "补测试" }]);
    mock.confirmQueue.mockResolvedValue({ run_id: "r3", state: "running" });
    const onRun = vi.fn();
    render(<QueuePanelV2 workspace="/tmp/cairn-a" revision={0} onRun={onRun} onError={vi.fn()} />);
    await screen.findByText("补测试");
    expect(mock.confirmQueue).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "确认启动" }));
    await waitFor(() => expect(mock.confirmQueue).toHaveBeenCalledWith({ workspace: "/tmp/cairn-a", input_id: "q1" }));
    await waitFor(() => expect(onRun).toHaveBeenCalledWith("r3"));
  });
});


describe("V2 interaction parity — no legacy App imports", () => {
  beforeEach(() => {vi.clearAllMocks();setup();});
  const event=(runId:string,seq:number,type:string,payload:Record<string,unknown>={},operation_id?:string)=>({
    schema_version:1,event_id:runId+":"+seq,run_id:runId,workspace_hash:"w",seq,
    occurred_at:"2026-11-02T04:00:00Z",type,payload,operation_id,prev_hash:"",event_hash:"",
  });
  it("Ctrl+Enter queues exactly once and never invokes startRun",async()=>{
    mock.submitQueue.mockResolvedValue({input_id:"q",state:"pending"});
    render(<AppV2/>);
    await screen.findByRole("heading",{name:"今天想让 Cairn 做什么？"});
    await waitFor(()=>expect(mock.listRuns).toHaveBeenCalled());
    const draft=screen.getByRole("textbox",{name:"描述任务"});
    fireEvent.change(draft,{target:{value:"排队执行测试"}});
    fireEvent.keyDown(draft,{key:"Enter",ctrlKey:true});
    await waitFor(()=>expect(mock.submitQueue).toHaveBeenCalledTimes(1));
    expect(mock.startRun).not.toHaveBeenCalled();
  });
  it("queue error restores submitted text and preserves new user input",async()=>{
    let reject!:(e:Error)=>void;
    mock.submitQueue.mockImplementation(()=>new Promise((_,r)=>{reject=r;}));
    render(<AppV2/>);
    await waitFor(()=>expect(mock.listRuns).toHaveBeenCalled());
    const draft=screen.getByRole("textbox",{name:"描述任务"});
    fireEvent.change(draft,{target:{value:"original"}});
    fireEvent.keyDown(draft,{key:"Enter",ctrlKey:true});
    await waitFor(()=>expect(mock.submitQueue).toHaveBeenCalledTimes(1));
    fireEvent.change(draft,{target:{value:"new draft"}});
    await act(async()=>reject(new Error("queue transport failed")));
    expect(draft).toHaveValue("original\nnew draft");
    expect(screen.getByRole("alert")).toHaveTextContent("queue transport failed");
  });
  it("approval transport failure retains fingerprint and lets user retry",async()=>{
    mock.listRuns.mockResolvedValue([{...sampleRun("running"),waiting_approval:true}]);
    mock.inspectRun.mockResolvedValue({run_id:"r1",state:"running",summary:{},events:[
      event("r1",1,"run_created",{task:"修复登陆"}),
      event("r1",2,"approval_requested",{fingerprint:"fp-x",summary:"编辑文件"},"op-1")
    ]});
    mock.resumeRun.mockRejectedValueOnce(new Error("transport failed")).mockResolvedValue({run_id:"r1",state:"resuming"});
    render(<AppV2/>);
    fireEvent.click(await screen.findByRole("button",{name:/打开会话 修复登陆/}));
    const allow=await screen.findByRole("button",{name:"允许并继续"});
    fireEvent.click(allow);
    await waitFor(()=>expect(screen.getByRole("alert")).toHaveTextContent("transport failed"));
    fireEvent.click(allow);
    await waitFor(()=>expect(mock.resumeRun).toHaveBeenCalledTimes(2));
    expect(mock.resumeRun.mock.calls[0][0]).toEqual(expect.objectContaining({run_id:"r1",fingerprint:"fp-x",approve:true}));
    expect(mock.resumeRun.mock.calls[0][0]).not.toHaveProperty("mode");
  });
  it("clears previous run's content before the next inspection resolves",async()=>{
    const run2={...sampleRun(),run_id:"r2",title:"第二个任务",task:"第二个任务"};
    mock.listRuns.mockResolvedValue([sampleRun(),run2]);
    mock.inspectRun.mockImplementation(({run_id}:{run_id:string})=>
      run_id==="r1" ? Promise.resolve({run_id,state:"completed",summary:{},events:[event("r1",1,"run_created",{task:"OLD_RUN_SECRET"})]}) : new Promise(()=>{}));
    render(<AppV2/>);
    fireEvent.click(await screen.findByRole("button",{name:/打开会话 修复登陆/}));
    await screen.findByText("OLD_RUN_SECRET");
    fireEvent.click(screen.getByRole("button",{name:/打开会话 第二个任务/}));
    expect(document.querySelector(".v2-transcript")?.textContent||"").not.toContain("OLD_RUN_SECRET");
  });
  it("opens explicit fork from a run without silently continuing its original session",async()=>{
    mock.startRun.mockResolvedValue({run_id:"fork-r2",state:"starting"});
    render(<AppV2/>);
    await screen.findByRole("button",{name:/打开会话 修复登陆/});
    fireEvent.click(screen.getByRole("button",{name:"管理会话 r1"}));
    fireEvent.click(screen.getByRole("button",{name:"从此 Run 创建分支"}));
    expect(screen.getByText(/从 Run r1 创建新分支/)).toBeInTheDocument();
    fireEvent.change(screen.getByRole("textbox",{name:"描述任务"}),{target:{value:"独立重构分支"}});
    fireEvent.click(screen.getByRole("button",{name:"提交任务"}));
    await waitFor(()=>expect(mock.startRun).toHaveBeenCalledWith(expect.objectContaining({fork_source_run_id:"r1",task:"独立重构分支"})));
    const req=mock.startRun.mock.calls[0][0];
    expect(req.session_id).toBeUndefined();
    expect(req.require_session_tail).toBe(false);
  });
  it("shows real dirty state, bridge health and server-supplied Git counts",async()=>{
    mock.observeWorkspace.mockResolvedValue({path:"/tmp/cairn-a",exists:true,has_config:true,dirty:true,config_loaded:true});
    mock.listChanges.mockResolvedValue({root:"/tmp/cairn-a",entries:[],added:7,modified:3,deleted:2,untracked:4});
    render(<AppV2/>);
    await waitFor(()=>expect(screen.getByText("● 未提交")).toBeInTheDocument());
    expect(screen.getByText(/桥接已连接/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button",{name:/打开会话 修复登陆/}));
    fireEvent.click(screen.getByRole("button",{name:"工作台"}));
    expect(await screen.findByTestId("v2-change-summary")).toHaveTextContent("新增 7");
    expect(screen.getByTestId("v2-change-summary")).toHaveTextContent("未跟踪 4");
  });
});


describe("V2 Journal and workspace isolation",()=>{
 beforeEach(()=>{vi.clearAllMocks();setup();});
 it("polls selected Journal run again so a late approval stop becomes visible",async()=>{
  let reads=0;
  mock.inspectRun.mockImplementation(async()=>{
    reads++;
    const events=reads<2 ? [] : [{
      schema_version:1,event_id:"approval-event",run_id:"r1",workspace_hash:"w",seq:2,
      occurred_at:"2026-11-02T04:01:00Z",type:"approval_requested",payload:{fingerprint:"fp-late",summary:"写入文件"},prev_hash:"",event_hash:"",
    }];
    return {run_id:"r1",state:"waiting_approval",summary:{},events};
  });
  render(<AppV2/>);
  fireEvent.click(await screen.findByRole("button",{name:/打开会话 修复登陆/}));
  await waitFor(()=>expect(mock.inspectRun).toHaveBeenCalled());
  await new Promise(resolve=>setTimeout(resolve,2120));
  expect(mock.inspectRun.mock.calls.length).toBeGreaterThanOrEqual(2);
  expect(await screen.findByRole("button",{name:"允许并继续"})).toBeInTheDocument();
 },8500);
 it("drops stale A→B→A workspace data instead of overwriting the current project",async()=>{
  const a="/tmp/cairn-a",b="/tmp/cairn-b";
  mock.listRecentProjects.mockResolvedValue([
    {path:a,name:"cairn-a",pinned:false,archived:false,last_used:""},
    {path:b,name:"cairn-b",pinned:false,archived:false,last_used:""}
  ]);
  let oldA!: (value:unknown)=>void; let aCalls=0;
  mock.listRuns.mockImplementation(({workspace}:{workspace:string})=>{
    if(workspace===a&&++aCalls===1) return new Promise(resolve=>{oldA=resolve;});
    return Promise.resolve([{...sampleRun(),title:workspace===a?"CURRENT_A":"CURRENT_B",task:workspace===a?"CURRENT_A":"CURRENT_B"}]);
  });
  mock.observeWorkspace.mockImplementation(async(path:string)=>({path,has_config:true,exists:true,dirty:false,config_loaded:true}));
  let handler:((evt:unknown)=>void)|null=null;
  mock.onStream.mockImplementation((cb?: (evt:unknown)=>void)=>{handler=cb || null;return ()=>{};});
  render(<AppV2/>);
  await waitFor(()=>expect(mock.listRuns).toHaveBeenCalledWith({workspace:a}));
  fireEvent.click(screen.getByRole("button",{name:/cairn-b/}));
  await screen.findByRole("button",{name:/打开会话 CURRENT_B/});
  if(handler)await act(async()=>{handler!({kind:"run.event",workspace:a,run_id:"foreign",event:{run_id:"foreign",seq:1,event_id:"for",type:"assistant_message_appended",payload:{text:"FOREIGN_BODY"}}});});
  expect(document.querySelector(".v2-transcript")?.textContent||"").not.toContain("FOREIGN_BODY");
  fireEvent.click(screen.getByRole("button",{name:/cairn-a/}));
  await screen.findByRole("button",{name:/打开会话 CURRENT_A/});
  await act(async()=>oldA([{...sampleRun(),title:"STALE_A",task:"STALE_A"}]));
  expect(screen.queryByRole("button",{name:/打开会话 STALE_A/})).not.toBeInTheDocument();
 });
});
