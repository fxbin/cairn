# internal/benchmark/
> L2 | Parent: ../../AGENTS.md

带外评测工具链。冻结身份、隔离执行和恢复结论各有唯一事实源。
评测资产显式传入；不改变生产默认。

## 成员

AGENTS.md: 职责地图与完整成员入口。
freeze_eval_set.go: 冻结任务、变体、镜像与私有验证资产。启动前自检绑定同一指纹。
freeze_eval_set_test.go: 冻结资产生成与自检回归。输出仅写测试目录。
frozen_inputs.go: 执行计划摘要的唯一公式。冻结器和执行器共用。
frozen_inputs_test.go: 冻结输入摘要敏感性与历史身份形状回归。
json_commit_unix.go: 非Windows的同步与JSON原子提交。
json_commit_windows.go: Windows的JSON提交边界。按平台编译。
local_config.go: 显式本地配置和忽略资产的严格装载。
local_config_test.go: 本地资产解析、未知字段与跟踪资产拒绝回归。
manifest.go: 本地任务包与仓库基线合同。不推导私有资产目录。
mirror.go: 本地优先镜像与固定基线检出。缺失基线才获取。
mirror_test.go: 镜像本地优先、按需获取与基线拒绝回归。
runner.go: 隔离任务运行与准备入口。结果按固定合同留档。
runner_process.go: 受限进程、代理环境和构建身份。
runner_result.go: 任务尝试的资格、差异与结果收束。
runner_store.go: 持久化事件指标与规范JSON提交。
runner_test.go: 准备、失败分类和任务结果回归。
runner_workspace.go: 固定基线克隆、引导与私有验证注入。
sandbox_evidence.go: 重算冻结评测身份与整数收益证据。
sandbox_executor.go: 受控评测入口、唯一评测事务与闭合输出。
sandbox_executor_test.go: 冻结前检、记录恢复、租约与证据拒绝回归。
sandbox_precheck.go: 启动前核对身份、租约、凭据和冻结任务。
sandbox_recovery.go: 恢复记录、套件结论重算与原子结果提交。
sandbox_schedule.go: 交错调度、单次运行、证据提取与租约计账。
sandbox_suite.go: 闭合变体展开与离线比较预筛。
sandbox_suite_test.go: 变体范围、臂交错顺序与比较器回归。

[PROTOCOL]: Update this header on change, then check AGENTS.md
