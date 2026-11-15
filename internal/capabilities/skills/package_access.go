// [INPUT]: 共享合同、受限文件视图和目录锁。
// [OUTPUT]: 定义工厂与锁端口，并取得规范物理根。
// [POS]: Skill 能力基础设施边界，不依赖 Runtime。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package skills 声明技能包读取与管理所需的最小基础设施能力。
// Author: fxbin
package skills

import "errors"

// PackageView 保留共享工作区的路径、敏感性和普通文件容量边界，不执行包内代码。
// Resolve(".") 必须返回已规范化的物理根，读取须绑定该根并拒绝链接逃逸。
type PackageView interface {
	Resolve(string) (string, string, error)
	Ignore(string) bool
	ReadBoundedFile(string, int64) ([]byte, error)
}

// PackageViewFactory 为显式技能根构造受限视图；调用不授予新的访问权限。
type PackageViewFactory func(string) (PackageView, error)

// StoreLock 是原生技能目录管理的排他锁释放能力。
type StoreLock interface {
	Release() error
}

// StoreLockAcquirer 只为明确的原生目录锁路径取得互斥，获取失败必须关闭操作。
type StoreLockAcquirer func(string) (StoreLock, error)

// openPackageView 核对显式工厂并取得规范物理根，供来源摘要和目录句柄共用。
func openPackageView(factory PackageViewFactory, root string) (PackageView, string, error) {
	if factory == nil {
		return nil, "", errors.New("skill package view factory is required")
	}
	view, err := factory(root)
	if err != nil {
		return nil, "", err
	}
	if view == nil {
		return nil, "", errors.New("skill package view is required")
	}
	canonical, _, err := view.Resolve(".")
	if err != nil {
		return nil, "", err
	}
	return view, canonical, nil
}
