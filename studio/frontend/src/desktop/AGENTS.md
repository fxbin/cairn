# studio/frontend/src/desktop/
> L2 | Parent: ../../../../AGENTS.md

用同一宿主传输连接工作台。演示桥只在显式入口使用。

AGENTS.md: 桥接层成员入口。
browserBridge.ts: 显式演示/模拟桥，不作为真实宿主回退。
chatBlocks.ts: 将 Journal 回执投影到对话块。
client.ts: 封装宿主绑定与错误传输；Workflow 直接调用 App 门面。
events.ts: 订阅真实运行事件通道。
timeline.ts: 展示持久事件时间线。
types.ts: 定义宿主传输、工作流实例与任务验收报告。
wails-api.ts: 绑定生成方法，并保护原生与 HTTP 传输入口。

[PROTOCOL]: Update this header on change, then check AGENTS.md
