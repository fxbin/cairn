// Package tools 提供声明式内容包安装：清单只声明资源，安装不执行脚本或钩子。
// Author: fxbin
package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/lock"
	"gopkg.in/yaml.v3"
)

// 内容包限制约束单包体积、条目数与清单规模，避免无界安装。
const (
	PackageContentVersion   = 1
	PackageMaxNameChars     = 64
	PackageMaxVersionChars  = 32
	PackageMaxEntries       = 256
	PackageMaxFileBytes     = 1024 * 1024
	PackageMaxTotalBytes    = 8 * 1024 * 1024
	PackageManifestFile     = "package.yaml"
	PackageMaxManifestBytes = 64 * 1024
	packageDisabledFile     = ".cairn-disabled"
	packageLockName         = ".cairn-packages.lock"
)

// PackageResource 声明包内一个普通文件的相对路径与内容摘要。
type PackageResource struct {
	Path   string `json:"path" yaml:"path"`
	SHA256 string `json:"sha256" yaml:"sha256"`
}

// PackageManifest 是内容包清单；只声明资源、版本与摘要，不声明 hooks 或安装脚本。
type PackageManifest struct {
	Version   int               `json:"version" yaml:"version"`
	Name      string            `json:"name" yaml:"name"`
	Package   string            `json:"package" yaml:"package"`
	Resources []PackageResource `json:"resources" yaml:"resources"`
}

// PackageEntry 是已安装内容包的可展示投影。
type PackageEntry struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Disabled bool   `json:"disabled"`
	SHA256   string `json:"sha256"`
	Files    int    `json:"files"`
}

// PackageStore 管理本地内容包目录；共享来源保持外部管理。
type PackageStore struct {
	Root string
}

// NewPackageStore 打开指定包根目录。
func NewPackageStore(root string) (PackageStore, error) {
	if strings.TrimSpace(root) == "" {
		return PackageStore{}, errors.New("package store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return PackageStore{}, errors.New("resolve package store failed")
	}
	return PackageStore{Root: abs}, nil
}

// ParsePackageManifest 严格解析清单；拒绝路径穿越、重名、钩子与脚本字段。
func ParsePackageManifest(raw []byte) (PackageManifest, error) {
	var manifest PackageManifest
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return PackageManifest{}, errors.New("invalid package manifest")
	}
	if err := decoder.Decode(new(PackageManifest)); err != io.EOF {
		return PackageManifest{}, errors.New("package manifest must contain one document")
	}
	if manifest.Version != PackageContentVersion {
		return PackageManifest{}, errors.New("unsupported package manifest version")
	}
	if !ValidSkillName(manifest.Name) || len(manifest.Name) > PackageMaxNameChars {
		return PackageManifest{}, errors.New("invalid package name")
	}
	if strings.TrimSpace(manifest.Package) == "" || len(manifest.Package) > PackageMaxVersionChars {
		return PackageManifest{}, errors.New("invalid package version")
	}
	if len(manifest.Resources) == 0 || len(manifest.Resources) > PackageMaxEntries {
		return PackageManifest{}, errors.New("package resource count out of range")
	}
	seen := map[string]struct{}{}
	for _, resource := range manifest.Resources {
		if err := validatePackageResourcePath(resource.Path); err != nil {
			return PackageManifest{}, err
		}
		if _, exists := seen[resource.Path]; exists {
			return PackageManifest{}, errors.New("duplicate package resource path")
		}
		seen[resource.Path] = struct{}{}
		if !strings.HasPrefix(resource.SHA256, "sha256:") || len(resource.SHA256) != len("sha256:")+sha256.Size*2 {
			return PackageManifest{}, errors.New("invalid package resource digest")
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(resource.SHA256, "sha256:")); err != nil {
			return PackageManifest{}, errors.New("invalid package resource digest")
		}
	}
	sort.Slice(manifest.Resources, func(i, j int) bool {
		return manifest.Resources[i].Path < manifest.Resources[j].Path
	})
	return manifest, nil
}

