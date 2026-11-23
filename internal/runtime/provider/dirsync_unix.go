//go:build darwin || linux

// dirsync_unix.go 在 Unix 平台同步目录元数据，保证内容寻址发布的持久化顺序。
//
// Author: fxbin
package provider

import (
	"errors"
	"os"
)

// syncProviderDirectory 同步目录元数据，并保留同步与关闭两个阶段的错误。
func syncProviderDirectory(path string) (err error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, directory.Close())
	}()
	return directory.Sync()
}
