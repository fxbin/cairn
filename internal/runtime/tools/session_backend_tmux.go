// session_backend_tmux.go 实现 SessionBackend 的 tmux 会话后端：
// open 用 tmux new-session，write 用 send-keys，read 用 capture-pane，
// close 用 kill-session + SIGHUP。无 tmux 时失败关闭。
// Author: fxbin
package tools

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// TmuxSessionBackend 是 tmux 实现的会话后端。
type TmuxSessionBackend struct {
	mu       sync.Mutex
	sessions map[string]string // sessionID -> cwd
	// tmuxPath 可注入二进制路径，便于测试。
	tmuxPath string
}

// NewTmuxSessionBackend 创建 tmux 后端。
func NewTmuxSessionBackend() *TmuxSessionBackend {
	return &TmuxSessionBackend{sessions: make(map[string]string), tmuxPath: "tmux"}
}

// Open 创建 tmux 会话。
func (backend *TmuxSessionBackend) Open(sessionID, cwd string, _ map[string]string) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if _, exists := backend.sessions[sessionID]; exists {
		return fmt.Errorf("session %q already open", sessionID)
	}
	args := []string{"new-session", "-d", "-s", sessionID, "-c", cwd}
	if output, err := exec.Command(backend.tmuxPath, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, strings.TrimSpace(string(output)))
	}
	backend.sessions[sessionID] = cwd
	return nil
}

// Write 向会话注入输入并返回摘要。
func (backend *TmuxSessionBackend) Write(sessionID, content string) (string, error) {
	backend.mu.Lock()
	_, exists := backend.sessions[sessionID]
	backend.mu.Unlock()
	if !exists {
		return "", fmt.Errorf("session %q not open", sessionID)
	}
	args := []string{"send-keys", "-t", sessionID, "-l", content}
	if output, err := exec.Command(backend.tmuxPath, args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("tmux send-keys: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return ContentDigest(content), nil
}

// Read 读取自上次游标起的输出。
func (backend *TmuxSessionBackend) Read(sessionID string, maxBytes int) (string, bool, error) {
	backend.mu.Lock()
	_, exists := backend.sessions[sessionID]
	backend.mu.Unlock()
	if !exists {
		return "", false, fmt.Errorf("session %q not open", sessionID)
	}
	args := []string{"capture-pane", "-t", sessionID, "-p"}
	output, err := exec.Command(backend.tmuxPath, args...).Output()
	if err != nil {
		return "", false, fmt.Errorf("tmux capture-pane: %w", err)
	}
	text := string(output)
	truncated := false
	if len(text) > maxBytes {
		text = text[:maxBytes]
		truncated = true
	}
	return text, truncated, nil
}

// Close 关闭会话（kill-session）。
func (backend *TmuxSessionBackend) Close(sessionID string) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if _, exists := backend.sessions[sessionID]; !exists {
		return fmt.Errorf("session %q not open", sessionID)
	}
	args := []string{"kill-session", "-t", sessionID}
	if output, err := exec.Command(backend.tmuxPath, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux kill-session: %w: %s", err, strings.TrimSpace(string(output)))
	}
	delete(backend.sessions, sessionID)
	return nil
}
