// Package app 装配技能目录与显式本机管理用例，不连接 Provider 或执行技能代码。
// Author: fxbin
package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/capabilities/skills"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/runtime/lock"
	"github.com/fxbin/cairn/internal/runtime/tools"
	"github.com/fxbin/cairn/internal/runtime/userdir"
)

// SkillStoreFor 解析原生安装目录；工作区目标先经过 Workspace.Resolve 防止逃逸。
func (service Service) SkillStoreFor(workspace, scope string) (skills.SkillStore, error) {
	if scope == "user" {
		path, err := userdir.Path("skills")
		if err != nil {
			return skills.SkillStore{}, errors.New("resolve user skill directory failed")
		}
		return skills.NewSkillStore(path, openSkillPackageView, acquireSkillStoreLock)
	}
	if scope != "workspace" {
		return skills.SkillStore{}, errors.New("skill scope must be workspace or user")
	}
	view, err := tools.NewWorkspace(workspace, nil, 0, 0, 0)
	if err != nil {
		return skills.SkillStore{}, err
	}
	path, _, err := view.Resolve(filepath.Join(".agent", "skills"))
	if err != nil {
		return skills.SkillStore{}, errors.New("workspace skill directory escapes workspace")
	}
	return skills.NewSkillStore(path, openSkillPackageView, acquireSkillStoreLock)
}

// Skills 以项目原生、项目共享、用户原生、用户共享的固定优先级读取目录。
func (service Service) Skills(workspace string, shared bool) (*skills.SkillCatalog, error) {
	project, err := service.SkillStoreFor(workspace, "workspace")
	if err != nil {
		return nil, err
	}
	userRoot, err := userdir.Path("skills")
	if err != nil {
		return nil, err
	}
	roots := []skills.SkillRoot{{Path: project.Root, Scope: "workspace"}}
	if shared {
		view, err := tools.NewWorkspace(workspace, nil, 0, 0, 0)
		if err != nil {
			return nil, err
		}
		path, _, err := view.Resolve(filepath.Join(".agents", "skills"))
		if err != nil {
			return nil, errors.New("shared workspace skill directory escapes workspace")
		}
		roots = append(roots, skills.SkillRoot{Path: path, Scope: "workspace-shared"})
	}
	roots = append(roots, skills.SkillRoot{Path: userRoot, Scope: "user"})
	if shared {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("resolve shared user skill directory failed")
		}
		roots = append(roots, skills.SkillRoot{Path: filepath.Join(home, ".agents", "skills"), Scope: "user-shared", AllowLinkedPackages: true})
	}
	return skills.DiscoverSkills(roots, openSkillPackageView)
}

// configuredSkillTool 只在配置明确启用时装配内置技能工具，未启用不扫描用户目录。
func (service Service) configuredSkillTool(workspace string, cfg config.Config) (*skills.Skill, error) {
	if cfg.Skills == nil || !cfg.Skills.Enabled {
		return nil, nil
	}
	catalog, err := service.SkillsWithConfig(workspace, *cfg.Skills)
	if err != nil {
		return nil, err
	}
	return &skills.Skill{Catalog: catalog}, nil
}

// SkillsWithConfig 按配置装配技能根：原生/共享之外，可选 skillshare 默认源
// 与 extra_dirs（只读、允许符号链接包）。目录不存在时跳过，不报错。
func (service Service) SkillsWithConfig(workspace string, cfg config.SkillsConfig) (*skills.SkillCatalog, error) {
	roots := make([]skills.SkillRoot, 0, 6)
	project, err := service.SkillStoreFor(workspace, "workspace")
	if err != nil {
		return nil, err
	}
	roots = append(roots, skills.SkillRoot{Path: project.Root, Scope: "workspace"})
	if cfg.Shared {
		view, err := tools.NewWorkspace(workspace, nil, 0, 0, 0)
		if err != nil {
			return nil, err
		}
		path, _, err := view.Resolve(filepath.Join(".agents", "skills"))
		if err != nil {
			return nil, errors.New("shared workspace skill directory escapes workspace")
		}
		roots = append(roots, skills.SkillRoot{Path: path, Scope: "workspace-shared"})
	}
	userRoot, err := userdir.Path("skills")
	if err == nil {
		roots = append(roots, skills.SkillRoot{Path: userRoot, Scope: "user"})
	}
	if cfg.Shared {
		if home, err := os.UserHomeDir(); err == nil {
			roots = append(roots, skills.SkillRoot{
				Path: filepath.Join(home, ".agents", "skills"), Scope: "user-shared", AllowLinkedPackages: true,
			})
		}
	}
	if cfg.Skillshare {
		if path, err := skillshareSkillsDir(); err == nil {
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
				roots = append(roots, skills.SkillRoot{Path: path, Scope: "skillshare", AllowLinkedPackages: true})
			}
		}
	}
	for _, dir := range cfg.ExtraDirs {
		path := strings.TrimSpace(dir)
		if path == "" {
			continue
		}
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			roots = append(roots, skills.SkillRoot{Path: path, Scope: "external", AllowLinkedPackages: true})
		}
	}
	return skills.DiscoverSkills(roots, openSkillPackageView)
}

// skillshareSkillsDir 返回 skillshare 默认 skills 源目录。
func skillshareSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// macOS/Linux 约定；Windows 可用 extra_dirs 显式配置。
	return filepath.Join(home, ".config", "skillshare", "skills"), nil
}

// openSkillPackageView 复用工具工作区的规范路径、敏感项与有界读取，不扩大权限。
func openSkillPackageView(root string) (skills.PackageView, error) {
	return tools.NewWorkspace(root, nil, skills.SkillMaxResourceBytes, skills.SkillMaxFiles, skills.SkillMaxPackageBytes)
}

// acquireSkillStoreLock 将原生目录管理绑定到现有进程级排他锁。
func acquireSkillStoreLock(path string) (skills.StoreLock, error) {
	return lock.Acquire(path)
}
