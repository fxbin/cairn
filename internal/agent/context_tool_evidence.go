// Package agent 的工具定位摘要从已脱敏回执提取有界证据，不复制原始数据体。
//
// Author: fxbin
package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
)

// checkpointReadEvidence 是成功读取回执中的历史字节定位，不从正文长度推断范围。
type checkpointReadEvidence struct {
	Path       string
	Offset     int64
	End        int64
	TotalBytes int64
}

const (
	checkpointToolEvidenceMaxBytes   = 1024
	checkpointSearchMatchLimit       = 3
	checkpointSearchPreviewMaxBytes  = 160
	checkpointRecoveryActionMaxBytes = 480
)

// checkpointToolEvidence 根据已知调用名选择定位字段；未知或畸形回执不推断证据。
// data 必须先经完整 JSON 脱敏，再进入此处的截断与格式化。
func checkpointToolEvidence(name string, data json.RawMessage) string {
	switch name {
	case "read_file":
		return checkpointReadLocation(data)
	case "search_text":
		return checkpointSearchLocations(data)
	default:
		return ""
	}
}

// checkpointReadLocation 保留读取时的字节区间；缺字段不推断零值，区间按原始
// 字节元数据计算，不能使用已脱敏或经 UTF-8 替换后的正文长度。
func checkpointReadLocation(raw json.RawMessage) string {
	data, ok := decodeCheckpointReadEvidence(raw)
	if !ok {
		return ""
	}
	evidence := fmt.Sprintf("historical_read: path=%q byte_range=[%d,%d) total_bytes=%d", data.Path, data.Offset, data.End, data.TotalBytes)
	if len(evidence) > checkpointToolEvidenceMaxBytes {
		return ""
	}
	return evidence
}

// decodeCheckpointReadEvidence 校验完整定位元数据，防止缺字段、越界或整数溢出产生虚构范围。
func decodeCheckpointReadEvidence(raw json.RawMessage) (checkpointReadEvidence, bool) {
	var data struct {
		Path       string `json:"path"`
		Offset     *int64 `json:"offset"`
		Limit      *int64 `json:"limit"`
		TotalBytes *int64 `json:"total_bytes"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Path == "" || data.Offset == nil || data.Limit == nil || data.TotalBytes == nil {
		return checkpointReadEvidence{}, false
	}
	offset, limit, total := *data.Offset, *data.Limit, *data.TotalBytes
	if offset < 0 || limit < 0 || total < offset || limit == 0 && offset != total {
		return checkpointReadEvidence{}, false
	}
	end := offset + min(limit, total-offset)
	return checkpointReadEvidence{Path: data.Path, Offset: offset, End: end, TotalBytes: total}, true
}

// checkpointPatchRecovery 保留已知补丁错误的有限恢复字段；未知字段、原文与候选代码不进入摘要。
// data 必须先完整脱敏，next_action 仅作为历史工具反馈保留，不获得任务或权限优先级。
func checkpointPatchRecovery(code string, raw json.RawMessage) string {
	if code != tools.CodeInvalidPatchArguments && code != tools.CodePatchTextMismatch {
		return ""
	}
	var data struct {
		Mode                string `json:"mode"`
		Path                string `json:"path"`
		ReplacementIndex    *int   `json:"replacement_index"`
		ActualOccurrences   *int   `json:"actual_occurrences"`
		ExpectedOccurrences *int   `json:"expected_occurrences"`
		NextAction          string `json:"next_action"`
	}
	if json.Unmarshal(raw, &data) != nil || data.NextAction == "" {
		return ""
	}
	parts := []string{}
	if code == tools.CodeInvalidPatchArguments {
		if data.Mode == "create" || data.Mode == "update" || data.Mode == "delete" {
			parts = append(parts, "mode="+data.Mode)
		}
	} else {
		if data.Path == "" || data.ReplacementIndex == nil || *data.ReplacementIndex < 0 || data.ActualOccurrences == nil || *data.ActualOccurrences < 0 || data.ExpectedOccurrences == nil || *data.ExpectedOccurrences < 1 {
			return ""
		}
		parts = append(parts, fmt.Sprintf("path=%q replacement_index=%d actual_occurrences=%d expected_occurrences=%d", clipText(data.Path, checkpointSearchPreviewMaxBytes), *data.ReplacementIndex, *data.ActualOccurrences, *data.ExpectedOccurrences))
	}
	parts = append(parts, "next_action="+clipText(data.NextAction, checkpointRecoveryActionMaxBytes))
	return "historical_patch_recovery: " + strings.Join(parts, " ")
}

// checkpointSearchLocations 保留有限条历史搜索位置和短预览，明确省略数；
// 旧回执缺少字节偏移时仅保留行号，文件变化后位置仍需重新核对。
func checkpointSearchLocations(raw json.RawMessage) string {
	var data struct {
		Matches []struct {
			Path       string `json:"path"`
			Line       int64  `json:"line"`
			ByteOffset *int64 `json:"byte_offset"`
			Preview    string `json:"preview"`
		} `json:"matches"`
	}
	if json.Unmarshal(raw, &data) != nil || len(data.Matches) == 0 {
		return ""
	}
	const prefix = "historical_search_locations: "
	entries := make([]string, 0, checkpointSearchMatchLimit)
	used := len(prefix)
	omittedReserve := len(fmt.Sprintf("; omitted_matches=%d", len(data.Matches)))
	for _, match := range data.Matches {
		if len(entries) >= checkpointSearchMatchLimit {
			break
		}
		if match.Path == "" || match.Line < 1 || match.ByteOffset != nil && *match.ByteOffset < 0 {
			continue
		}
		entry := fmt.Sprintf("path=%q line=%d", match.Path, match.Line)
		if match.ByteOffset != nil {
			entry += fmt.Sprintf(" byte_offset=%d", *match.ByteOffset)
		}
		if match.Preview != "" {
			entry += fmt.Sprintf(" preview=%q", clipText(match.Preview, checkpointSearchPreviewMaxBytes))
		}
		if used+len(entry)+len("; ")+omittedReserve > checkpointToolEvidenceMaxBytes {
			break
		}
		entries = append(entries, entry)
		used += len(entry) + len("; ")
	}
	omitted := len(data.Matches) - len(entries)
	if omitted > 0 {
		entries = append(entries, fmt.Sprintf("omitted_matches=%d", omitted))
	}
	return prefix + strings.Join(entries, "; ")
}
