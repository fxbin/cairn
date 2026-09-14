// Package agent 的工具定位摘要从已脱敏回执提取有界证据，不复制原始数据体。
//
// Author: fxbin
package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	checkpointToolEvidenceMaxBytes  = 1024
	checkpointSearchMatchLimit      = 3
	checkpointSearchPreviewMaxBytes = 160
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
	var data struct {
		Path       string `json:"path"`
		Offset     *int64 `json:"offset"`
		Limit      *int64 `json:"limit"`
		TotalBytes *int64 `json:"total_bytes"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Path == "" || data.Offset == nil || data.Limit == nil || data.TotalBytes == nil {
		return ""
	}
	offset, limit, total := *data.Offset, *data.Limit, *data.TotalBytes
	if offset < 0 || limit < 0 || total < offset || limit == 0 && offset != total {
		return ""
	}
	end := offset + min(limit, total-offset)
	evidence := fmt.Sprintf("historical_read: path=%q byte_range=[%d,%d) total_bytes=%d", data.Path, offset, end, total)
	if len(evidence) > checkpointToolEvidenceMaxBytes {
		return ""
	}
	return evidence
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
