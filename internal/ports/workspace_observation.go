// workspace_observation.go 定义 verifier 视图的工作区观察窄端口。
// 观察值是绑定命令审批与续跑漂移检测的内容摘要；实现必须覆盖每个非敏感、
// 非 volatile 路径，不得以工具视图的忽略规则缩小 verifier 的观察范围。
// Author: fxbin
package ports

// WorkspaceObserver 提供 verifier 视图的稳定工作区观察摘要。Core 经此端口
// 读取观察值，不持有具体工作区实现；摘要用于审批绑定与恢复时的漂移检测。
type WorkspaceObserver interface {
	// Observation 返回当前工作区内容的稳定摘要；观察失败必须返回错误，
	// 不得以空摘要冒充成功。
	Observation() (string, error)
}
