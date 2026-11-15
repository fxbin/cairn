# internal/capabilities/hooks/
> L2 | Parent: ../../../AGENTS.md

显式钩子能力。校验规则并生成只收紧候选。
动作仅queue_followup；Core负责观察点与持久化顺序。

## 成员

AGENTS.md: 能力职责与成员入口。
hooks.go: 规则校验、工具匹配与受限动作投影。不执行副作用。
hooks_test.go: 拒绝优先、不得放宽、规则拒绝与动作匹配回归。
constraint_test.go: 冻结来源、非法规则、空配置与缺失实例回归。
constraint.go: 校验并复制本次规则。只输出授权效果与说明。

[PROTOCOL]: Update this header on change, then check AGENTS.md
