// keysfile_test 覆盖本机凭据文件的装载与合并写入：权限失败关闭、行形态
// 校验、显式环境优先不覆盖、合并保留他行与行序、原子写权限收紧。测试使
// 用独立临时目录与独立变量名，不触碰真实环境约定。
//
// Author: fxbin
package credential

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKeysFile 以指定权限写入测试凭据文件。
func writeKeysFile(t *testing.T, path string, mode os.FileMode, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// TestLoadEnvFileMissingIsNoop 验证文件不存在时安静返回空装载。
func TestLoadEnvFileMissingIsNoop(t *testing.T) {
	loaded, err := LoadEnvFile(filepath.Join(t.TempDir(), KeysFileName))
	if err != nil || len(loaded) != 0 {
		t.Fatalf("缺失文件应为空装载，得到 loaded=%v err=%v", loaded, err)
	}
}

// TestLoadEnvFileRejectsLoosePermissions 验证权限过宽整文件拒绝装载且错误
// 文本不含任何值。
func TestLoadEnvFileRejectsLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeysFileName)
	writeKeysFile(t, path, 0o644, "TEST_KEY_A=secret-a\n")
	loaded, err := LoadEnvFile(path)
	if err == nil || loaded != nil {
		t.Fatalf("权限过宽应拒绝装载，得到 loaded=%v err=%v", loaded, err)
	}
	if strings.Contains(err.Error(), "secret-a") {
		t.Fatalf("错误文本不得包含凭据值: %v", err)
	}
}

// TestLoadEnvFileSkipsInvalidLines 验证非法行导致整文件拒绝装载。
func TestLoadEnvFileSkipsInvalidLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeysFileName)
	writeKeysFile(t, path, 0o600, "# 注释与空行应跳过\n\nVALID_NAME=ok\nlowercase=bad\n")
	_, err := LoadEnvFile(path)
	if err == nil {
		t.Fatal("非法变量名应整文件拒绝装载")
	}
}

// TestLoadEnvFileDoesNotOverrideExistingEnv 验证显式导出的环境变量优先，文
// 件值不覆盖。
func TestLoadEnvFileDoesNotOverrideExistingEnv(t *testing.T) {
	t.Setenv("CAIRN_KEYSFILE_TEST_A", "from-env")
	path := filepath.Join(t.TempDir(), KeysFileName)
	writeKeysFile(t, path, 0o600, "CAIRN_KEYSFILE_TEST_A=from-file\nCAIRN_KEYSFILE_TEST_B=from-file\n")
	loaded, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CAIRN_KEYSFILE_TEST_A") != "from-env" {
		t.Fatal("已导出的环境变量不得被文件覆盖")
	}
	if os.Getenv("CAIRN_KEYSFILE_TEST_B") != "from-file" {
		t.Fatal("缺失变量应从文件装载")
	}
	if len(loaded) != 1 || loaded[0] != "CAIRN_KEYSFILE_TEST_B" {
		t.Fatalf("装载清单应只含补缺变量，得到 %v", loaded)
	}
}

// TestUpsertEnvFileMergesAndPreservesOrder 验证合并写保留其他行与既有行
// 序、更新同名、追加新名，并保持 0600 权限。
func TestUpsertEnvFileMergesAndPreservesOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeysFileName)
	writeKeysFile(t, path, 0o600, "CAIRN_KEYSFILE_TEST_FIRST=1\nCAIRN_KEYSFILE_TEST_SECOND=2\n")
	if err := UpsertEnvFile(path, "CAIRN_KEYSFILE_TEST_SECOND", "two"); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEnvFile(path, "CAIRN_KEYSFILE_TEST_THIRD", "3"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "CAIRN_KEYSFILE_TEST_FIRST=1\nCAIRN_KEYSFILE_TEST_SECOND=two\nCAIRN_KEYSFILE_TEST_THIRD=3\n"
	if string(content) != want {
		t.Fatalf("合并写行序或内容不符:\n got %q\nwant %q", content, want)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("合并写后权限应保持 0600，得到 %v err=%v", info.Mode().Perm(), err)
	}
	if !EnvFileHas(path, "CAIRN_KEYSFILE_TEST_SECOND") || EnvFileHas(path, "ABSENT") {
		t.Fatal("EnvFileHas 存在性判断不符")
	}
}

// TestUpsertEnvFileValidatesInput 验证非法变量名与多行值被拒绝。
func TestUpsertEnvFileValidatesInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeysFileName)
	if err := UpsertEnvFile(path, "bad-name", "v"); err == nil {
		t.Fatal("非法变量名应拒绝")
	}
	if err := UpsertEnvFile(path, "GOOD_NAME", "line\ninjected"); err == nil {
		t.Fatal("多行值应拒绝")
	}
	if err := UpsertEnvFile(path, "GOOD_NAME", ""); err == nil {
		t.Fatal("空值应拒绝")
	}
}

// TestCredentialChainPrefersWorkspace 锁定 ADR-0016 两级装载链：工作区凭据
// 先装载、用户级按同名变量补缺，两层文件可同时生效于不同变量。
func TestCredentialChainPrefersWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := t.TempDir()
	workspaceKeys := filepath.Join(workspace, KeysFileName)
	writeKeysFile(t, workspaceKeys, 0o600, "CAIRN_CHAIN_TEST_SHARED=from-workspace\nCAIRN_CHAIN_TEST_ONLY_WS=ws\n")
	if _, err := LoadEnvFile(workspaceKeys); err != nil {
		t.Fatal(err)
	}
	userKeys, err := UserKeysPath()
	if err != nil {
		t.Fatal(err)
	}
	writeKeysFile(t, userKeys, 0o600, "CAIRN_CHAIN_TEST_SHARED=from-user\nCAIRN_CHAIN_TEST_ONLY_USER=user\n")
	if _, err := LoadEnvFile(userKeys); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CAIRN_CHAIN_TEST_SHARED"); got != "from-workspace" {
		t.Fatalf("同名变量工作区应优先，得到 %q", got)
	}
	if got := os.Getenv("CAIRN_CHAIN_TEST_ONLY_WS"); got != "ws" {
		t.Fatalf("工作区独有变量应装载，得到 %q", got)
	}
	if got := os.Getenv("CAIRN_CHAIN_TEST_ONLY_USER"); got != "user" {
		t.Fatalf("用户级独有变量应补缺装载，得到 %q", got)
	}
}
