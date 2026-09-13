//go:build darwin || linux

// 本文件实现 Provider 内容寻址存储在 Unix 平台上的目录持久化与 no-replace 发布。
//
// Author: fxbin
package provider

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// openContentAddressedStore 逐级创建存储路径并持久化每级目录项；仅最终存储根
// 目录强制使用 0700，避免改变调用方已有父目录的权限。
func openContentAddressedStore(root string) error {
	root = filepath.Clean(root)
	ancestor := root
	var missing []string
	for {
		info, err := os.Stat(ancestor)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("provider store path %s is not a directory", ancestor)
			}
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect provider store path %s: %w", ancestor, err)
		}
		missing = append(missing, ancestor)
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fmt.Errorf("provider store path has no existing ancestor: %s", root)
		}
		ancestor = parent
	}

	for index := len(missing) - 1; index >= 0; index-- {
		directory := missing[index]
		if err := os.Mkdir(directory, 0o700); err != nil {
			if !os.IsExist(err) {
				return fmt.Errorf("create provider store directory %s: %w", directory, err)
			}
			info, statErr := os.Stat(directory)
			if statErr != nil {
				return fmt.Errorf("inspect concurrently created provider store path %s: %w", directory, statErr)
			}
			if !info.IsDir() {
				return fmt.Errorf("provider store path %s is not a directory", directory)
			}
		}
		if err := syncProviderDirectory(filepath.Dir(directory)); err != nil {
			return fmt.Errorf("sync parent of provider store directory %s: %w", directory, err)
		}
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("set provider store root permissions: %w", err)
	}
	if err := syncProviderDirectory(filepath.Dir(root)); err != nil {
		return fmt.Errorf("sync parent of provider store root: %w", err)
	}
	if err := syncProviderDirectory(root); err != nil {
		return fmt.Errorf("sync provider store root: %w", err)
	}
	return nil
}

// appendContentAddressed 将完整内容以 0600 权限发布到内容寻址目录。硬链接提供
// no-replace 语义；所有成功返回均先同步目录，临时文件清理错误与主错误合并返回。
func appendContentAddressed(root, name string, content []byte, recordKind, fingerprint string) (err error) {
	target := filepath.Join(root, name)
	existing, readErr := os.ReadFile(target)
	switch {
	case readErr == nil:
		if !bytes.Equal(existing, content) {
			return fmt.Errorf("%s %s already exists with different content", recordKind, fingerprint)
		}
		if syncErr := syncProviderDirectory(root); syncErr != nil {
			return fmt.Errorf("sync %s store directory: %w", recordKind, syncErr)
		}
		return nil
	case !os.IsNotExist(readErr):
		return fmt.Errorf("read existing %s %s: %w", recordKind, fingerprint, readErr)
	}

	temporary, err := os.CreateTemp(root, ".append-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary %s %s: %w", recordKind, fingerprint, err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close temporary %s %s: %w", recordKind, fingerprint, closeErr))
			}
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, fmt.Errorf("remove temporary %s %s: %w", recordKind, fingerprint, removeErr))
		}
		if syncErr := syncProviderDirectory(root); syncErr != nil {
			err = errors.Join(err, fmt.Errorf("sync %s store directory after temporary cleanup: %w", recordKind, syncErr))
		}
	}()

	if chmodErr := temporary.Chmod(0o600); chmodErr != nil {
		return fmt.Errorf("set temporary %s %s permissions: %w", recordKind, fingerprint, chmodErr)
	}
	written, writeErr := temporary.Write(content)
	if writeErr != nil {
		return fmt.Errorf("write temporary %s %s: %w", recordKind, fingerprint, writeErr)
	}
	if written != len(content) {
		return fmt.Errorf("write temporary %s %s: incomplete write", recordKind, fingerprint)
	}
	if syncErr := temporary.Sync(); syncErr != nil {
		return fmt.Errorf("sync temporary %s %s: %w", recordKind, fingerprint, syncErr)
	}
	if closeErr := temporary.Close(); closeErr != nil {
		closed = true
		return fmt.Errorf("close temporary %s %s: %w", recordKind, fingerprint, closeErr)
	}
	closed = true

	if linkErr := os.Link(temporaryPath, target); linkErr != nil {
		if !os.IsExist(linkErr) {
			return fmt.Errorf("publish %s %s: %w", recordKind, fingerprint, linkErr)
		}
		existing, readErr = os.ReadFile(target)
		if readErr != nil {
			return fmt.Errorf("read concurrently published %s %s: %w", recordKind, fingerprint, readErr)
		}
		if !bytes.Equal(existing, content) {
			return fmt.Errorf("%s %s already exists with different content", recordKind, fingerprint)
		}
	}
	return nil
}

// syncProviderDirectory 同步目录元数据，并保留同步与关闭两个阶段的错误。
func syncProviderDirectory(path string) (err error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, directory.Close())
	}()
	if err := directory.Sync(); err != nil {
		return err
	}
	return nil
}
