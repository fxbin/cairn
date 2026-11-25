//go:build darwin || linux

// sandbox_temp_test.go 锁定可写临时根的推导：固定根之外，只有当前进程实际使用的临时
// 目录（取规范路径）会被追加；无法解析的目录保持原样，过宽或无法核对的目录失败关闭。
// 只在有原生沙箱的平台上编译：其余平台上这些路径约定没有对应物。
// Author: fxbin
package tools

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fixedTempRootsForTest 是与 macOS 实际固定根一致的四个系统临时根。
var fixedTempRootsForTest = []string{"/private/tmp", "/tmp", "/var/folders", "/private/var/folders"}

// noWorkspace 是没有工作区的写入范围，用于与工作区无关的用例。
var noWorkspace = sandboxTempScope{}

// identityResolve 是不改变路径的解析函数，用于不需要符号链接语义的用例。
func identityResolve(path string) (string, error) { return path, nil }

// fixedSandboxTempInput 返回只含固定值、不读取真实环境的输入：临时目录与主目录取参数
// 给定的值，路径解析由 resolve 决定。
func fixedSandboxTempInput(tempDir, home string, resolve func(string) (string, error)) sandboxTempInput {
	return sandboxTempInput{
		TempDir:    func() string { return tempDir },
		HomeDir:    func() (string, error) { return home, nil },
		ResolveDir: resolve,
	}
}

// writableWorkspaceOf 返回以 root 为工作区、允许写工作区的范围。
func writableWorkspaceOf(root string) sandboxTempScope {
	return sandboxTempScope{WorkspaceRoot: root, WorkspaceWritable: true}
}

// TestSandboxTempRootsKeepsFixedRootsWhenTempDirIsInsideThem 锁定临时目录位于固定根
// 之下时不追加任何规则，默认的 macOS 环境因此与改动前完全一致。
func TestSandboxTempRootsKeepsFixedRootsWhenTempDirIsInsideThem(t *testing.T) {
	for _, tempDir := range []string{"", "/private/var/folders/ab/T/", "/tmp/cache"} {
		roots, err := sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput(tempDir, "/Users/example", identityResolve), noWorkspace)
		if err != nil {
			t.Fatalf("temp dir %q: %v", tempDir, err)
		}
		if !reflect.DeepEqual(roots, fixedTempRootsForTest) {
			t.Fatalf("temp dir %q 不应追加临时根: %v", tempDir, roots)
		}
	}
}

// TestSandboxTempRootsKeepsFixedRootsWhenSymlinkedTempDirResolvesInsideThem 锁定经符号
// 链接拼写的默认临时目录在解析后落回固定根，不追加重复规则。
func TestSandboxTempRootsKeepsFixedRootsWhenSymlinkedTempDirResolvesInsideThem(t *testing.T) {
	input := fixedSandboxTempInput("/var/folders/ab/T", "/Users/example", func(path string) (string, error) {
		return strings.Replace(path, "/var/", "/private/var/", 1), nil
	})
	roots, err := sandboxTempRoots(fixedTempRootsForTest, input, noWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roots, fixedTempRootsForTest) {
		t.Fatalf("解析后落在固定根之下的临时目录不应追加规则: %v", roots)
	}
}

// TestSandboxTempRootsAppendsTempDirOutsideFixedRoots 锁定固定根之外的专用临时目录被追加，
// 与有没有工作区无关。
func TestSandboxTempRootsAppendsTempDirOutsideFixedRoots(t *testing.T) {
	for _, scope := range []sandboxTempScope{noWorkspace, writableWorkspaceOf("/Users/example/projects/ws")} {
		roots, err := sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput("/Users/example/tmp", "/Users/example", identityResolve), scope)
		if err != nil {
			t.Fatal(err)
		}
		want := append(append([]string(nil), fixedTempRootsForTest...), "/Users/example/tmp")
		if !reflect.DeepEqual(roots, want) {
			t.Fatalf("roots = %v, want %v", roots, want)
		}
	}
}

