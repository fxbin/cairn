// Package interop 提供 Cairn 与原生 Agent CLI（Codex / Claude Code）之间的
// 兼容性研究脚手架：能力矩阵探测、受控对照实验与升级破坏面取证。
//
// 本包属于研究用测试树，不是运行时依赖：Cairn 的生产路径不导入本包，也不
// 因本包引入任何默认后台进程、插件宿主或外部 CLI 依赖。
//
// 三条硬边界在本包内以代码形式固定：
//
//  1. 外部 CLI 的"完成"只记为 harness 观测，不得转写为 Cairn 的 verified；
//     验证可信只来自 Cairn 自己的 verifier 证据。
//  2. 外部工具的权限模型不得提升 Cairn Policy：探测只在临时目录内进行，
//     不接受外部 CLI 的授权决定作为 Cairn 的授权输入。
//  3. 授权、账号与凭据不得从沙箱透传：探测使用本地桩模型与占位密钥，
//     不读取、不记录、不落盘任何用户真实账号凭据。
//
// Author: fxbin
package interop