// validatePackageResourcePath 拒绝路径穿越、隐藏文件、符号链接式写法与可执行扩展。
func validatePackageResourcePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("package resource path is required")
	}
	if path != filepath.Clean(path) || strings.HasPrefix(path, "..") || strings.HasPrefix(path, "/") {
		return errors.New("package resource path escapes package")
	}
	if strings.Contains(path, "\\") || strings.Contains(path, string(filepath.Separator)+"..") {
		return errors.New("package resource path escapes package")
	}
	base := filepath.Base(path)
	if base == packageDisabledFile || path == PackageManifestFile || strings.HasPrefix(base, ".") {
		return errors.New("package resource path is reserved")
	}
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, ".sh") || strings.HasSuffix(lower, ".exe") || strings.Contains(lower, "install") || strings.Contains(lower, "hook") {
		return errors.New("package must not declare executable or hook resources")
	}
	return nil
}

// InspectPackage 只读校验本地内容包：清单、摘要与资源存在性；不安装。
func InspectPackage(source string) (PackageManifest, error) {
	view, err := NewWorkspace(source, nil, PackageMaxFileBytes, PackageMaxEntries, PackageMaxTotalBytes)
	if err != nil {
		return PackageManifest{}, errors.New("open package source failed")
	}
	raw, err := skillReadFile(view, PackageManifestFile, PackageMaxManifestBytes)
	if err != nil {
		return PackageManifest{}, errors.New("read package manifest failed")
	}
	manifest, err := ParsePackageManifest(raw)
	if err != nil {
		return PackageManifest{}, err
	}
	totalBytes := 0
	for _, resource := range manifest.Resources {
		content, readErr := skillReadFile(view, resource.Path, PackageMaxFileBytes)
		if readErr != nil {
			return PackageManifest{}, fmt.Errorf("package resource %s unavailable", resource.Path)
		}
		totalBytes += len(content)
		if totalBytes > PackageMaxTotalBytes {
			return PackageManifest{}, errors.New("package total size exceeds limit")
		}
		if packageDigest(content) != normalizePackageDigest(resource.SHA256) {
			return PackageManifest{}, fmt.Errorf("package resource %s digest mismatch", resource.Path)
		}
	}
	return manifest, nil
}

// InstallPackage 复制经校验的普通文件到临时包并原子发布；失败不留半包。
func (store PackageStore) InstallPackage(source string) (entry PackageEntry, err error) {
	manifest, err := InspectPackage(source)
	if err != nil {
		return PackageEntry{}, err
	}
	view, err := NewWorkspace(source, nil, PackageMaxFileBytes, PackageMaxEntries, PackageMaxTotalBytes)
	if err != nil {
		return PackageEntry{}, errors.New("open package source failed")
	}
	if err := os.MkdirAll(store.Root, 0o700); err != nil {
		return PackageEntry{}, errors.New("create package store failed")
	}
	guard, err := lock.Acquire(filepath.Join(store.Root, packageLockName))
	if err != nil {
		return PackageEntry{}, errors.New("package store is locked or unavailable")
	}
	defer func() { err = errors.Join(err, guard.Release()) }()
	root, err := os.OpenRoot(store.Root)
	if err != nil {
		return PackageEntry{}, errors.New("open package store failed")
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if _, statErr := root.Lstat(manifest.Name); !errors.Is(statErr, os.ErrNotExist) {
		return PackageEntry{}, errors.New("package already installed or target unavailable")
	}
	temporary, err := os.MkdirTemp(root.Name(), ".pkg-install-")
	if err != nil {
		return PackageEntry{}, errors.New("create temporary package failed")
	}
	temporaryName := filepath.Base(temporary)
	defer func() { err = errors.Join(err, root.RemoveAll(temporaryName)) }()
	target, err := root.OpenRoot(temporaryName)
	if err != nil {
		return PackageEntry{}, errors.New("open temporary package failed")
	}
	defer func() { err = errors.Join(err, target.Close()) }()
	for _, resource := range manifest.Resources {
		content, readErr := skillReadFile(view, resource.Path, PackageMaxFileBytes)
		if readErr != nil || packageDigest(content) != normalizePackageDigest(resource.SHA256) {
			return PackageEntry{}, errors.New("package source changed during installation")
		}
		if err := writePackageResource(target, resource.Path, content); err != nil {
			return PackageEntry{}, err
		}
	}
	manifestRaw, err := yaml.Marshal(manifest)
	if err != nil {
		return PackageEntry{}, errors.New("encode package manifest failed")
	}
	if err := writePackageResource(target, PackageManifestFile, manifestRaw); err != nil {
		return PackageEntry{}, err
	}
	if err := syncSkillTree(target); err != nil {
		return PackageEntry{}, errors.New("sync package failed")
	}
	if _, statErr := root.Lstat(manifest.Name); !errors.Is(statErr, os.ErrNotExist) {
		return PackageEntry{}, errors.New("package destination appeared during installation")
	}
	if err := root.Rename(temporaryName, manifest.Name); err != nil {
		return PackageEntry{}, errors.New("publish package failed")
	}
	if err := syncSkillStore(root); err != nil {
		return PackageEntry{}, err
	}
	return PackageEntry{Name: manifest.Name, Version: manifest.Package, SHA256: packageDigest(manifestRaw), Files: len(manifest.Resources)}, nil
}

// writePackageResource 在临时包根句柄下独占创建并同步普通资源，不跟随预置链接。
func writePackageResource(root *os.Root, path string, content []byte) error {
	if err := root.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.New("create package resource directory failed")
	}
	file, err := root.OpenFile(filepath.FromSlash(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create package resource failed")
	}
	_, writeErr := file.Write(content)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return errors.New("write package resource failed")
	}
	return nil
}

