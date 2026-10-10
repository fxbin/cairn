// Package agent 的探索清单仅从配对成功回执生成，保留历史范围而不复制源码。
//
// Author: fxbin
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/ports"
)

// 清单的定位文本与离散范围保持有界；被省略的范围不能被宣称为已覆盖。
const (
	explorationTargetMaxBytes  = 160
	explorationReadRangeLimit  = 4
	explorationInventoryHeader = "已检查对象（成功回执，仅历史范围）：\n"
)

// explorationByteRange 是回执确认的半开字节区间，不包含源码正文。
type explorationByteRange struct {
	Start int64
	End   int64
}

// explorationInventoryEntry 按历史工具目标或文件版本合并成功证据，记录最近位置用于有限预算排序。
type explorationInventoryEntry struct {
	Name       string
	Target     string
	SHA256     string
	TotalBytes int64
	Ranges     []explorationByteRange
	Latest     int
}

// explorationInventory 按消息顺序配对调用与成功回执；有限预算先保留最新锚点，
// 再保留早期定位。仅同一已确认 SHA 的读取区间允许合并，exec 只保留程序名。
func explorationInventory(messages []ports.Message, maximumBytes int) string {
	if maximumBytes < len(explorationInventoryHeader) {
		return ""
	}
	calls := make(map[string]ports.ToolCall)
	entries := make([]explorationInventoryEntry, 0, 16)
	positions := make(map[string]int)
	for index, message := range messages {
		for _, call := range message.ToolCalls {
			calls[call.ID] = call
		}
		if message.Role != "tool" {
			continue
		}
		call, exists := calls[message.ToolCallID]
		if !exists {
			continue
		}
		delete(calls, message.ToolCallID)
		var result struct {
			OK     bool            `json:"ok"`
			SHA256 string          `json:"sha256"`
			Data   json.RawMessage `json:"data"`
		}
		if json.Unmarshal(artifact.RedactJSON([]byte(message.Content)), &result) != nil || !result.OK {
			continue
		}
		target := toolExplorationTarget(call.Name, call.Arguments)
		if target == "" {
			continue
		}
		entry := explorationInventoryEntry{Name: call.Name, Target: target, Latest: index}
		key := call.Name + "\x00" + target
		// 展示可以截断查询，但不同原始条件不能因相同短预览而被视为同一次搜索。
		if call.Name == "search_text" {
			key = call.Name + "\x00" + string(artifact.RedactJSON(call.Arguments))
		}
		if call.Name == "read_file" {
			if read, ok := decodeCheckpointReadEvidence(result.Data); ok {
				entry.Target = read.Path
				entry.TotalBytes = read.TotalBytes
				entry.SHA256 = explorationReadSHA256(result.SHA256)
				entry.Ranges = []explorationByteRange{{Start: read.Offset, End: read.End}}
				key = fmt.Sprintf("read_file\x00%s\x00%s\x00%d", read.Path, entry.SHA256, read.TotalBytes)
				if entry.SHA256 == "" {
					key += fmt.Sprintf("\x00unversioned:%d", index)
				}
			}
		}
		if position, duplicate := positions[key]; duplicate {
			entries[position].Latest = index
			entries[position].Ranges = mergeExplorationReadRanges(append(entries[position].Ranges, entry.Ranges...))
			continue
		}
		positions[key] = len(entries)
		entries = append(entries, entry)
	}
	latest := 0
	for index := range entries {
		if entries[index].Latest > entries[latest].Latest {
			latest = index
		}
	}
	if latest > 0 {
		entry := entries[latest]
		copy(entries[1:latest+1], entries[:latest])
		entries[0] = entry
	}
	lines := make([]string, 0, len(entries))
	used := len(explorationInventoryHeader)
	const omitted = "- …\n"
	omittedEntries := false
	for _, entry := range entries {
		line := "- " + explorationInventoryLine(entry) + "\n"
		if used+len(line)+len(omitted) > maximumBytes {
			omittedEntries = true
			continue
		}
		lines = append(lines, line)
		used += len(line)
	}
	if omittedEntries && used+len(omitted) <= maximumBytes {
		lines = append(lines, omitted)
	}
	if len(lines) == 0 {
		return ""
	}
	return explorationInventoryHeader + strings.Join(lines, "")
}

