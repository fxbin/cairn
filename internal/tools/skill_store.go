// Package tools 以显式本地操作安装和管理技能包，不执行安装钩子。
// Author: fxbin
package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fxbin/cairn/internal/lock"
)

// SkillStore 只管理 Cairn 原生技能目录；共享目录保持外部管理。
type SkillStore struct{ Root string }

// open 创建私有目录并取得进程级排他锁，防止并发管理操作覆盖彼此。
func (store SkillStore) open() (*os.Root, *lock.Lock, error) {
	if err := os.MkdirAll(store.Root, 0700); err != nil {
		return nil, nil, errors.New("create skill store failed")
	}
	info, err := os.Lstat(store.Root)
	if err != nil || !info.IsDir() {
		return nil, nil, errors.New("skill store must be a real directory")
	}
	view, err := NewWorkspace(store.Root, nil, SkillMaxResourceBytes, SkillMaxFiles, SkillMaxPackageBytes)
	if err != nil {
		return nil, nil, errors.New("open skill store failed")
	}
	lockPath, _, err := view.Resolve(".cairn-skills.lock")
	if err != nil {
		return nil, nil, errors.New("invalid skill store lock")
	}
	guard, err := lock.Acquire(lockPath)
	if err != nil {
		return nil, nil, errors.New("skill store is locked or unavailable")
	}
	root, err := os.OpenRoot(view.Root)
	if err != nil {
		return nil, nil, errors.Join(errors.New("open skill store failed"), guard.Release())
	}
	return root, guard, nil
}

// Install 将经过校验的普通文件复制到临时包，全部同步后原子发布；已有名称失败关闭。
func (store SkillStore) Install(source string) (name string, err error) {
	view, err := NewWorkspace(source, nil, SkillMaxResourceBytes, SkillMaxFiles, SkillMaxPackageBytes)
	if err != nil {
		return "", errors.New("open local skill source failed")
	}
	raw, err := skillReadFile(view, "SKILL.md", SkillMaxInstructionsBytes)
	if err != nil {
		return "", err
	}
	metadata, _, err := parseSkill(raw, filepath.Base(view.Root))
	if err != nil {
		return "", err
	}
	resources, err := skillResources(view)
	if err != nil {
		return "", err
	}
	root, guard, err := store.open()
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, root.Close(), guard.Release()) }()
	if _, err := root.Lstat(metadata.Name); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("skill destination already exists or cannot be inspected")
	}
	temporary, err := os.MkdirTemp(root.Name(), ".skill-install-")
	if err != nil {
		return "", errors.New("create temporary skill package failed")
	}
	temporaryName := filepath.Base(temporary)
	defer func() { err = errors.Join(err, root.RemoveAll(temporaryName)) }()
	target, err := root.OpenRoot(temporaryName)
	if err != nil {
		return "", errors.New("open temporary skill package failed")
	}
	for _, resource := range resources {
		content, readErr := skillReadFile(view, resource.Path, SkillMaxResourceBytes)
		if readErr != nil || skillDigest(content) != resource.SHA256 {
			_ = target.Close()
			return "", errors.New("skill source changed during installation")
		}
		if resource.Path == "SKILL.md" && skillDigest(content) != skillDigest(raw) {
			_ = target.Close()
			return "", errors.New("skill instructions changed during installation")
		}
		if err := target.MkdirAll(filepath.Dir(resource.Path), 0700); err != nil {
			_ = target.Close()
			return "", errors.New("create skill resource directory failed")
		}
		path, _, resolveErr := view.Resolve(resource.Path)
		if resolveErr != nil {
			_ = target.Close()
			return "", errors.New("resolve skill resource failed")
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			_ = target.Close()
			return "", errors.New("inspect skill resource failed")
		}
		mode := os.FileMode(0600)
		if info.Mode().Perm()&0111 != 0 {
			mode = 0700
		}
		file, writeErr := target.OpenFile(resource.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if writeErr != nil {
			_ = target.Close()
			return "", errors.New("create skill resource failed")
		}
		_, writeErr = file.Write(content)
		writeErr = errors.Join(writeErr, file.Sync(), file.Close())
		if writeErr != nil {
			_ = target.Close()
			return "", errors.New("write skill resource failed")
		}
	}
	if err := errors.Join(syncSkillTree(target), target.Close()); err != nil {
		return "", errors.New("close skill package failed")
	}
	if _, err := root.Lstat(metadata.Name); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("skill destination appeared during installation")
	}
	if err := root.Rename(temporaryName, metadata.Name); err != nil {
		return "", errors.New("publish skill package failed")
	}
	return metadata.Name, syncSkillStore(root)
}

// Remove 在排他锁内先移出公开名称再删除，只操作指定原生技能目录。
func (store SkillStore) Remove(name string) (err error) {
	if !ValidSkillName(name) {
		return errors.New("invalid skill name")
	}
	root, guard, err := store.open()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close(), guard.Release()) }()
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() {
		return errors.New("installed skill directory is unavailable")
	}
	temporary, err := os.MkdirTemp(root.Name(), ".skill-remove-")
	if err != nil {
		return errors.New("reserve skill removal failed")
	}
	trash := filepath.Base(temporary)
	if err := root.Remove(trash); err != nil {
		return errors.New("prepare skill removal failed")
	}
	if err := root.Rename(name, trash); err != nil {
		return errors.New("unpublish skill failed")
	}
	return errors.Join(root.RemoveAll(trash), syncSkillStore(root))
}

// SetEnabled 通过包内私有标记保存启停状态；变更从下一次装配生效。
func (store SkillStore) SetEnabled(name string, enabled bool) (err error) {
	if !ValidSkillName(name) {
		return errors.New("invalid skill name")
	}
	root, guard, err := store.open()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close(), guard.Release()) }()
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() {
		return errors.New("installed skill directory is unavailable")
	}
	if enabled {
		err = root.Remove(filepath.Join(name, skillDisabledFile))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.Join(err, syncSkillDirectory(root, name))
	}
	file, err := root.OpenFile(filepath.Join(name, skillDisabledFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return errors.New("disable skill failed")
	}
	return errors.Join(file.Sync(), file.Close(), syncSkillDirectory(root, name))
}

// syncSkillTree 在发布前由深到浅同步临时包目录，保证资源文件的目录项也完成持久化。
func syncSkillTree(root *os.Root) error {
	directories := []string{"."}
	if err := walkSkillPackage(root.FS(), func(path string, entry fs.DirEntry) error {
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncSkillDirectory(root, directories[index]); err != nil {
			return err
		}
	}
	return nil
}

// syncSkillDirectory 同步根目录句柄下的指定目录，不重新解析宿主机绝对路径。
func syncSkillDirectory(root *os.Root, path string) error {
	file, err := root.Open(path)
	if err != nil {
		return errors.New("open skill directory for sync failed")
	}
	return errors.Join(file.Sync(), file.Close())
}

// syncSkillStore 同步目录元数据，使已返回成功的发布具备持久化边界。
func syncSkillStore(root *os.Root) error {
	return syncSkillDirectory(root, ".")
}
