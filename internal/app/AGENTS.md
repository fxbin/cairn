# internal/app/
> L2 | Parent: ../../AGENTS.md

唯一组合根与应用用例。CLI、TUI 和 Studio 复用本层。
Boot 统一资源所有权。能力按有效配置显式装配。

## 成员

AGENTS.md: 模块职责地图与文件成员入口。
boot.go: Boot 统一资源所有权。钩子先校验，冻结规则分别注入约束与Core动作。
boot_test.go: 回归 Boot 唯一装配模型、工具、授权和存储。资源所有权覆盖运行与恢复。
boot_tool_callbacks.go: 显式绑定能力召回工具与本次Journal。服务端强制工作区范围。
boot_tool_callbacks_test.go: 回归 Boot 回调绑定本次记忆与消息资源。最小组合不构造可选回调。
hooks.go: 纯校验执行配置并转换冻结规则。工作流与运行入口共用。
hooks_test.go: 真实运行与Workflow入口拒绝非法规则。冻结投影和Doctor口径回归。
context_provider_test.go: 验证显式技能配置与历史正文投影装配。
contract.go: FrozenRunConfig 绑定运行清单与历史配置。恢复沿用同一序列化合同。
delegate_async.go: AsyncDelegateRuntime 跟踪子运行生命周期。父运行返回前回收进程内任务。
delegate_async_test.go: 回归 AsyncDelegateRuntime 跟踪子运行生命周期。父运行返回前回收进程内任务。
delegate_run.go: Delegator 在父 Journal 写入委派事实。子预算与工具权限从父运行收窄。
delegate_run_test.go: 回归 Delegator 在父 Journal 写入委派事实。子预算与工具权限从父运行收窄。
deliver_run_message.go: RunMessage 将正文存为制品并追加 steering 事实。投递绑定目标 Journal。
diagnostics.go: Doctor 只读投影配置、Provider 和沙箱就绪状态。不宣称真实调用成功。host 收紧档位下只做规则推导，提前给出命令会被启动前拒绝的稳定原因码，不改变就绪判定。
diagnostics_confinement_test.go: 验证 Doctor 提前给出沙箱规则无法生成的原因码，报告里不带路径。
diagnostics_telemetry_test.go: 验证构建身份与可选观测状态的只读投影。
diagnostics_test.go: 回归 Doctor 只读投影配置、Provider 和沙箱就绪状态。不宣称真实调用成功。
frozen_registry.go: FrozenToolRegistryFingerprint 与 Boot 共用工具选择。冻结身份不创建运行。
latest_subagent_regression_test.go: 验证子运行恢复、取消及身份与预算准入。
mcp.go: MCPStatus 只读配置与凭据存在性。登录和退出由显式用例触发。
mcp_test.go: 验证 MCP 惰性装配、请求作用域及缺失来源拒绝。
memory.go: 连接Memory能力与运行来源。捕获、召回不决定Run终态。
memory_consolidate.go: MemoryConsolidation 校验来源后写入派生记忆。撤销关系保留审计证据。
memory_consolidate_test.go: 回归 MemoryConsolidation 校验来源后写入派生记忆。撤销关系保留审计证据。
memory_evolution.go: EvolutionCandidate 冻结评测引用与候选状态。试运行不改变生产默认。
memory_evolution_test.go: 回归 EvolutionCandidate 冻结评测引用与候选状态。试运行不改变生产默认。
memory_test.go: 回归 Memory 用例捕获结构化结果并生成召回证据。记忆不决定运行终态。
minimal_kernel.go: MinimalRegistry 隐藏可选能力方法。冲突配置在资源创建前拒绝。
minimal_kernel_test.go: 回归 MinimalRegistry 隐藏可选能力方法。冲突配置在资源创建前拒绝。
project_instructions.go: ProjectInstructionSnapshot 冻结脱敏规约。新运行读取当前来源，恢复读取历史。
project_instructions_test.go: 回归 ProjectInstructionSnapshot 冻结脱敏规约。新运行读取当前来源，恢复读取历史。
provider_admission.go: ProviderAdmission 将确认绑定到证据与请求画像。替换决定须匹配旧链头。
provider_credential.go: ProviderCredential 按受限来源装载密钥。输出不回显凭据值。
provider_credential_test.go: 回归 ProviderCredential 按受限来源装载密钥。输出不回显凭据值。
provider_probe.go: ProviderProbe 保存协议探测证据。目录建议不构成能力验证。
providerfactory.go: ProviderFactory 根据有效协议构造适配器。生成参数保持显式配置。
queue.go: Queue 将输入持久化为待处理、关联或取消状态。锁竞争有界失败。
queue_test.go: 回归 Queue 将输入持久化为待处理、关联或取消状态。锁竞争有界失败。
rewind.go: Rewind 从无损 before 制品生成回退计划。每次文件写入走完整授权链。
rewind_test.go: 回归 Rewind 从无损 before 制品生成回退计划。每次文件写入走完整授权链。
service.go: Service.Run 在观测前校验钩子并驱动 Boot；任务分配可在同一锁内锚定创建事件。
service_admission.go: AdmissionGate 统一运行和诊断的准入检查。过期证据失败关闭。
service_artifacts.go: ArtifactIntegrity 校验历史引用和摘要。损坏制品阻止恢复。
service_assemble.go: ConfiguredLoop 注入同实例投影与释放参与者。授权保留请求作用域。
service_control.go: 恢复与对账在观测前校验钩子。应急终止不激活钩子。
service_query.go: RunQuery 只读投影运行与证据。缺失账本不生成虚构记录。
service_session.go: SessionAncestry 校验会话来源和工作区身份。循环与损坏链失败关闭。
service_test.go: 回归 Service.Run 校验冻结身份并驱动 Boot。运行事实来自 Journal。
service_wall_budget_test.go: 验证服务层墙钟预算与取消收尾。
session.go: PriorContextCard 从历史事件生成前情。缺失字段如实标记。
session_fork_test.go: 验证会话分叉来源、原链保留及损坏拒绝。
session_test.go: 回归 PriorContextCard 从历史事件生成前情。缺失字段如实标记。
session_title.go: SessionTitle 选择标题模型并清理输出。标题不参与运行身份。
session_title_test.go: 回归 SessionTitle 选择标题模型并清理输出。标题不参与运行身份。
setup.go: SetupService 校验配置与探测结果后保存设置。凭据使用独立存储。
setup_test.go: 回归 SetupService 校验配置与探测结果后保存设置。凭据使用独立存储。
skills.go: Skill 用例显式注入文件视图和锁。管理入口只写原生目录。
skills_test.go: 回归 Skill 用例显式注入文件视图和锁。管理入口只写原生目录。
studio_bootstrap.go: StudioBootstrap 提供用户根和受控评测入口。桌面层不直接持有底层实现。
studio_content.go: ContentFacade 为模板、内容包与技能提供应用入口。界面层消费稳定数据形状。
studio_evaluation.go: EvaluationFacade 暴露隔离评测合同。界面层不装配评测执行器。
studio_memory.go: MemoryFacade 暴露记忆合同与应用用例。界面层不持有数据库实现。
studio_provider.go: ProviderFacade 暴露模型目录与准入存储入口。建议与准入证据分别呈现。
studio_tui.go: StudioTUI 提供 steering 与凭据路径入口。界面层不依赖 Runtime 类型。
studio_tui_tools.go: StudioWorkspace 提供受限工作区动作。写入复用应用授权边界。
subagent.go: Subagent 合同绑定父子身份与预算。委派不能扩大父权限。
subagent_budget.go: BudgetRemaining 按持久化用量裁剪子预算。超额分配失败关闭。
subagent_budget_test.go: 回归 BudgetRemaining 按持久化用量裁剪子预算。超额分配失败关闭。
subagent_cancel.go: SubagentCancel 向未完成子运行传播取消。完成依据保留在各自 Journal。
subagent_cancel_test.go: 回归 SubagentCancel 向未完成子运行传播取消。完成依据保留在各自 Journal。
subagent_complete.go: SubagentCompletion 将子终态摘要回写父运行。状态与验证可信度分别投影。
subagent_emit.go: SubagentEventDraft 生成父运行的委派事件。载荷只保留任务摘要和预算。
subagent_emit_test.go: 回归 SubagentEventDraft 生成父运行的委派事件。载荷只保留任务摘要和预算。
subagent_journal.go: SubagentJournal 追加父子关联事实。重复身份和链不一致失败关闭。
subagent_journal_test.go: 回归 SubagentJournal 追加父子关联事实。重复身份和链不一致失败关闭。
subagent_progress.go: SubagentProgress 投影父子进展与细节。界面不替代 Journal。
subagent_progress_test.go: 回归 SubagentProgress 投影父子进展与细节。界面不替代 Journal。
subagent_resume.go: SubagentResume 重建已分派子运行。历史关联阻止重复委派。
subagent_resume_test.go: 回归 SubagentResume 重建已分派子运行。历史关联阻止重复委派。
subagent_review.go: SubagentReview 区分产出与独立验收来源。角色标签不能授予权限。
verifier.go: 派生隔离文件工具。冻结标准选择与结构化输出共用同一合同。
verifier_test.go: 回归只读配置、标准选择与结构化输出失败关闭。
verifier_context.go: 核对目标 Run 终态及历史工作区观察。只传有界验收上下文。
verifier_run.go: 先核验目标身份，再委派只读 Run。校验子终态与工作区版本。
verifier_run_test.go: 回归目标身份、上下文隔离、父预算和子 Run 终态。
finding_validation.go: 所有严重度都依赖核验证据。定位不授予确认。
finding_validation_test.go: 回归零证据、伪造制品和复核决定。
verifier_findings.go: 同一目标的模型发现仅标记为未定论。制品存在不确认缺陷。
verifier_findings_test.go: 回归真实和伪造引用都不能单独确认模型发现。
rework.go: 确认 Finding 驱动的有界返工资格检查，复用 workflow.RetryStep。
subagent_review_test.go: 回归 SubagentReview 区分产出与独立验收来源。角色标签不能授予权限。
subagent_start.go: SubagentStart 校验先有委派事实再启动子运行。工具与预算保持父授权子集。
subagent_start_test.go: 回归 SubagentStart 校验先有委派事实再启动子运行。工具与预算保持父授权子集。
subagent_test.go: 回归 Subagent 合同绑定父子身份与预算。委派不能扩大父权限。
task.go: 装配 Task SQLite。应用门面按版本推进工作区任务与用户验收。
task_context.go: TaskContext 只读加载任务、工作流链、Journal 与制品事实并交给合同层投影。逐来源映射稳定原因码，完整性失败落 blocked 而不是报错；当前没有绑定到步骤的验证报告时取最近一份已收束尝试的报告并重新核验，已知不回退；不写任何文件，不携带模型输出。
task_context_test.go: 回归任务上下文的确定性、只读、逐字段落状态、制品在位判定与返工期间证据不回退。
task_handoff.go: 显式开启任务交接时生成交接文本。任一字段 blocked 则拒绝启动；复用会话前情通道，交接文本按内容寻址制品化以便回放。
task_handoff_test.go: 回归交接摘要进入新会话的手工分配与返工 Worker 的模型输入，默认关闭与调用方自带前驱时不注入。
task_run.go: 在创建 Run 前持久化分配。执行阶段与证据从 Run Journal 读取。
task_workflow.go: 先固定自主实例身份，再确认创建。显式验收绑定审查报告。
task_workflow_test.go: 回归冷恢复、重复授权、显式验收与有界返工。
workflow_task_rework.go: 从已确认的程序检查失败推进冻结额度内的返工。
workflow_program_finding.go: 核对冻结检查与实际退出制品。无法核验时保留待定。
workflow_program_finding_test.go: 回归真实非零退出、对账、信号与制品错配。
telemetry.go: 显式装配遥测能力并收束资源。Doctor共用接线判定，导出失败不改变运行事实。
telemetry_test.go: 回归 Telemetry 装配可选导出并收束资源。导出失败不改变运行事实。
trial_activation.go: TrialActivation 只读解析显式试运行变体。不改变生产默认配置。
trial_activation_test.go: 回归 TrialActivation 只读解析显式试运行变体。不改变生产默认配置。
user_directory_test.go: 验证用户根 OAuth 存储、权限与按身份退出。
workflow_definition.go: 校验宿主草案并冻结制品。查询验证修订与工作区归属。
workflow_exec.go: 工作流写入前校验钩子。执行、恢复与人工验收分别持久化。
workflow_exec_test.go: 回归 WorkflowExecution 持久化执行、验证和人工验收步骤。恢复遵循已有步骤事实。
workflow_instance.go: 装配不可变定义实例。实例锁内校验版本、配置和人工门决定。
workflow_instance_run.go: 在既有 Run 锁内绑定 Agent、Script 和 Verify 尝试。按 Journal 核验并输出制品。
workflow_control.go: 按实例链头追加暂停、继续、取消及有界返工授权。
workflow_operation.go: 复用已绑定 Run 的指纹审批和对账。版本漂移保留原审批。
workflow_run_owner.go: 从 Run Journal 识别绑定归属，旧实例从工作流链查询。普通续链保留原工作区约束。
workflow_history.go: 从持久化实例与 Run Journal 核验历史、产物和审批阶段。
workflow_query.go: 只读投影实例、Task 状态、冻结标准及验收报告。
workflow_query_test.go: 回归 WorkflowStatus 只读投影持久化工作流链。查询不启动调度器。

workflow_store.go: 连接能力 SQLite 叶子与制品。路径、只读打开和链头 CAS 保持。
workflow_verify.go: 冻结脚本、验收合同与目标版本。程序验证复用原授权、沙箱及恢复入口。
workflow_verification.go: 连接 Worker、Program、独立审查与报告制品。所有阶段核对同一工作区版本。
workflow_verification_test.go: 通过真实工具验证程序证据、隔离审查及报告重放。
workflow_verification_report.go: 绑定冻结范围、覆盖事实、Finding 与逐标准验收结论。
workflow_verification_report_test.go: 回归缺失证据和独立发现不会产生错误通过。

[PROTOCOL]: Update this header on change, then check AGENTS.md
