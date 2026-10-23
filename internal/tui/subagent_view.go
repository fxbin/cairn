// subagent_view.go 定义 TUI 子运行展示模型；由事件投影填充。
// Author: fxbin
package tui

import (
	"encoding/json"

	"github.com/fxbin/cairn/internal/domain"
)

// SubagentView 是子运行的只读展示快照。
type SubagentView struct {
	ChildRunID      string
	ParentRunID     string
	Task            string
	State           string
	Outcome         string
	ToolCalls       int
	Tokens          int64
	PendingApproval bool
}

// populateSubagentView 从 subagent 事件填充展示投影。
func (console *RunConsole) populateSubagentView(event domain.Event) {
	switch event.Type {
	case domain.EventSubagentDelegated:
		var payload struct {
			ChildRunID string `json:"child_run_id"`
			TaskDigest string `json:"task_digest"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.ChildRunID == "" {
			return
		}
		for _, view := range console.subagentViews {
			if view.ChildRunID == payload.ChildRunID {
				return
			}
		}
		console.subagentViews = append(console.subagentViews, SubagentView{
			ChildRunID:  payload.ChildRunID,
			ParentRunID: string(event.RunID),
			Task:        payload.TaskDigest,
			State:       "delegated",
		})
	case domain.EventSubagentCompleted:
		var payload struct {
			ChildRunID string `json:"child_run_id"`
			Outcome    string `json:"outcome"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return
		}
		for index := range console.subagentViews {
			if console.subagentViews[index].ChildRunID == payload.ChildRunID {
				console.subagentViews[index].State = "completed"
				console.subagentViews[index].Outcome = payload.Outcome
				return
			}
		}
		console.subagentViews = append(console.subagentViews, SubagentView{
			ChildRunID: payload.ChildRunID,
			State:      "completed",
			Outcome:    payload.Outcome,
		})
	}
}