// TestSandboxTempRootsUsesCanonicalPathOfSymlinkedTempDir 锁定追加的是规范路径：内核按
// 解析符号链接之后的路径匹配写规则。
func TestSandboxTempRootsUsesCanonicalPathOfSymlinkedTempDir(t *testing.T) {
	input := fixedSandboxTempInput("/var/custom-tmp", "/Users/example", func(path string) (string, error) {
		return strings.Replace(path, "/var/", "/private/var/", 1), nil
	})
	roots, err := sandboxTempRoots(fixedTempRootsForTest, input, noWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != len(fixedTempRootsForTest)+1 || roots[len(roots)-1] != "/private/var/custom-tmp" {
		t.Fatalf("应追加规范路径: %v", roots)
	}
}

// TestSandboxTempRootsDoesNotTouchFixedRoots 锁定传入的切片与其底层数组不被改写。
func TestSandboxTempRootsDoesNotTouchFixedRoots(t *testing.T) {
	backing := make([]string, 8)
	backing[0] = "/tmp"
	fixed := backing[0:1:4]
	if _, err := sandboxTempRoots(fixed, fixedSandboxTempInput("/Users/example/tmp", "/Users/example", identityResolve), noWorkspace); err != nil {
		t.Fatal(err)
	}
	if len(fixed) != 1 || fixed[0] != "/tmp" || backing[1] != "" {
		t.Fatalf("固定根被改写: fixed=%v backing=%v", fixed, backing)
	}
}

// TestSandboxTempRootsKeepsFixedRootsWhenTempDirCannotBeResolved 锁定无法解析为绝对目录
// 的临时目录不追加规则也不报错：沙箱无从放行一个无法确认的位置，行为与此前一致。
func TestSandboxTempRootsKeepsFixedRootsWhenTempDirCannotBeResolved(t *testing.T) {
	for _, tempDir := range []string{"tmp", "/Users/example/missing", "/Users/example/a-file"} {
		input := fixedSandboxTempInput(tempDir, "/Users/example", func(string) (string, error) {
			return "", errors.New("unresolved")
		})
		roots, err := sandboxTempRoots(fixedTempRootsForTest, input, noWorkspace)
		if err != nil {
			t.Fatalf("临时目录 %q 无法解析，不应报错: %v", tempDir, err)
		}
		if !reflect.DeepEqual(roots, fixedTempRootsForTest) {
			t.Fatalf("临时目录 %q 无法解析，不应追加规则: %v", tempDir, roots)
		}
	}
}

// TestSandboxTempRootsRejectsTooBroadTempDir 锁定过宽的临时目录失败关闭，并说明原因。
func TestSandboxTempRootsRejectsTooBroadTempDir(t *testing.T) {
	cases := []struct {
		name    string
		tempDir string
		want    string
	}{
		{name: "文件系统根", tempDir: "/", want: "filesystem root"},
		{name: "吞掉固定根", tempDir: "/private", want: "fixed temp root /private/tmp"},
		{name: "吞掉另一个固定根", tempDir: "/private/var", want: "fixed temp root /private/var/folders"},
		{name: "主目录", tempDir: "/Users/example", want: "user home directory"},
		{name: "主目录的上级", tempDir: "/Users", want: "user home directory"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			roots, err := sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput(testCase.tempDir, "/Users/example", identityResolve), writableWorkspaceOf("/srv/projects/ws"))
			if err == nil {
				t.Fatalf("临时目录 %q 应失败关闭，得到 %v", testCase.tempDir, roots)
			}
			if !strings.Contains(err.Error(), "too broad") || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("错误应说明过宽及原因 %q: %v", testCase.want, err)
			}
			if roots != nil {
				t.Fatalf("失败时不应返回任何临时根: %v", roots)
			}
		})
	}
}

// TestSandboxTempRootsGovernsCoveringTempDirByWriteMode 锁定覆盖工作区的临时目录按写入档位
// 区分：可写档下由临时写规则管辖，照常追加，工作区位于系统临时根之下时本来就是这样；
// 只读档下必须拒绝，否则工作区会经临时目录规则变得可写。
func TestSandboxTempRootsGovernsCoveringTempDirByWriteMode(t *testing.T) {
	const workspace = "/Users/example/projects/ws"
	for _, tempDir := range []string{"/Users/example/projects", workspace} {
		roots, err := sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput(tempDir, "/Users/me", identityResolve), writableWorkspaceOf(workspace))
		if err != nil || len(roots) != len(fixedTempRootsForTest)+1 || roots[len(roots)-1] != tempDir {
			t.Fatalf("可写档下覆盖工作区的临时目录 %q 应被追加: roots=%v err=%v", tempDir, roots, err)
		}
		roots, err = sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput(tempDir, "/Users/me", identityResolve), sandboxTempScope{WorkspaceRoot: workspace})
		if err == nil || !strings.Contains(err.Error(), "covers the workspace") || roots != nil {
			t.Fatalf("只读档下覆盖工作区的临时目录 %q 应失败关闭: roots=%v err=%v", tempDir, roots, err)
		}
	}
}

