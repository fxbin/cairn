# studio/backend/
> L2 | Parent: ../../AGENTS.md

桌面桥接把绑定调用转换为 App 用例。
App 拥有运行、持久任务和工作流事实。
控制器只保留进程内取消句柄与事件出口。

AGENTS.md: 后端职责与文件入口。
api.go: Wails 门面与装配。工作区和运行历史通过 App。
api_files.go: 桌面文件选择、查看与受控操作入口。
api_hooks.go: 桌面生命周期与轻量钩子偏好。
api_provider_review.go: Provider 配置与审阅绑定入口。
api_runs.go: Run 发起、恢复、对账和证据绑定。
api_task.go: 宿主任务生命周期与运行分配映射。
api_workflow.go: 保存、预演和读取 App 模板修订。旧偏好只供导入。
api_workflow_memory.go: 项目规约与记忆绑定入口。
bridge_contract_test.go: 回归桥接调用和兼容字段。
bridge_event_test.go: 回归跨运行事件路由与隔离。
desktop_test.go: 回归桌面绑定、状态和权限。
display_test.go: 回归终态、时间和内容展示投影。
doc.go: 桌面包的信任与依赖边界。
errors.go: 将 App 和端口异常映射为稳定错误码。
file_tree_bfs_test.go: 回归文件树遍历边界。
files.go: 工作区文件视图、编辑与 Diff 操作。
helpers.go: 配置装载、作用域和审批请求映射。
open_location.go: 原生编辑器与位置选择。
prefs.go: 维护最近项目、会话、钩子和旧模板元数据。
provider_setup_regression_test.go: 回归 Provider 配置与验证。
queue.go: 待处理任务的桌面入队与确认桥接。
review_regression_test.go: 回归恢复、工作区和会话行为。
run.go: 转发 Run 事件。跟踪活动任务取消与 Steering。
service.go: 支持桌面运行的服务类型与事件入口。
types.go: Wails 请求、回执和共享事件形状。
workflow_runtime.go: 创建实例后调用 App 推进。投送持久状态和 Run 通知。
workspace.go: 规范化工作区路径与只读信息。

[PROTOCOL]: Update this header on change, then check AGENTS.md