// ListPackages 枚举已安装包；跳过无效目录，不阻断其余发现。
func (store PackageStore) ListPackages() ([]PackageEntry, error) {
	entries, err := os.ReadDir(store.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []PackageEntry{}, nil
		}
		return nil, errors.New("read package store failed")
	}
	view, err := NewWorkspace(store.Root, nil, PackageMaxManifestBytes, 0, 0)
	if err != nil {
		return nil, errors.New("open package store failed")
	}
	out := make([]PackageEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		raw, readErr := skillReadFile(view, filepath.Join(entry.Name(), PackageManifestFile), PackageMaxManifestBytes)
		if readErr != nil {
			continue
		}
		manifest, parseErr := ParsePackageManifest(raw)
		if parseErr != nil || manifest.Name != entry.Name() {
			continue
		}
		_, statErr := os.Lstat(filepath.Join(store.Root, entry.Name(), packageDisabledFile))
		out = append(out, PackageEntry{
			Name:     manifest.Name,
			Version:  manifest.Package,
			Disabled: statErr == nil,
			Files:    len(manifest.Resources),
			SHA256:   packageDigest(raw),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetPackageEnabled 通过包内私有标记保存启停；不删除资源、不改写清单。
func (store PackageStore) SetPackageEnabled(name string, enabled bool) error {
	if !ValidSkillName(name) {
		return errors.New("invalid package name")
	}
	lockPath := filepath.Join(store.Root, packageLockName)
	guard, err := lock.Acquire(lockPath)
	if err != nil {
		return errors.New("package store is locked or unavailable")
	}
	defer func() { _ = guard.Release() }()
	target := filepath.Join(store.Root, name)
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		return errors.New("installed package is unavailable")
	}
	marker := filepath.Join(target, packageDisabledFile)
	if enabled {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("enable package failed")
		}
		return nil
	}
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return errors.New("disable package failed")
	}
	return errors.Join(file.Sync(), file.Close(), syncDirectory(target))
}

// packageDigest 计算内容包清单与资源的稳定摘要。
func packageDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// normalizePackageDigest 归一化资源摘要的显示前缀。
func normalizePackageDigest(value string) string {
	if strings.HasPrefix(value, "sha256:") {
		return value
	}
	return "sha256:" + value
}

// PackageSummary 返回单行可读摘要。
func PackageSummary(entry PackageEntry) string {
	state := "enabled"
	if entry.Disabled {
		state = "disabled"
	}
	return fmt.Sprintf("%s  v%s  %s  files=%d  %s", entry.Name, entry.Version, state, entry.Files, entry.SHA256)
}
