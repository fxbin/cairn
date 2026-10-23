// exec_session.go 实现 EXE-04 受控 tmux 会话工具：open/write/read/close
// 四步生命周期；默认禁用；不绕过 Policy/审批，不记录按键原文。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/ports"
)

// ExecSession 限制约束会话输入输出与生命周期。
const (
	ExecSessionMaxWriteBytes = 4 * 1024
	ExecSessionMaxReadBytes  = 64 * 1024
	ExecSessionMaxSessions   = 4
	ExecSessionTimeout       = 15 * time.Minute
)

// SessionAction 是会话动作类型。
type SessionAction string

const (
	// SessionOpen 创建受控 tmux 会话。
	SessionOpen SessionAction = "open"
	// SessionWrite 向会话注入输入。
	SessionWrite SessionAction = "write"
	// SessionRead 读取自游标起的输出。
	SessionRead SessionAction = "read"
	// SessionClose 关闭会话并回收进程组。
	SessionClose SessionAction = "close"
)

// SessionRequest 是 exec_session 的统一入参。
type SessionRequest struct {
	Action SessionAction `json:"action"`
	// SessionID 是会话身份；open 时可省略由运行时分配。
	SessionID string `json:"session_id,omitempty"`
	// Content 是 write 的输入文本（摘要入 Journal，不记原文）。
	Content string `json:"content,omitempty"`
	// CWD 是会话工作目录，必须在工作区内。
	CWD string `json:"cwd,omitempty"`
}

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

// sensitiveVerbs 是 write 需二次确认的敏感动词（大小写不敏感）。
var sensitiveVerbs = []string{"sudo", "rm -rf", "curl", "wget", "chmod 777", "shutdown", "reboot", "mkfs", "dd if="}

// IsSensitiveInput 判断输入是否触发二次确认。
func IsSensitiveInput(content string) bool {
	lower := strings.ToLower(content)
	for _, verb := range sensitiveVerbs {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	return false
}

// ValidateSessionRequest 校验会话动作；不执行任何外部调用。
func ValidateSessionRequest(request SessionRequest) error {
	switch request.Action {
	case SessionOpen:
		if strings.TrimSpace(request.CWD) == "" {
			return fmt.Errorf("cwd is required for open")
		}
	case SessionWrite:
		if strings.TrimSpace(request.SessionID) == "" {
			return fmt.Errorf("session_id is required for write")
		}
		if strings.TrimSpace(request.Content) == "" {
			return fmt.Errorf("content is required for write")
		}
		if !utf8.ValidString(request.Content) {
			return fmt.Errorf("content must be valid utf-8")
		}
		if len(request.Content) > ExecSessionMaxWriteBytes {
			return fmt.Errorf("content exceeds %d bytes", ExecSessionMaxWriteBytes)
		}
	case SessionRead, SessionClose:
		if strings.TrimSpace(request.SessionID) == "" {
			return fmt.Errorf("session_id is required for %s", request.Action)
		}
	default:
		return fmt.Errorf("unknown session action %q", request.Action)
	}
	return nil
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
func (ExecSession) SideEffect() SideEffectClass { return SideEffectProcess }

// Execute 校验请求后调用后端；后端缺失失败关闭。
func (tool ExecSession) Execute(ctx context.Context, raw json.RawMessage) Result {
	var request SessionRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return Failure("invalid_arguments", "exec_session 参数解析失败")
	}
	if err := ValidateSessionRequest(request); err != nil {
		return Failure("invalid_arguments", err.Error())
	}
	if tool.Backend == nil {
		return Failure("not_configured", "session backend is not configured")
	}
	if request.Action == SessionOpen {
		if _, _, err := tool.Workspace.Resolve(request.CWD); err != nil {
			return Failure("invalid_cwd", "cwd 越出工作区边界")
		}
	}
	switch request.Action {
	case SessionOpen:
		sessionID := request.SessionID
		if sessionID == "" {
			sessionID = "sess-" + fmt.Sprint(time.Now().UTC().UnixNano())
		}
		if err := tool.Backend.Open(sessionID, request.CWD, nil); err != nil {
			return Failure("session_open_failed", err.Error())
		}
		payload, _ := json.Marshal(SessionResult{Action: SessionOpen, SessionID: sessionID})
		return Result{OK: true, Code: "ok", Data: payload, Preview: "会话已开启 " + sessionID}
	case SessionWrite:
		digest, err := tool.Backend.Write(request.SessionID, request.Content)
		if err != nil {
			return Failure("session_write_failed", err.Error())
		}
		payload, _ := json.Marshal(SessionResult{Action: SessionWrite, SessionID: request.SessionID, InputDigest: digest})
		return Result{OK: true, Code: "ok", Data: payload, Preview: "输入摘要 " + digest}
	case SessionRead:
		output, truncated, err := tool.Backend.Read(request.SessionID, ExecSessionMaxReadBytes)
		if err != nil {
			return Failure("session_read_failed", err.Error())
		}
		payload, _ := json.Marshal(SessionResult{Action: SessionRead, SessionID: request.SessionID, Output: output, Truncated: truncated})
		return Result{OK: true, Code: "ok", Data: payload, Preview: fmt.Sprintf("读取 %d bytes", len(output)), Truncated: truncated}
	case SessionClose:
		if err := tool.Backend.Close(request.SessionID); err != nil {
			return Failure("session_close_failed", err.Error())
		}
		payload, _ := json.Marshal(SessionResult{Action: SessionClose, SessionID: request.SessionID})
		return Result{OK: true, Code: "ok", Data: payload, Preview: "会话已关闭"}
	}
	return Failure("internal", "unreachable")
}
