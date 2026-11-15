# internal/contract/
> L2 | Parent: ../../AGENTS.md

共享纯数据、摘要和只读投影。不依赖 App、能力或 Runtime 执行实现。
编码与历史回执绑定。合同不授予权限，也不决定运行终态。

## 成员

AGENTS.md: 合同职责地图与成员入口。
artifact_ref.go: Ref 定义内容寻址制品引用。字段顺序绑定历史事件。
artifact_ref_test.go: 回归 Ref 定义内容寻址制品引用。字段顺序绑定历史事件。
authorization_constraint.go: AuthorizationConstraint只表达效果与说明。不携带审批或执行身份。
authorization.go: AuthorizationDecision 定义纯授权结果。不透明执行绑定不授予权限。
authorization_ports.go: PatchPlanner 定义基础策略只读规划端口。不携带执行能力。
classify.go: Classify 从运行结果投影闭合失败分类。不替代 Journal。
contract_test.go: 验证版本化任务、清单与运行结果的组合合同。
display.go: Display 统一运行阶段与可信展示词表。不改变事件 schema。
display_guard_test.go: 验证界面调用共享词表，阻止重复状态映射。
display_test.go: 回归 Display 统一运行阶段与可信展示词表。不改变事件 schema。
lifecycle.go: LifecycleOperation 定义固定释放提议。资源执行留在审计工具链。
manifest.go: Manifest 定义冻结运行身份与摘要。兼容读取保留历史合同。
parallel_plan.go: ParallelPlan 规划固定只读调用分组。不执行工具。
project_instructions.go: ProjectInstructions 定义脱敏规约快照及历史引用。不访问当前文件。
provider_card.go: ProviderCard 定义协议能力与证据投影。目录建议不构成验证。
provider_card_test.go: 回归 ProviderCard 定义协议能力与证据投影。目录建议不构成验证。
provider_profile.go: ProviderProfile 冻结请求画像及指纹。缺少修订时显式标记。
provider_profile_test.go: 回归 ProviderProfile 冻结请求画像及指纹。缺少修订时显式标记。
redact.go: Redact 统一文本与结构化载荷的脱敏。事件和制品共用此合同。
redact_equivalence_test.go: 验证脱敏词干与兼容入口的逐字节等价。
result.go: Result 从 Journal 投影运行结果。不是第二事实源。
sensitive_key.go: SensitiveKey 统一凭据名称判定。不保存密钥值。
skill_content.go: SkillContent 定义历史正文快照。内容不能授予权限。
task.go: TaskSpec 绑定用户任务与冻结输入。运行事实仍归 Journal。
tool_policy_contract.go: ToolPolicy 定义命令、补丁、会话和本地授权数据。字段绑定原审批指纹。
tool_result.go: ToolResult 定义成功与失败回执。编码顺序绑定历史数据。
verification_evidence.go: VerificationEvidence 核对已持久化的独立验证链。不执行验证。
verification_plan.go: VerificationPlan 冻结命令及其摘要。不继承模型工具审批。

[PROTOCOL]: Update this header on change, then check AGENTS.md
