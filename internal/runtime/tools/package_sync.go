// [INPUT]: 临时包根句柄与有界目录遍历。
// [OUTPUT]: 由深到浅同步目录并保留错误证据。
// [POS]: 内容包发布的持久化边界。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package tools 保持内容包发布前的根句柄同步与有界目录遍历。
// Author: fxbin
package tools

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// packageMaxTreeEntries 保留原共享同步器的目录项预算，包含目录和普通文件。
const packageMaxTreeEntries = 1024

// syncPackageTree 在发布前由深到浅同步临时包目录，保证资源文件的目录项也完成持久化。
func syncPackageTree(root *os.Root) error {
	directories := []string{"."}
	if err := walkPackageTree(root.FS(), func(path string, entry fs.DirEntry) error {
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncPackageDirectory(root, directories[index]); err != nil {
			return err
		}
	}
	return nil
}

// syncPackageDirectory 同步根目录句柄下的指定目录，不重新解析宿主机绝对路径。
func syncPackageDirectory(root *os.Root, path string) error {
	file, err := root.Open(path)
	if err != nil {
		return errors.New("open skill directory for sync failed")
	}
	return errors.Join(file.Sync(), file.Close())
}

// walkPackageTree 在读取目录项时就限制剩余额度，拒绝链接和特殊文件；调用者必须传入受限根的 FS。
func walkPackageTree(filesystem fs.FS, visit func(string, fs.DirEntry) error) error {
	visited := 0
	var walk func(string) error
	walk = func(directory string) error {
		file, err := filesystem.Open(directory)
		if err != nil {
			return errors.New("open skill package directory failed")
		}
		reader, ok := file.(fs.ReadDirFile)
		if !ok {
			_ = file.Close()
			return errors.New("skill package entry is not a directory")
		}
		remaining := packageMaxTreeEntries - visited
		entries, readErr := reader.ReadDir(remaining + 1)
		closeErr := file.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
			return errors.New("read skill package directory failed")
		}
		if len(entries) > remaining {
			return errors.New("skill package has too many entries")
		}
		visited += len(entries)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
				return errors.New("skill package links and special files are not supported")
			}
			path := filepath.ToSlash(filepath.Join(directory, entry.Name()))
			if err := visit(path, entry); err != nil {
				return err
			}
			if entry.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(".")
}
