// session.go 实现会话前情卡的确定性投影与自动装配（ADR-0018）：由上一
// run 的已持久化事件生成可复现、可审计的前情文本，内容寻址落制品库并随
// 创建事件引用。投影不调用模型、不含密钥与任务正文以外的敏感内容。
//
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// priorContextResponseLimit 是前情卡最终回复段落的字符上限；完整回复以事
// 件与制品为准，前情只需可辨识的结论轮廓。
const priorContextResponseLimit = 1600

// PriorContextCard 把上一 run 的事件流投影为前情卡文本：任务原文、最终
// 回复、终态与失败分类、变更文件清单与验证结论。输入必须是以 run_created
// 开头的完整事件序列；缺失字段如实标注，不推断、不补造。
func PriorContextCard(events []domain.Event) string {
	if len(events) == 0 {
		return ""
	}
	var task, finalResponse, failureClass string
	proposedPatches := make(map[domain.OperationID]string)
	changedFiles := make(map[string]struct{})
	state := "unknown"
	verification := "未执行"
	for _, event := range events {
		switch event.Type {
		case domain.EventRunCreated:
			var payload domain.RunCreatedPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				task = payload.Task
			}
		case domain.EventToolCallProposed:
			if event.OperationID == nil || *event.OperationID == "" {
				continue
			}
			var payload struct {
				ToolName  string          `json:"tool_name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.ToolName != "apply_patch" {
				continue
			}
			var patch struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(payload.Arguments, &patch) == nil && patch.Path != "" {
				proposedPatches[*event.OperationID] = patch.Path
			}
		case domain.EventOperationSucceeded:
			if event.OperationID == nil || *event.OperationID == "" {
				continue
			}
			var payload struct {
				ToolName string `json:"tool_name"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.ToolName != "apply_patch" {
				continue
			}
			if path := proposedPatches[*event.OperationID]; path != "" {
				changedFiles[path] = struct{}{}
			}
		case domain.EventVerificationFinished:
			var payload domain.VerificationFinishedPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				if payload.OK {
					verification = "通过"
				} else {
					verification = "未通过"
				}
			}
		case domain.EventRunCompleted:
			var payload struct {
				FinalResponse string `json:"final_response"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				finalResponse = payload.FinalResponse
			}
			state = string(domain.RunCompleted)
		case domain.EventRunFailed:
			var payload struct {
				Reason     string `json:"reason"`
				ErrorClass string `json:"error_class"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				failureClass = payload.ErrorClass
				if failureClass == "" {
					failureClass = payload.Reason
				}
			}
			state = string(domain.RunFailed)
		case domain.EventRunCancelled:
			state = string(domain.RunCancelled)
		}
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "任务: %s\n", strings.TrimSpace(task))
	if state == string(domain.RunFailed) {
		fmt.Fprintf(&builder, "终态: 失败（%s）\n", failureClass)
	} else {
		fmt.Fprintf(&builder, "终态: %s\n", stateText(state))
	}
	if len(changedFiles) > 0 {
		files := make([]string, 0, len(changedFiles))
		for path := range changedFiles {
			files = append(files, path)
		}
		sort.Strings(files)
		fmt.Fprintf(&builder, "变更文件: %s\n", strings.Join(files, "、"))
	}
	fmt.Fprintf(&builder, "验证: %s\n", verification)
	if response := strings.TrimSpace(finalResponse); response != "" {
		runes := []rune(response)
		if len(runes) > priorContextResponseLimit {
			response = string(runes[:priorContextResponseLimit]) + "…"
		}
		fmt.Fprintf(&builder, "最终回复:\n%s\n", response)
	}
	return builder.String()
}

// stateText 把运行终态映射为前情卡短语。
func stateText(state string) string {
	switch state {
	case string(domain.RunCompleted):
		return "完成"
	case string(domain.RunCancelled):
		return "已取消"
	default:
		return "未知"
	}
}

// resolveSessionContext 装配本次运行的会话链关系。显式前情总是重新写入
// 内容寻址制品库并返回规范引用；仅有引用时必须完整读取校验。仅给出前置
// 运行且没有显式前情时，从其事件投影前情卡并持久化。调用方必须先完成
// 前置运行的制品引用与 v2 冻结合同校验。
func (service Service) resolveSessionContext(ctx context.Context, journal ports.Journal, artifacts *artifact.Store, options RunOptions) (string, string, *artifact.Ref, error) {
	sessionID := options.SessionID
	priorContext := options.PriorContext
	priorContextRef := options.PriorContextRef
	if priorContext != "" {
		if artifacts == nil {
			return "", "", nil, errors.New("artifact store unavailable for prior context")
		}
		ref, err := artifacts.Put([]byte(priorContext), "text/plain")
		if err != nil {
			return "", "", nil, fmt.Errorf("store explicit prior context: %w", err)
		}
		priorContextRef = &ref
		content, err := artifacts.Open(ref)
		if err != nil {
			return "", "", nil, fmt.Errorf("open stored explicit prior context: %w", err)
		}
		priorContext = string(content)
	} else if priorContextRef != nil {
		if artifacts == nil {
			return "", "", nil, errors.New("artifact store unavailable for prior context")
		}
		content, err := artifacts.Open(*priorContextRef)
		if err != nil {
			return "", "", nil, fmt.Errorf("open prior context artifact: %w", err)
		}
		priorContext = string(content)
	}
	if options.PrevRunID == "" {
		return sessionID, priorContext, priorContextRef, nil
	}
	previous, err := journal.Load(ctx, options.PrevRunID)
	if err != nil {
		return "", "", nil, fmt.Errorf("load prior run %s for session context: %w", options.PrevRunID, err)
	}
	if len(previous) == 0 || previous[0].Type != domain.EventRunCreated {
		return "", "", nil, fmt.Errorf("prior run %s carries no replayable events", options.PrevRunID)
	}
	var createdPayload domain.RunCreatedPayload
	if json.Unmarshal(previous[0].Payload, &createdPayload) == nil && sessionID == "" {
		sessionID = createdPayload.SessionID
	}
	if sessionID == "" {
		sessionID = domain.NewID("session")
	}
	if priorContext == "" {
		card := PriorContextCard(previous)
		if strings.TrimSpace(card) == "" {
			return "", "", nil, fmt.Errorf("prior run %s projects an empty context card", options.PrevRunID)
		}
		ref, err := artifacts.Put([]byte(card), "text/plain")
		if err != nil {
			return "", "", nil, fmt.Errorf("store prior context card: %w", err)
		}
		content, err := artifacts.Open(ref)
		if err != nil {
			return "", "", nil, fmt.Errorf("open stored prior context card: %w", err)
		}
		priorContext = string(content)
		priorContextRef = &ref
	}
	return sessionID, priorContext, priorContextRef, nil
}
