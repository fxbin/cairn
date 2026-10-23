// subagent_progress.go 实现 AGT-03 子任务进度与消息投影：从父/子 Journal
// 只读汇总真实子运行状态、消息身份与工具次数；外部输入不扩大授权。
// Author: fxbin
package app

import (
	"context"
	"fmt"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// SubagentMessage 是父子消息投影，含明确身份。
type SubagentMessage struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Kind      string `json:"kind"`
	Preview   string `json:"preview"`
	Timestamp string `json:"timestamp,omitempty"`
}

// SubagentProgress 是单个子运行的真实进度快照。
type SubagentProgress struct {
	ChildRunID      string            `json:"child_run_id"`
	ParentRunID     string            `json:"parent_run_id"`
	State           string            `json:"state"`
	Outcome         string            `json:"outcome,omitempty"`
	Turns           int               `json:"turns"`
	ToolCalls       int               `json:"tool_calls"`
	PendingApproval bool              `json:"pending_approval"`
	LastSeq         uint64            `json:"last_seq"`
	Messages        []SubagentMessage `json:"messages,omitempty"`
}

// ProjectSubagentProgress 从父运行事件流投影各子进度。
// 只读，不写 Journal；子 run 存在独立历史时用 LoadChildDetails 补全。
func ProjectSubagentProgress(parentRunID domain.RunID, parentEvents []domain.Event) []SubagentProgress {
	type acc struct {
		record   domain.SubagentRecord
		turns    int
		toolCall int
		messages []SubagentMessage
		lastSeq  uint64
	}
	table := map[string]*acc{}
	order := make([]string, 0, 4)
	for _, event := range parentEvents {
		switch event.Type {
		case domain.EventSubagentDelegated:
			var payload DelegatedPayload
			if err := jsonUnmarshal(event.Payload, &payload); err != nil {
				continue
			}
			if _, ok := table[payload.ChildRunID]; !ok {
				order = append(order, payload.ChildRunID)
			}
			entry := table[payload.ChildRunID]
			if entry == nil {
				entry = &acc{}
				table[payload.ChildRunID] = entry
			}
			entry.record = domain.SubagentRecord{ChildRunID: payload.ChildRunID, State: "delegated"}
			entry.messages = append(entry.messages, SubagentMessage{
				From: string(parentRunID), To: payload.ChildRunID, Kind: "delegate",
				Preview: "delegated " + payload.TaskDigest,
			})
		case domain.EventSubagentCompleted:
			var payload CompletedPayload
			if err := jsonUnmarshal(event.Payload, &payload); err != nil {
				continue
			}
			entry := table[payload.ChildRunID]
			if entry == nil {
				continue
			}
			entry.record.State = "completed"
			entry.record.Outcome = payload.Outcome
			entry.messages = append(entry.messages, SubagentMessage{
				From: payload.ChildRunID, To: string(parentRunID), Kind: "complete",
				Preview: payload.Outcome,
			})
		case domain.EventSteeringInputSubmitted:
			var payload struct {
				InputID string `json:"input_id"`
				Kind    string `json:"kind"`
			}
			if err := jsonUnmarshal(event.Payload, &payload); err != nil {
				continue
			}
			// 父运行 steering 附带给所有 delegated 子身份标注来源
			for id, entry := range table {
				if entry.record.State != "delegated" {
					continue
				}
				entry.messages = append(entry.messages, SubagentMessage{
					From: string(parentRunID), To: id, Kind: "steer",
					Preview: payload.InputID,
				})
			}
		}
		entry := table[string(event.RunID)]
		if entry != nil {
			entry.lastSeq = event.Seq
		}
	}
	out := make([]SubagentProgress, 0, len(order))
	for _, id := range order {
		entry := table[id]
		if entry == nil {
			continue
		}
		out = append(out, SubagentProgress{
			ChildRunID:  id,
			ParentRunID: string(parentRunID),
			State:       entry.record.State,
			Outcome:     entry.record.Outcome,
			ToolCalls:   entry.toolCall,
			Messages:    entry.messages,
			LastSeq:     entry.lastSeq,
		})
	}
	return out
}

// LoadChildDetails 从子运行事件补全轮次、工具与待审批事实。
func LoadChildDetails(ctx context.Context, journal ports.Journal, progress *SubagentProgress) error {
	if journal == nil || progress == nil || progress.ChildRunID == "" {
		return fmt.Errorf("journal and child id are required")
	}
	events, err := journal.Load(ctx, domain.RunID(progress.ChildRunID))
	if err != nil {
		return fmt.Errorf("load child run: %w", err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return fmt.Errorf("replay child run: %w", err)
	}
	for _, event := range events {
		if event.Type == domain.EventTurnOpened {
			progress.Turns++
		}
	}
	for _, event := range events {
		if event.Type == domain.EventToolCallProposed {
			progress.ToolCalls++
		}
	}
	progress.PendingApproval = projection.PendingApproval != nil
	progress.LastSeq = projection.LastSeq
	return nil
}
