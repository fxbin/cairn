// todo.go 提供长任务可见进度工具：增改查计划项，结果进 Journal 供 UI 投影。
// 状态挂在 Run 装配的 TodoBoard 上，不授予任何权限。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/fxbin/cairn/internal/ports"
)

// TodoItem 是一条计划项。
type TodoItem struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

// TodoBoard 保存一次运行内的计划状态；线程安全。
type TodoBoard struct {
	mu    sync.Mutex
	items []TodoItem
}

// NewTodoBoard 创建空计划板。
func NewTodoBoard() *TodoBoard { return &TodoBoard{items: []TodoItem{}} }

// Snapshot 返回当前计划副本。
func (board *TodoBoard) Snapshot() []TodoItem {
	if board == nil {
		return nil
	}
	board.mu.Lock()
	defer board.mu.Unlock()
	out := make([]TodoItem, len(board.items))
	copy(out, board.items)
	return out
}

// Todo 是计划/TODO 工具。
type Todo struct {
	Board *TodoBoard
}

// SideEffect 声明 todo 无副作用。
func (Todo) SideEffect() SideEffectClass { return SideEffectNone }

// Spec 返回 todo 工具定义。
func (tool Todo) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "todo",
		Description: "Track a short ordered plan for long tasks so the user can see progress. Actions: list, add, update, complete, clear. Use stable short ids.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["action"],"properties":{"action":{"type":"string","enum":["list","add","update","complete","clear"]},"id":{"type":"string"},"text":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","done"]}}}`),
	}
}

// Execute 处理 todo 动作；每次结果携带完整清单，Journal 可回放进度。
func (tool Todo) Execute(ctx context.Context, raw json.RawMessage) Result {
	if tool.Board == nil {
		return Failure("not_configured", "todo board is not configured")
	}
	var args struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		Text   string `json:"text"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return Failure("invalid_arguments", "todo arguments invalid")
	}
	board := tool.Board
	board.mu.Lock()
	defer board.mu.Unlock()
	switch args.Action {
	case "list":
	case "add":
		text := strings.TrimSpace(args.Text)
		if text == "" {
			return Failure("invalid_arguments", "text is required for add")
		}
		id := strings.TrimSpace(args.ID)
		if id == "" {
			id = fmt.Sprintf("t%d", len(board.items)+1)
		}
		for _, item := range board.items {
			if item.ID == id {
				return Failure("invalid_arguments", "todo id already exists")
			}
		}
		board.items = append(board.items, TodoItem{ID: id, Text: text, Status: "pending"})
	case "update", "complete":
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return Failure("invalid_arguments", "id is required")
		}
		status := args.Status
		if args.Action == "complete" {
			status = "done"
		}
		if status == "" {
			status = "in_progress"
		}
		found := false
		for index := range board.items {
			if board.items[index].ID != id {
				continue
			}
			if text := strings.TrimSpace(args.Text); text != "" {
				board.items[index].Text = text
			}
			board.items[index].Status = status
			found = true
		}
		if !found {
			return Failure("invalid_arguments", "todo id not found")
		}
	case "clear":
		board.items = nil
	default:
		return Failure("invalid_arguments", "unknown todo action")
	}
	snapshot := make([]TodoItem, len(board.items))
	copy(snapshot, board.items)
	data := map[string]any{"items": snapshot}
	return Success(data, fmt.Sprintf("%d item(s)", len(snapshot)), 0, "", false)
}
