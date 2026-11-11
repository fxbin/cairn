package tools

// SeatbeltWriteMode 决定文件写入是否放开到工作区（OS 约束共用语义）。
type SeatbeltWriteMode string

const (
	// SeatbeltWriteNone 仅允许临时目录写入（只读档）。
	SeatbeltWriteNone SeatbeltWriteMode = "none"
	// SeatbeltWriteWorkspace 允许写工作区与临时目录。
	SeatbeltWriteWorkspace SeatbeltWriteMode = "workspace"
)

// SeatbeltOptions 描述一次 host 命令的 OS 约束。
type SeatbeltOptions struct {
	WorkspaceRoot string
	WriteMode     SeatbeltWriteMode
	AllowNetwork  bool
	// ChildPIDFd 请求包装层把沙箱内部 workload 的宿主 PID 上报到该描述符；
	// 0 表示不上报。仅 Linux bwrap（--info-fd）实现该协议，其他平台忽略。
	// 该上报使取消信号能绕过不转发信号且遇 SIGTERM 即退出的包装进程，
	// 直接作用于内部 workload，让包装进程承接 TerminateGrace。
	ChildPIDFd int
}
