// Package workspace 承载可替换工作区后端的对照实验：以 Go 测试形式复跑共享
// 目录与 Git worktree 两种后端在并行分支、冲突检测、失败恢复与交付上的
// 差异，并把测量结果输出为可复现的报告。
//
// 本包只服务于 Spike 验证，不接入生产运行路径，也不改动既有工具行为。
//
// Author: fxbin
package workspace
