// [INPUT]: 显式视图和锁的替身及真实受限工具。
// [OUTPUT]: 验证缺依赖关闭、来源与锁释放边界。
// [POS]: Skill 文件和装配端口回归。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package skills

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSkillCatalogInspectRetainsFactoryAndDisabledState 显式查看停用技能必须保留
// 工厂与来源绑定，不能启用原目录或把目录摘要替换为草稿内容摘要。
func TestSkillCatalogInspectRetainsFactoryAndDisabledState(t *testing.T) {
	root := t.TempDir()
	directory := writeTestSkill(t, root, "inspect-skill", "", "historical body")
	if err := os.WriteFile(filepath.Join(directory, skillDisabledFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	factory := func(root string) (PackageView, error) { calls++; return testPackageView(root) }
	catalog, err := DiscoverSkills([]SkillRoot{{Path: root, Scope: "workspace"}}, factory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Load("inspect-skill"); err == nil {
		t.Fatal("disabled skill loaded implicitly")
	}
	before := calls
	content, err := catalog.Inspect("inspect-skill")
	if err != nil || content.Instructions != "historical body" || calls <= before {
		t.Fatalf("inspect lost view factory: calls=%d content=%+v err=%v", calls, content, err)
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if content.SourceDigest != skillDigest([]byte(canonical)) || !catalog.Entries()[0].Disabled {
		t.Fatal("inspect changed source identity or original enabled state")
	}
}

// TestSkillPackageDependenciesFailClosed 缺少显式文件视图或原生锁不能自动发现
// 替代实现，也不能在没有排他锁的情况下发布已验证的技能包。
func TestSkillPackageDependenciesFailClosed(t *testing.T) {
	if _, err := DiscoverSkills(nil, nil); err == nil {
		t.Fatal("catalog accepted missing factory")
	}
	if _, err := NewSkillStore(t.TempDir(), nil, testStoreLock); err == nil {
		t.Fatal("store accepted missing factory")
	}
	if _, err := NewSkillStore(t.TempDir(), testPackageView, nil); err == nil {
		t.Fatal("store accepted missing lock acquirer")
	}
	root := t.TempDir()
	if _, err := DiscoverSkills([]SkillRoot{{Path: root}}, func(string) (PackageView, error) { return nil, nil }); err == nil {
		t.Fatal("catalog accepted nil package view")
	}
	sourceRoot := t.TempDir()
	source := writeTestSkill(t, sourceRoot, "locked-skill", "", "no execution")
	for _, acquire := range []StoreLockAcquirer{
		func(string) (StoreLock, error) { return nil, errors.New("lock unavailable") },
		func(string) (StoreLock, error) { return nil, nil },
	} {
		target := t.TempDir()
		store, err := NewSkillStore(target, testPackageView, acquire)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Install(source); err == nil || err.Error() != "skill store is locked or unavailable" {
			t.Fatalf("unlocked installation not rejected: %v", err)
		}
		if _, err := os.Stat(filepath.Join(target, "locked-skill")); !os.IsNotExist(err) {
			t.Fatalf("unlocked package published: %v", err)
		}
	}
}

// releaseRecordedLock 记录真实锁的释放，不替代原生互斥行为。
type releaseRecordedLock struct {
	StoreLock
	releases int
}

func (guard *releaseRecordedLock) Release() error {
	guard.releases++
	return guard.StoreLock.Release()
}

// TestSkillStoreReleasesInjectedLock 覆盖成功安装与重复发布失败时的锁释放；
// 同一目录的后续管理操作不能因迁移遗漏释放而永久阻塞。
func TestSkillStoreReleasesInjectedLock(t *testing.T) {
	source := writeTestSkill(t, t.TempDir(), "release-skill", "", "body")
	var locks []*releaseRecordedLock
	acquire := func(path string) (StoreLock, error) {
		real, err := testStoreLock(path)
		if err != nil {
			return nil, err
		}
		recorded := &releaseRecordedLock{StoreLock: real}
		locks = append(locks, recorded)
		return recorded, nil
	}
	store, err := NewSkillStore(t.TempDir(), testPackageView, acquire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Install(source); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Install(source); err == nil {
		t.Fatal("duplicate installation overwrote skill")
	}
	if len(locks) != 2 || locks[0].releases != 1 || locks[1].releases != 1 {
		t.Fatalf("lock releases=%+v", locks)
	}
	if err := store.Remove("release-skill"); err != nil {
		t.Fatal(err)
	}
	if len(locks) != 3 || locks[2].releases != 1 {
		t.Fatal("removal did not release the same injected lock")
	}
}
