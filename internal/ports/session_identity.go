// session_identity.go 定义模型请求附带的会话/父会话身份，供适配层作为
// 请求元数据或头部发送；不含密钥，仅作追踪与父子关联。
// Author: fxbin
package ports

// SessionIdentity 是模型请求的会话追踪标识。
type SessionIdentity struct {
	// SessionID 是当前会话标识。
	SessionID string `json:"session_id,omitempty"`
	// ParentSessionID 是父会话标识（fork/子运行来源）。
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// RunID 是当前运行标识。
	RunID string `json:"run_id,omitempty"`
	// ParentRunID 是父运行标识（AGT 子运行）。
	ParentRunID string `json:"parent_run_id,omitempty"`
}

// HeaderNames 返回应发送的请求头映射（空值省略）。
func (identity SessionIdentity) HeaderNames() map[string]string {
	headers := make(map[string]string, 4)
	if identity.SessionID != "" {
		headers["X-Cairn-Session-Id"] = identity.SessionID
	}
	if identity.ParentSessionID != "" {
		headers["X-Cairn-Parent-Session-Id"] = identity.ParentSessionID
	}
	if identity.RunID != "" {
		headers["X-Cairn-Run-Id"] = identity.RunID
	}
	if identity.ParentRunID != "" {
		headers["X-Cairn-Parent-Run-Id"] = identity.ParentRunID
	}
	return headers
}

// IsZero 表示无任何身份信息。
func (identity SessionIdentity) IsZero() bool {
	return identity.SessionID == "" && identity.ParentSessionID == "" && identity.RunID == "" && identity.ParentRunID == ""
}