// TestSandboxTempRootsAllowsDedicatedDirInsideHome 锁定主目录之下的专用目录可以放行：
// 防护只拦住主目录本身及其上级。
func TestSandboxTempRootsAllowsDedicatedDirInsideHome(t *testing.T) {
	roots, err := sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput("/Users/example/Library/Caches/tmp", "/Users/example", identityResolve), noWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if roots[len(roots)-1] != "/Users/example/Library/Caches/tmp" {
		t.Fatalf("主目录之下的专用目录应被追加: %v", roots)
	}
}

// TestSandboxTempRootsGuardsHomeByCanonicalPath 锁定主目录防护按规范路径比较，主目录经
// 符号链接拼写出入时结论不变。
func TestSandboxTempRootsGuardsHomeByCanonicalPath(t *testing.T) {
	input := fixedSandboxTempInput("/Users", "/var/users/example", func(path string) (string, error) {
		return strings.Replace(path, "/var/users/", "/Users/", 1), nil
	})
	if _, err := sandboxTempRoots(fixedTempRootsForTest, input, noWorkspace); err == nil || !strings.Contains(err.Error(), "user home directory") {
		t.Fatalf("经符号链接拼写的主目录上级应失败关闭: %v", err)
	}
}

// TestSandboxTempRootsFailsClosedWhenHomeCannotBeChecked 锁定无法核对主目录时不放行额外的
// 临时根：取不到、为空、不是绝对路径或就是文件系统根都算无法核对，错误要提示该补什么。
func TestSandboxTempRootsFailsClosedWhenHomeCannotBeChecked(t *testing.T) {
	cases := []struct {
		name string
		home string
		err  error
	}{
		{name: "取不到", err: errors.New("home unavailable")},
		{name: "为空"},
		{name: "相对路径", home: "relative/home"},
		{name: "文件系统根", home: "/"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := sandboxTempInput{
				TempDir:    func() string { return "/srv/tmp" },
				HomeDir:    func() (string, error) { return testCase.home, testCase.err },
				ResolveDir: identityResolve,
			}
			roots, err := sandboxTempRoots(fixedTempRootsForTest, input, noWorkspace)
			if err == nil || !strings.Contains(err.Error(), "cannot be granted") || !strings.Contains(err.Error(), "set HOME") || roots != nil {
				t.Fatalf("无法核对主目录时应失败关闭并提示设置 HOME: roots=%v err=%v", roots, err)
			}
		})
	}
}

// TestSandboxTempRootsIgnoresCaseOnCaseInsensitiveFilesystems 锁定不区分大小写的平台上，
// 换个大小写拼写的同一目录不能绕过防护。
func TestSandboxTempRootsIgnoresCaseOnCaseInsensitiveFilesystems(t *testing.T) {
	if !sandboxCaseInsensitive {
		t.Skip("本平台文件系统区分大小写")
	}
	if _, err := sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput("/Users/EXAMPLE", "/Users/example", identityResolve), noWorkspace); err == nil || !strings.Contains(err.Error(), "user home directory") {
		t.Fatalf("换个大小写的主目录应失败关闭: %v", err)
	}
	if _, err := sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput("/Users/Example/projects", "/Users/me", identityResolve), sandboxTempScope{WorkspaceRoot: "/Users/example/projects/ws"}); err == nil || !strings.Contains(err.Error(), "covers the workspace") {
		t.Fatalf("换个大小写的、覆盖工作区的临时目录在只读档下应失败关闭: %v", err)
	}
}

