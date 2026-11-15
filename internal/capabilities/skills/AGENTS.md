# internal/capabilities/skills/
> L2 | Parent: ../../../AGENTS.md

技能运行、发现、资源与原生目录发布。通过窄端口复用基础设施。
同一 Skill 实例服务工具、History 和 Context。内容不授予权限。

## 成员

AGENTS.md: 模块职责地图与文件成员入口。
context.go: Context 投影已激活的历史正文。资料保持用户级信任边界。
history.go: History 关联技能提议与成功回执。正文激活不重读当前来源。
history_test.go: 回归 History 关联技能提议与成功回执。正文激活不重读当前来源。
package_access.go: PackageView 与 StoreLock 限定文件和互斥来源。App 显式注入基础设施。
package_access_test.go: 回归 PackageView 与 StoreLock 限定文件和互斥来源。App 显式注入基础设施。
skill.go: Skill 读取技能并保存成功回执激活状态。模型视图与完整证据分别输出。
skill_audit.go: SkillAudit 显式调用已有审计命令。不执行技能内容，不伪造审计成功。
skill_catalog.go: SkillCatalog 有界发现、加载与校验资源。来源摘要绑定规范物理目录。
skill_edit.go: SkillDraft 校验正文和资源路径。版本摘要不替代历史来源摘要。
skill_edit_test.go: 回归 SkillDraft 校验正文和资源路径。版本摘要不替代历史来源摘要。
skill_store.go: SkillStore 只写原生目录。锁内同步临时树后原子发布。
skill_test.go: 回归 Skill 读取技能并保存成功回执激活状态。模型视图与完整证据分别输出。

[PROTOCOL]: Update this header on change, then check AGENTS.md
