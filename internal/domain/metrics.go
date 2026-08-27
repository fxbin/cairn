// 本文件实现事件流证据指标提取。
//
// Author: fxbin
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// RunEvidenceMetrics 汇总从单一 Run 事件流提取的两项评审计数。
type RunEvidenceMetrics struct {
	// ToolPairingBreaks 统计工具调用配对破坏：未闭环的提议、无出处的结果与
	// 同一操作的重复结果各计一次。
	ToolPairingBreaks int
	// RepeatedExplorations 按 (operation_signature, progress_fingerprint) 对
	// outcome=continue 的关轮分组后累计 max(0, count-1)。
	RepeatedExplorations int
}

// explorationKey 是重复探索的归组键，两段都来自关轮负载的显式字段。
type explorationKey struct {
	operationSignature  string
	progressFingerprint string
}

// turnClosedExplorationProbe 用于区分关轮负载中签名字段的"键缺席"与"空串"：
// omitempty 反序列化无法表达这一差异，指针字段可以。
type turnClosedExplorationProbe struct {
	Outcome             string  `json:"outcome"`
	OperationSignature  *string `json:"operation_signature"`
	ProgressFingerprint *string `json:"progress_fingerprint"`
}

// CollectRunEvidenceMetrics 从按序事件流提取评审计数。continue 关轮若缺失
// 任一探索签名字段即整体失败关闭——补写轮与异常序列不得折算为"零扰动"混入
// 评分；final/failed/cancelled 关轮不参与重复探索统计。
func CollectRunEvidenceMetrics(events []Event) (RunEvidenceMetrics, error) {
	var metrics RunEvidenceMetrics
	explorations := map[explorationKey]int{}
	for _, event := range events {
		if event.Type != EventTurnClosed {
			continue
		}
		var probe turnClosedExplorationProbe
		if err := json.Unmarshal(event.Payload, &probe); err != nil {
			return metrics, fmt.Errorf("decode turn closed payload at seq %d: %w", event.Seq, err)
		}
		switch probe.Outcome {
		case "continue":
			if probe.OperationSignature == nil || probe.ProgressFingerprint == nil {
				return metrics, fmt.Errorf("continue turn at seq %d is missing its exploration signatures", event.Seq)
			}
			key := explorationKey{*probe.OperationSignature, *probe.ProgressFingerprint}
			explorations[key]++
			if explorations[key] > 1 {
				metrics.RepeatedExplorations++
			}
		case "final", "failed", "cancelled":
		default:
			return metrics, fmt.Errorf("turn closed at seq %d carries outcome %q outside the lexicon", event.Seq, probe.Outcome)
		}
	}
	return metrics, nil
}

// ToolCallPairingBreaks 扫描整条 Run 事件流，按操作标识统计三类配对破坏：
// 提议后没有结果追加（未闭环）、结果先于任何提议出现（无出处）以及同一操
// 作被多次追加结果（重复闭合）。
func ToolCallPairingBreaks(events []Event) (int, error) {
	breaks := 0
	proposed := map[OperationID]int{}
	resulted := map[OperationID]bool{}
	for _, event := range events {
		switch event.Type {
		case EventToolCallProposed:
			if event.OperationID == nil || *event.OperationID == "" {
				return 0, errors.New("tool call proposal without an operation id")
			}
			id := *event.OperationID
			proposed[id]++
			if proposed[id] > 1 {
				return 0, fmt.Errorf("duplicate tool call proposal for operation %s", id)
			}
			if resulted[id] {
				return 0, fmt.Errorf("tool call proposal for operation %s appears after its result", id)
			}
		case EventToolResultAppended:
			if event.OperationID == nil || *event.OperationID == "" {
				return 0, errors.New("tool result without an operation id")
			}
			id := *event.OperationID
			if resulted[id] {
				return 0, fmt.Errorf("duplicate tool result for operation %s", id)
			}
			resulted[id] = true
			if proposed[id] == 0 {
				breaks++
			}
		default:
		}
	}
	for id, count := range proposed {
		if !resulted[id] && count > 0 {
			breaks++
		}
	}
	return breaks, nil
}
