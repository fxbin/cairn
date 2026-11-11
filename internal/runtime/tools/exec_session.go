// exec_session.go 实现 EXE-04 受控 tmux 会话工具：open/write/read/close
// 四步生命周期；默认禁用；不绕过 Policy/审批，不记录按键原文。
// Author: fxbin
package tools

import (
	"context"
	"fmt"
	"time"

	"encoding/json"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// ExecSession 限制约束会话输入输出与生命周期。
const (
	ExecSessionMaxReadBytes = 64 * 1024
	ExecSessionMaxSessions  = 4
	ExecSessionTimeout      = 15 * time.Minute
)

// SessionResult 是会话动作结果。
type SessionResult struct {
	Action    SessionAction `json:"action"`
	SessionID string        `json:"session_id"`
	// Output 是 read 截取的文本；open/write/close 为空。
	Output string `json:"output,omitempty"`
	// Truncated 表示 read 被截断。
	Truncated bool `json:"truncated,omitempty"`
	// InputDigest 是 write 的输入摘要，替代按键原文入账。
	InputDigest string `json:"input_digest,omitempty"`
}

// ExecSession 是受控 tmux 会话工具；默认不注册，需显式启用。
type ExecSession struct {
	// Workspace 用于解析 CWD 边界。
	Workspace Workspace
	// Backend 是会话后端；nil 时执行失败关闭。
	Backend SessionBackend
}

// SessionBackend 抽象 tmux 会话操作，便于测试注入。
type SessionBackend interface {
	Open(sessionID, cwd string, env map[string]string) error
	Write(sessionID, content string) (string, error)
	Read(sessionID string, maxBytes int) (string, bool, error)
	Close(sessionID string) error
}

// Spec 返回工具声明。
func (ExecSession) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "exec_session",
		Description: "受控交互式终端会话（open/write/read/close）。默认禁用；建会话与敏感动词需授权。",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["action"],"properties":{"action":{"type":"string","enum":["open","write","read","close"]},"session_id":{"type":"string"},"content":{"type":"string"},"cwd":{"type":"string"}}}`),
	}
}

// SideEffect 声明会话属于进程级副作用。
func (ExecSession) SideEffect() contract.SideEffectClass { return contract.SideEffectProcess }

// Execute 校验请求后调用后端；后端缺失失败关闭。
func (tool ExecSession) Execute(ctx context.Context, raw json.RawMessage) contract.ToolResult {
	var request SessionRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return contract.ToolFailure("invalid_arguments", "exec_session 参数解析失败")
	}
	if err := ValidateSessionRequest(request); err != nil {
		return contract.ToolFailure("invalid_arguments", err.Error())
	}
	if tool.Backend == nil {
		return contract.ToolFailure("not_configured", "session backend is not configured")
	}
	if request.Action == SessionOpen {
		if _, _, err := tool.Workspace.Resolve(request.CWD); err != nil {
			return contract.ToolFailure("invalid_cwd", "cwd 越出工作区边界")
		}
	}
	switch request.Action {
	case SessionOpen:
		sessionID := request.SessionID
		if sessionID == "" {
			sessionID = "sess-" + fmt.Sprint(time.Now().UTC().UnixNano())
		}
		if err := tool.Backend.Open(sessionID, request.CWD, nil); err != nil {
			return contract.ToolFailure("session_open_failed", err.Error())
		}
		payload, _ := json.Marshal(SessionResult{Action: SessionOpen, SessionID: sessionID})
		return contract.ToolResult{OK: true, Code: "ok", Data: payload, Preview: "会话已开启 " + sessionID}
	case SessionWrite:
		digest, err := tool.Backend.Write(request.SessionID, request.Content)
		if err != nil {
			return contract.ToolFailure("session_write_failed", err.Error())
		}
		payload, _ := json.Marshal(SessionResult{Action: SessionWrite, SessionID: request.SessionID, InputDigest: digest})
		return contract.ToolResult{OK: true, Code: "ok", Data: payload, Preview: "输入摘要 " + digest}
	case SessionRead:
		output, truncated, err := tool.Backend.Read(request.SessionID, ExecSessionMaxReadBytes)
		if err != nil {
			return contract.ToolFailure("session_read_failed", err.Error())
		}
		payload, _ := json.Marshal(SessionResult{Action: SessionRead, SessionID: request.SessionID, Output: output, Truncated: truncated})
		return contract.ToolResult{OK: true, Code: "ok", Data: payload, Preview: fmt.Sprintf("读取 %d bytes", len(output)), Truncated: truncated}
	case SessionClose:
		if err := tool.Backend.Close(request.SessionID); err != nil {
			return contract.ToolFailure("session_close_failed", err.Error())
		}
		payload, _ := json.Marshal(SessionResult{Action: SessionClose, SessionID: request.SessionID})
		return contract.ToolResult{OK: true, Code: "ok", Data: payload, Preview: "会话已关闭"}
	}
	return contract.ToolFailure("internal", "unreachable")
}
