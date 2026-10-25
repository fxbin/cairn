// deliver_run_message.go 把 send_message 内容写入对端 run 的 Journal：
// 内容入制品库，事件用 steering_input_submitted 携带 content_ref。
// Author: fxbin
package app

import (
	"context"
	"fmt"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// deliverRunMessage 将消息投递到目标 run：制品存内容，Journal 追加 steering 事实。
func deliverRunMessage(ctx context.Context, journal ports.Journal, artifacts *artifact.Store, workspaceHash string, toRunID domain.RunID, content, reason string) error {
	if journal == nil {
		return fmt.Errorf("journal is required")
	}
	if toRunID == "" {
		return fmt.Errorf("to_run_id is required")
	}
	events, err := journal.Load(ctx, toRunID)
	if err != nil {
		return fmt.Errorf("load target run: %w", err)
	}
	seq := uint64(len(events))
	var contentRef *artifact.Ref
	if artifacts != nil {
		ref, err := artifacts.Put([]byte(content), "text/plain")
		if err != nil {
			return fmt.Errorf("store message: %w", err)
		}
		contentRef = &ref
	}
	payload := map[string]any{
		"payload_version": 1,
		"input_id":        domain.NewID("msg"),
		"kind":            "steer",
	}
	if contentRef != nil {
		payload["content_ref"] = contentRef
	}
	if reason != "" {
		payload["reason"] = reason
	}
	raw := artifact.MustEncodeRedactedJSON(payload)
	_, err = journal.Append(ctx, seq, domain.EventDraft{
		RunID:         toRunID,
		WorkspaceHash: workspaceHash,
		Type:          domain.EventSteeringInputSubmitted,
		Payload:       raw,
	})
	if err != nil {
		return fmt.Errorf("append message event: %w", err)
	}
	return nil
}
