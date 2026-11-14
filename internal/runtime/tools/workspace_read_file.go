// Package tools 为共享工作区视图提供有界普通文件读取。
// Author: fxbin
package tools

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ReadBoundedFile 经路径与忽略规则核验后，用根目录句柄读取有界普通文件。
// 保留读取前后的文件类型检查、最大值后一字节检查及既有诊断语义。
func (workspace Workspace) ReadBoundedFile(relative string, maximum int64) ([]byte, error) {
	root, err := os.OpenRoot(workspace.Root)
	if err != nil {
		return nil, errors.New("open skill root failed")
	}
	defer root.Close()
	_, resolved, err := workspace.Resolve(relative)
	if err != nil || workspace.Ignore(resolved) {
		return nil, errors.New("skill path is not allowed")
	}
	info, err := root.Lstat(filepath.FromSlash(resolved))
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("skill file is not a bounded regular file")
	}
	file, err := root.Open(filepath.FromSlash(resolved))
	if err != nil {
		return nil, errors.New("open skill file failed")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid skill file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(raw)) > maximum {
		return nil, errors.New("skill file exceeds limit")
	}
	return raw, nil
}
