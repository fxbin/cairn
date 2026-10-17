// Package userdir 统一当前用户的 Cairn 根目录。
// Author: fxbin
package userdir

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrUnavailable 表示无法确定当前用户主目录，调用方可按自身合同跳过可选用户来源。
var ErrUnavailable = errors.New("current user home directory is unavailable")

// Root 返回跨平台一致的 ~/.cairn；只计算路径，不创建或迁移用户数据。
func Root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", ErrUnavailable
	}
	return filepath.Join(home, ".cairn"), nil
}

// Path 返回当前用户资源的统一读写位置，只接收代码内定义的安全相对名称。
func Path(relative string) (string, error) {
	if !filepath.IsLocal(relative) || relative == "." || filepath.Clean(relative) != relative {
		return "", errors.New("invalid user resource name")
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, relative), nil
}
