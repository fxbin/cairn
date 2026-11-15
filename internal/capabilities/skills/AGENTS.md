# internal/capabilities/skills/
> L2 | Parent: ../../../AGENTS.md

技能历史正文与回执投影。当前运行和管理实现仍由工具层提供。
Context 和 History 共用成功快照来源。内容不能授予权限。

## 成员

AGENTS.md: 模块职责地图与文件成员入口。
context.go: Context 投影已激活的历史正文。资料保持用户级信任边界。
history.go: History 关联技能提议与成功回执。正文激活不重读当前来源。
history_test.go: 回归History 关联技能提议与成功回执。正文激活不重读当前来源。

[PROTOCOL]: Update this header on change, then check AGENTS.md
