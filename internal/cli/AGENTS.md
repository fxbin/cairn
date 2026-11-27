# internal/cli/
> L2 | Parent: ../../AGENTS.md

命令分派与输出。业务入口调用 App；JSON stdout 仅输出 NDJSON。

## 成员

AGENTS.md: 模块职责地图与成员入口。
command_benchmark.go: benchmark 与评测集冻结子命令。
command_memory.go: memory 系列子命令。
command_memory_candidate.go: memory candidate 生命周期子命令。
command_memory_promotion.go: 晋升、沙箱激活与遥测。
command_memory_store.go: 记忆捕获、检索与派生检查。
command_resume_run.go: resume / run 子命令与参数规范化。
command_app_scope.go: 集中持久任务和工作流的作用域参数。
command_task.go: 持久任务、分配与自主推进。显示逐标准报告和 Run 锚点；task context 只读输出任务上下文投影或交接摘要。
command_task_context_test.go: 验证 task context 的 NDJSON 输出、交接摘要与未找到的退出码。
command_workflow.go: 维护固定计划接口，分派版本化实例命令。
command_workflow_control.go: 转交绑定链头的显式控制、审批与对账。
command_workflow_definition.go: 严格解析定义草案并提交选定修订。
command_workflow_exec_test.go: 工作流执行类子命令的 CLI 接线测试：run/resume/accept 的退出码、NDJSON 输出与人工门提示，不接受任意脚本。
command_workflow_test.go: 工作流子命令的 CLI 接线测试：真实存储只读渲染、NDJSON 输出与用法约束。
display_test.go: 人类视图复用合同词表。JSON 输出保持 NDJSON。
init.go: CLI 收集配置向导输入。SetupService 校验并保存配置。
mcp.go: 显式 MCP 清单和 OAuth 入口。清单查询不连接服务器。
mcp_test.go: 验证MCP清单只输出NDJSON且不连接服务器。
package.go: 声明式内容包命令。安装不执行脚本或授予权限。
promotion_metrics.go: 从冻结评测记录提取晋升指标摘要。
promotion_trust_test.go: 拒绝缺少可信验证证据的旧评测结果。
provider.go: Provider 命令分派。应用层执行探测、配置与准入。
provider_check.go: 解析探测旗标并输出结果。CheckProvider 执行业务。
provider_check_test.go: 验证Provider两轮探测、失败关闭与证据保存。
provider_test.go: 验证Provider目录、配置、准入及Preset显式命令。
render.go: CLI 输出渲染与退出码映射。
review_regression_test.go: 回归模型筛选、内容模板与Preset安全写入。
root.go: CLI 入口、分发与核心子命令。
root_test.go: 根命令、NDJSON、诊断与显式记忆评测入口回归。
session.go: 会话查询、分叉与续链命令。显式来源阻止幽灵会话。
session_test.go: 会话只读输出与用法回归。缺少分叉来源时拒绝。
skills.go: 本地技能管理命令。JSON 每条记录独占一行。
skills_test.go: 验证技能管理脱敏与无Provider配置入口。
template.go: 用户文本模板命令。渲染不执行脚本或读取秘密。
tui.go: TUI 旗标、TTY 检查与向导入口。配置业务复用 SetupService。

[PROTOCOL]: Update this header on change, then check AGENTS.md
