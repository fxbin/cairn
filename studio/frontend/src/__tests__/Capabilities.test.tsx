import {beforeEach, afterEach, describe, expect, it, vi} from "vitest";
import {fireEvent, render, screen, waitFor, within} from "@testing-library/react";
const mock=vi.hoisted(()=>({
 listWorkflows:vi.fn(),saveWorkflow:vi.fn(),runWorkflow:vi.fn(),
 listSkills:vi.fn(),listHooks:vi.fn(),listMemories:vi.fn(),listCommands:vi.fn(),
 listProjectInstructions:vi.fn(),mcpStatus:vi.fn(),upsertHook:vi.fn(),
 disableHook:vi.fn(),inspectMemory:vi.fn(),revokeMemory:vi.fn(),writeProjectInstructions:vi.fn(),
 installSkill:vi.fn(),removeSkill:vi.fn(),setSkillEnabled:vi.fn(),consolidateMemories:vi.fn(),
}));
vi.mock("../desktop/client",()=>({desktop:mock}));
import WorkflowsView from "../workbench/WorkflowsView";
import CapabilitySettings from "../workbench/CapabilitySettings";
beforeEach(()=>{
 vi.clearAllMocks();
 mock.listWorkflows.mockResolvedValue([{id:"wf-1",name:"任务回归",steps:[{task:"运行测试"}]}]);
 mock.saveWorkflow.mockResolvedValue({ok:true});mock.runWorkflow.mockResolvedValue({workflow_id:"wf-1",state:"starting"});
 mock.listSkills.mockResolvedValue([]);mock.listHooks.mockResolvedValue([]);
 mock.listMemories.mockResolvedValue([{id:"mem-1",kind:"lesson",status:"active",trusted_verification:false}]);
 mock.listCommands.mockResolvedValue([]);
 mock.listProjectInstructions.mockResolvedValue({documents:[]});mock.mcpStatus.mockResolvedValue([]);
 mock.upsertHook.mockResolvedValue({ok:true});mock.disableHook.mockResolvedValue({ok:true});
 mock.inspectMemory.mockResolvedValue({candidate:{id:"mem-1",outcome:"ok",status:"active"},audit:["source verified"]});
 mock.revokeMemory.mockResolvedValue({ok:true});mock.writeProjectInstructions.mockResolvedValue({ok:true});
 mock.installSkill.mockResolvedValue({name:"skill"});mock.setSkillEnabled.mockResolvedValue({ok:true});
 mock.removeSkill.mockResolvedValue({ok:true});mock.consolidateMemories.mockResolvedValue(0);
});
afterEach(()=>vi.restoreAllMocks());
describe("Workbench workflow editor parity",()=>{
 it("creates explicit ordered steps through the real SaveWorkflow API",async()=>{
  render(<WorkflowsView workspace="/tmp/a" notify={vi.fn()}/>);
  fireEvent.change(screen.getByRole("textbox",{name:"工作流名称"}),{target:{value:"新任务链"}});
  fireEvent.change(screen.getByRole("textbox",{name:"第1步"}),{target:{value:"先验证代码"}});
  fireEvent.click(screen.getByRole("button",{name:/添加步骤/}));
  fireEvent.change(screen.getByRole("textbox",{name:"第2步"}),{target:{value:"再生成报告"}});
  fireEvent.click(screen.getByRole("button",{name:"保存工作流"}));
  await waitFor(()=>expect(mock.saveWorkflow).toHaveBeenCalledWith({id:undefined,workspace:"/tmp/a",name:"新任务链",steps:[{task:"先验证代码"},{task:"再生成报告"}]}));
 });
 it("shows an authoritative Task report and its evidence without a public workflow template",async()=>{
  mock.listWorkflows.mockResolvedValue([]);
  const onOpenRun=vi.fn();
  render(<WorkflowsView workspace="/tmp/a" notify={vi.fn()} onConverseCreate={vi.fn()} onOpenRun={onOpenRun} instances={[{
   workflow_id:"wf-task",task_id:"task-1",task_state:"active",created_at:"",finished:true,finish_state:"failed",resume_index:2,steps:[],
   verification_criteria:[{id:"behavior",statement:"计算结果符合单测"}],
   verification:{
    worker_run_id:"worker-1",program_run_id:"program-1",review_run_id:"review-1",
    evaluation:{verdict:"unknown",criteria:[{criterion_id:"behavior",verdict:"unknown",reason:"需要独立复核"}]},
    coverage:{entries:[{object_id:"program",status:"completed"},{object_id:"review",status:"failed"}]},
    findings:{inconclusive:[{id:"finding-1",status:"inconclusive",description:"缺少文件证据"}]},
   },
  }]}/>);
  const report=within(await screen.findByRole("region",{name:"任务验收报告"}));
  expect(report.getByText(/计算结果符合单测/)).toBeInTheDocument();
  expect(report.getByText(/缺少文件证据/)).toBeInTheDocument();
  fireEvent.click(report.getByRole("button",{name:"查看独立审查"}));
  expect(onOpenRun).toHaveBeenCalledWith("review-1");
  expect(screen.queryByText("这个工作流暂无持久化实例。")).not.toBeInTheDocument();
 });
 it("keeps a passed workflow in explicit Task acceptance until the Task state changes",async()=>{
  mock.listWorkflows.mockResolvedValue([]);
  const instance = {
   workflow_id:"wf-task",task_id:"task-1",task_state:"active",created_at:"",finished:true,finish_state:"completed",resume_index:-1,steps:[],
   verification:{
    worker_run_id:"worker-1",program_run_id:"program-1",review_run_id:"review-1",
    evaluation:{verdict:"passed",criteria:[]},coverage:{entries:[]},findings:{},
   },
  };
  const config = {workspace:"/tmp/a", notify:vi.fn(), onConverseCreate:vi.fn()};
  const {rerender}=render(<WorkflowsView {...config} instances={[instance]}/>);
  expect(await screen.findByText("待人工验收")).toBeInTheDocument();
  expect(screen.queryByText("已验收")).not.toBeInTheDocument();
  rerender(<WorkflowsView {...config} instances={[{...instance,task_state:"completed"}]}/>);
  expect(await screen.findByText("已验收")).toBeInTheDocument();
  expect(screen.queryByText("待人工验收")).not.toBeInTheDocument();
 });
 it("requires explicit confirmation before workflow execution, not for dry run",async()=>{
  const confirm=vi.spyOn(window,"confirm").mockReturnValue(false);
  render(<WorkflowsView workspace="/tmp/a" notify={vi.fn()}/>);
  await screen.findByText("任务回归");
  fireEvent.click(screen.getByRole("button",{name:"运行"}));
  expect(mock.runWorkflow).not.toHaveBeenCalled();
  confirm.mockReturnValue(true);
  fireEvent.click(screen.getByRole("button",{name:"运行"}));
  await waitFor(()=>expect(mock.runWorkflow).toHaveBeenCalledWith("/tmp/a","wf-1",false));
 });
});
describe("Workbench capability administration parity",()=>{
 it("registers only an allowlisted queue hook after explicit human content",async()=>{
  render(<CapabilitySettings workspace="/tmp/a" notify={vi.fn()}/>);
  fireEvent.click(screen.getByRole("button",{name:"Hooks"}));
  fireEvent.change(screen.getByRole("textbox",{name:"完成后入队内容"}),{target:{value:"完成后跑回归"}});
  fireEvent.click(screen.getByRole("button",{name:"添加完成入队钩子"}));
  await waitFor(()=>expect(mock.upsertHook).toHaveBeenCalledWith({workspace:"/tmp/a",event:"run_finished",enabled:true,actions:[{kind:"queue_followup",content:"完成后跑回归"}]}));
 });
 it("requires an explicit reason to revoke a memory candidate",async()=>{
  render(<CapabilitySettings workspace="/tmp/a" notify={vi.fn()}/>);
  fireEvent.click(screen.getByRole("button",{name:"记忆"}));
  await screen.findByText(/lesson · mem-1/);
  expect(screen.getByRole("button",{name:"撤销候选"})).toBeDisabled();
  fireEvent.change(screen.getByRole("textbox",{name:"记忆撤销说明"}),{target:{value:"过时信息"}});
  fireEvent.click(screen.getByRole("button",{name:"撤销候选"}));
  await waitFor(()=>expect(mock.revokeMemory).toHaveBeenCalledWith({workspace:"/tmp/a",candidate_id:"mem-1",reason:"过时信息"}));
 });
 it("does not write AGENTS.md until the user confirms",async()=>{
  const confirm=vi.spyOn(window,"confirm").mockReturnValue(false);
  render(<CapabilitySettings workspace="/tmp/a" notify={vi.fn()}/>);
  fireEvent.click(screen.getByRole("button",{name:"项目指令"}));
  fireEvent.change(screen.getByRole("textbox",{name:"项目规约正文"}),{target:{value:"# Project Rules"}});
  fireEvent.click(screen.getByRole("button",{name:"写入项目规约"}));
  expect(mock.writeProjectInstructions).not.toHaveBeenCalled();
  confirm.mockReturnValue(true);
  fireEvent.click(screen.getByRole("button",{name:"写入项目规约"}));
  await waitFor(()=>expect(mock.writeProjectInstructions).toHaveBeenCalledWith("/tmp/a","# Project Rules"));
 });
});
