// archive_preview.go 提供 ZIP 归档只读条目预览：列举文件名、大小与压缩比，
// 不解压内容、不跟随符号链接；用于对话中引用归档结构。
// Author: fxbin
package tools

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// ArchivePreviewMaxEntries 限制单次列举条目数。
const ArchivePreviewMaxEntries = 200

// ArchiveEntry 是归档内一个条目的摘要。
type ArchiveEntry struct {
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	CompressedSize int64  `json:"compressed_size"`
	IsDir          bool   `json:"is_dir"`
}

// ArchivePreviewResult 是归档预览结果。
type ArchivePreviewResult struct {
	Path      string         `json:"path"`
	Entries   []ArchiveEntry `json:"entries"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
}

// PreviewZipEntries 只读列举 ZIP 条目；非 zip 或越界路径失败关闭。
func PreviewZipEntries(root, relative string) (ArchivePreviewResult, error) {
	if strings.TrimSpace(relative) == "" {
		return ArchivePreviewResult{}, fmt.Errorf("relative path is required")
	}
	full := filepath.Join(root, relative)
	// 防越界
	if !strings.HasPrefix(filepath.Clean(full), filepath.Clean(root)+string(filepath.Separator)) && filepath.Clean(full) != filepath.Clean(root) {
		return ArchivePreviewResult{}, fmt.Errorf("path escapes workspace")
	}
	reader, err := zip.OpenReader(full)
	if err != nil {
		return ArchivePreviewResult{}, fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()
	result := ArchivePreviewResult{Path: relative, Entries: make([]ArchiveEntry, 0, len(reader.File))}
	for _, file := range reader.File {
		if len(result.Entries) >= ArchivePreviewMaxEntries {
			result.Truncated = true
			break
		}
		result.Entries = append(result.Entries, ArchiveEntry{
			Name:           file.Name,
			Size:           int64(file.UncompressedSize64),
			CompressedSize: int64(file.CompressedSize64),
			IsDir:          file.FileInfo().IsDir(),
		})
	}
	result.Total = len(reader.File)
	return result, nil
}

// EncodeArchivePreview 序列化预览结果。
func EncodeArchivePreview(result ArchivePreviewResult) (json.RawMessage, error) {
	return json.Marshal(result)
}