// TestSandboxTempRootsNeedsNoRuleUnderSandboxProvidedRoots 锁定落在沙箱自己提供的可写路径
// 之下的临时目录不追加任何规则，也不做过宽与主目录核对：这里没有新增可写位置，容器里
// 常见的“TMPDIR 指向 /dev/shm、HOME 缺失”不应因此被拒。
func TestSandboxTempRootsNeedsNoRuleUnderSandboxProvidedRoots(t *testing.T) {
	provided := sandboxTempScope{ProvidedRoots: []string{"/dev"}}
	noHome := sandboxTempInput{TempDir: func() string { return "/dev/shm" }, HomeDir: func() (string, error) {
		return "", errors.New("home unavailable")
	}, ResolveDir: identityResolve}
	roots, err := sandboxTempRoots(fixedTempRootsForTest, noHome, provided)
	if err != nil || !reflect.DeepEqual(roots, fixedTempRootsForTest) {
		t.Fatalf("沙箱已提供的位置不应触发主目录核对: roots=%v err=%v", roots, err)
	}
	readOnlyCovered := sandboxTempScope{WorkspaceRoot: "/dev/shm/ws", ProvidedRoots: []string{"/dev"}}
	roots, err = sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput("/dev/shm", "/Users/me", identityResolve), readOnlyCovered)
	if err != nil || !reflect.DeepEqual(roots, fixedTempRootsForTest) {
		t.Fatalf("沙箱已提供的位置不应触发只读档核对: roots=%v err=%v", roots, err)
	}
	roots, err = sandboxTempRoots(fixedTempRootsForTest, fixedSandboxTempInput("/srv/tmp", "/Users/me", identityResolve), provided)
	if err != nil || roots[len(roots)-1] != "/srv/tmp" {
		t.Fatalf("沙箱已提供路径之外的临时目录照常追加: roots=%v err=%v", roots, err)
	}
}

// TestPathWithinScope 锁定路径包含判断的边界：相等、文件系统根与形似前缀。
func TestPathWithinScope(t *testing.T) {
	if !pathWithin("/a/b", "/a/b") || !pathWithin("/a/b/c", "/a/b") || !pathWithin("/a", "/") {
		t.Fatal("相等、子路径与文件系统根应判为包含")
	}
	if pathWithin("/a/bc", "/a/b") || pathWithin("/a", "/a/b") {
		t.Fatal("形似前缀与反向包含不得判为包含")
	}
}

// TestSandboxTempScopeOfResolvesWorkspaceRoot 锁定写入范围的整理：空工作区得到空范围，
// 解析不了的工作区按原样比较。
func TestSandboxTempScopeOfResolvesWorkspaceRoot(t *testing.T) {
	if scope := sandboxTempScopeOf("  ", true, fixedSandboxTempInput("", "", identityResolve)); scope.WorkspaceRoot != "" || scope.WorkspaceWritable {
		t.Fatalf("空工作区应得到空范围: %+v", scope)
	}
	scope := sandboxTempScopeOf("/Users/example/missing/ws", false, fixedSandboxTempInput("", "", func(string) (string, error) {
		return "", errors.New("unresolved")
	}))
	if scope.WorkspaceRoot != "/Users/example/missing/ws" || scope.WorkspaceWritable {
		t.Fatalf("解析不了的工作区应按原样比较: %+v", scope)
	}
}

// TestResolveSandboxDirAndHomeUseRealEnvironment 锁定真实实现读取真实的文件系统：现存目录
// 得到规范路径，缺失路径与普通文件报错；主目录要么是绝对路径，要么带错误返回。
func TestResolveSandboxDirAndHomeUseRealEnvironment(t *testing.T) {
	root := t.TempDir()
	canonical, err := resolveSandboxDir(root)
	if err != nil || !filepath.IsAbs(canonical) {
		t.Fatalf("现存目录应解析为规范路径: %q %v", canonical, err)
	}
	if _, err := resolveSandboxDir(filepath.Join(root, "missing")); err == nil {
		t.Fatal("缺失路径应报错")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSandboxDir(file); err == nil {
		t.Fatal("普通文件应报错")
	}
	if home, err := currentUserHomeDir(); err == nil && home == "" {
		t.Fatal("没有报错时主目录不应为空")
	}

	if runtime.GOOS == "darwin" && !sandboxCaseInsensitive {
		t.Fatal("macOS 的路径比较应不区分大小写")
	}
}