// explorationReadSHA256 只接受完整的文件摘要，缺失或畸形摘要不能成为合并不同读取的依据。
func explorationReadSHA256(value string) string {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(value[len(prefix):]); err != nil {
		return ""
	}
	return strings.ToLower(value)
}

// mergeExplorationReadRanges 仅合并同一版本中重叠或相邻区间，不填补未读取的间隙。
func mergeExplorationReadRanges(ranges []explorationByteRange) []explorationByteRange {
	slices.SortFunc(ranges, func(left, right explorationByteRange) int {
		if left.Start < right.Start {
			return -1
		}
		if left.Start > right.Start {
			return 1
		}
		return 0
	})
	merged := ranges[:0]
	for _, interval := range ranges {
		if len(merged) == 0 || interval.Start > merged[len(merged)-1].End {
			merged = append(merged, interval)
		} else if interval.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = interval.End
		}
	}
	return merged
}

// explorationInventoryLine 格式化有限范围，缺元数据时明确未知，不将成功提议当作整文件读取。
func explorationInventoryLine(entry explorationInventoryEntry) string {
	line := entry.Name + " " + entry.Target
	if entry.Name != "read_file" {
		return line
	}
	line = "read_file " + fmt.Sprintf("%q", clipText(entry.Target, explorationTargetMaxBytes))
	if len(entry.Ranges) == 0 {
		return line + " range=unknown"
	}
	if entry.SHA256 != "" {
		line += " sha256=" + entry.SHA256
	}
	ranges := make([]string, 0, explorationReadRangeLimit)
	for _, interval := range entry.Ranges[:min(len(entry.Ranges), explorationReadRangeLimit)] {
		ranges = append(ranges, fmt.Sprintf("[%d,%d)", interval.Start, interval.End))
	}
	line += " byte_ranges=" + strings.Join(ranges, ",") + fmt.Sprintf(" total_bytes=%d", entry.TotalBytes)
	if omitted := len(entry.Ranges) - len(ranges); omitted > 0 {
		line += fmt.Sprintf(" omitted_ranges=%d", omitted)
	}
	return line
}

// toolExplorationTarget 保留搜索的完整条件；所有参数先整体脱敏，exec 无条件仅取 argv 首词。
func toolExplorationTarget(name string, arguments json.RawMessage) string {
	var fields struct {
		Path  string   `json:"path"`
		Query string   `json:"query"`
		Glob  string   `json:"glob"`
		Regex bool     `json:"regex"`
		Argv  []string `json:"argv"`
	}
	if len(arguments) == 0 || json.Unmarshal(artifact.RedactJSON(arguments), &fields) != nil {
		return ""
	}
	if name == "exec_command" {
		if len(fields.Argv) == 0 {
			return ""
		}
		return clipText(fields.Argv[0], explorationTargetMaxBytes)
	}
	if name == "search_text" && fields.Query != "" {
		parts := []string{}
		if fields.Path != "" {
			parts = append(parts, fmt.Sprintf("path=%q", clipText(fields.Path, explorationTargetMaxBytes)))
		}
		parts = append(parts, fmt.Sprintf("query=%q regex=%t", clipText(fields.Query, explorationTargetMaxBytes), fields.Regex))
		if fields.Glob != "" {
			parts = append(parts, fmt.Sprintf("glob=%q", clipText(fields.Glob, explorationTargetMaxBytes)))
		}
		return strings.Join(parts, " ")
	}
	if fields.Path != "" {
		return clipText(fields.Path, explorationTargetMaxBytes)
	}
	return ""
}
