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
}
