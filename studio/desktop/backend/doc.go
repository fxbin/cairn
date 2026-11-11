// Package desktop 是 Cairn GUI 的桌面适配层：把 internal/app 用例投影为
// Wails 可绑定的请求/响应/事件，并维持异步运行与审批唤醒。
//
// 边界：
//   - 不引入 Domain/Agent 内核改动；Event Journal 仍是运行事实源。
//   - Wails 仅通过 Emitter 突口通知前端；本包不直接依赖 Wails 类型。
//   - 前端不得获得密钥、OAuth token 或任意宿主 shell 权限。
//
// Author: fxbin
package desktop
