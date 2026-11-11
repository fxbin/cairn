package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// skillDirectoryReadRecorder 记录真实遍历发给目录句柄的读取额度，防止回调上限掩盖无限读取。
type skillDirectoryReadRecorder struct {
	fs.FS
	requests []int
}

// skillRecordedDirectory 在目录文件接口上观察 ReadDir 的实际参数。
type skillRecordedDirectory struct {
	fs.File
	reader   fs.ReadDirFile
	recorder *skillDirectoryReadRecorder
}

// Open 保留底层文件行为，只包装支持目录读取的文件。
func (recorder *skillDirectoryReadRecorder) Open(name string) (fs.File, error) {
	file, err := recorder.FS.Open(name)
	if err != nil {
		return nil, err
	}
	reader, ok := file.(fs.ReadDirFile)
	if !ok {
		return file, nil
	}
	return &skillRecordedDirectory{File: file, reader: reader, recorder: recorder}, nil
}

// ReadDir 保存读取请求，再委托原目录实现，测试不替遍历器实施限额。
func (directory *skillRecordedDirectory) ReadDir(count int) ([]fs.DirEntry, error) {
	directory.recorder.requests = append(directory.recorder.requests, count)
	return directory.reader.ReadDir(count)
}

// TestSkillPackageWalkBoundsReadsBeforeCallbacks 同时约束根目录和跨嵌套目录的读取，禁止 ReadDir(-1)。
func TestSkillPackageWalkBoundsReadsBeforeCallbacks(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(strconv.FormatBool(nested), func(t *testing.T) {
			files := fstest.MapFS{}
			for index := 0; index < SkillMaxPackageEntries+8; index++ {
				name := "entry-" + strconv.Itoa(index)
				if nested {
					if index < SkillMaxPackageEntries/2+4 {
						name = "a/" + name
					} else {
						name = "b/" + name
					}
				}
				files[name] = &fstest.MapFile{Data: nil, Mode: 0600}
			}
			recorder := &skillDirectoryReadRecorder{FS: files}
			callbacks := 0
			err := walkSkillPackage(recorder, func(string, fs.DirEntry) error { callbacks++; return nil })
			if err == nil {
				t.Fatal("oversized package walk succeeded")
			}
			for _, count := range recorder.requests {
				if count <= 0 || count > SkillMaxPackageEntries+1 {
					t.Fatalf("unbounded directory read: %d", count)
				}
			}
			if !nested && callbacks != 0 {
				t.Fatalf("root limit applied after %d callbacks", callbacks)
			}
			if nested && (len(recorder.requests) < 3 || recorder.requests[len(recorder.requests)-1] >= recorder.requests[0]) {
				t.Fatal("nested directories did not consume the shared entry budget")
			}
		})
	}
}

// writeTestSkill 建立独立技能包，不读取用户已有目录。
func writeTestSkill(t *testing.T, root, name, extra, body string) string {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: 用于测试技能按需加载\n"+extra+"---\n"+body), 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// TestSkillFrontmatterValidation 验证可移植格式、扩展兼容及拒绝无效/危险输入。
func TestSkillFrontmatterValidation(t *testing.T) {
	valid := "---\nname: review-code\ndescription: >\n  检查代码。\n  在审查时使用。\nallowed-tools: [Bash, Read]\ncontext: fork\ndisable-model-invocation: true\nmetadata:\n  author: someone\n---\n先核对证据。"
	metadata, body, err := parseSkill([]byte(valid), "review-code")
	if err != nil || !metadata.ManualOnly || body != "先核对证据。" || metadata.Description != "检查代码。 在审查时使用。" {
		t.Fatalf("%#v %q %v", metadata, body, err)
	}
	for _, raw := range []string{
		strings.Replace(valid, "name: review-code", "name: ../escape", 1),
		strings.Replace(valid, "name: review-code", "name: review--code", 1),
		strings.Replace(valid, "name: review-code", "name: other", 1),
		"---\nname: review-code\ndescription: 123\n---\nbody",
		"---\nname: review-code\n---\nbody",
		"---\nname: review-code\ndescription: short\n---\n",
		strings.Replace(valid, "true", "maybe", 1),
		valid + "\x1b[2J",
		strings.Repeat("x", SkillMaxInstructionsBytes+1),
	} {
		if _, _, err := parseSkill([]byte(raw), "review-code"); err == nil {
			t.Fatalf("invalid skill accepted: %.80q", raw)
		}
	}
}

// TestSkillCatalogPrecedenceAndDisabledShadow 项目技能和停用标记不能泄漏同名低优先级回退。
func TestSkillCatalogPrecedenceAndDisabledShadow(t *testing.T) {
	project, user := t.TempDir(), t.TempDir()
	writeTestSkill(t, user, "review-code", "", "user body")
	directory := writeTestSkill(t, project, "review-code", "", "project body")
	if err := os.WriteFile(filepath.Join(directory, skillDisabledFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := DiscoverSkills([]SkillRoot{{Path: project, Scope: "workspace"}, {Path: user, Scope: "user"}})
	if err != nil {
		t.Fatal(err)
	}
	if entries := catalog.Entries(); len(entries) != 1 || !entries[0].Disabled || entries[0].Scope != "workspace" {
		t.Fatalf("%#v", entries)
	}
	if len(catalog.Diagnostics) != 1 || catalog.Diagnostics[0].Code != "shadowed" {
		t.Fatalf("%#v", catalog.Diagnostics)
	}
	if _, err := catalog.Load("review-code"); err == nil {
		t.Fatal("disabled skill loaded")
	}
	content, err := catalog.Inspect("review-code")
	if err != nil || content.Instructions != "project body" {
		t.Fatalf("%#v %v", content, err)
	}
}

// TestSkillInstallLifecycleAndNeverExecute 安装按字节复制脚本并保留可执行位，整个管理过程不运行它。
func TestSkillInstallLifecycleAndNeverExecute(t *testing.T) {
	sourceRoot, targetRoot := t.TempDir(), t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	source := writeTestSkill(t, sourceRoot, "review-code", "allowed-tools: Bash(*)\n", "检查当前任务。")
	if err := os.Mkdir(filepath.Join(source, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(source, "scripts", "danger.sh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	store := SkillStore{Root: targetRoot}
	if name, err := store.Install(source); err != nil || name != "review-code" {
		t.Fatalf("%s %v", name, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("installation executed a script")
	}
	if _, err := store.Install(source); err == nil {
		t.Fatal("installation overwrote an existing skill")
	}
	installed := filepath.Join(targetRoot, "review-code", "scripts", "danger.sh")
	content, err := os.ReadFile(installed)
	if err != nil || string(content) != script {
		t.Fatalf("%q %v", content, err)
	}
	info, err := os.Stat(installed)
	if err != nil || info.Mode().Perm()&0100 == 0 {
		t.Fatal("executable bit was lost")
	}
	for _, enabled := range []bool{false, true} {
		if err := store.SetEnabled("review-code", enabled); err != nil {
			t.Fatal(err)
		}
		catalog, err := DiscoverSkills([]SkillRoot{{Path: targetRoot, Scope: "workspace"}})
		if err != nil || catalog.Entries()[0].Disabled == enabled {
			t.Fatalf("enabled=%t err=%v", enabled, err)
		}
	}
	if err := store.Remove("review-code"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "review-code")); !os.IsNotExist(err) {
		t.Fatal("skill was not removed")
	}
	if err := store.Remove("../outside"); err == nil {
		t.Fatal("unsafe removal accepted")
	}
}

// TestSkillRejectsEscapingLinksAndSensitivePackages 使用真实文件系统验证源码和安装边界。
func TestSkillRejectsEscapingLinksAndSensitivePackages(t *testing.T) {
	for _, kind := range []string{"symlink", "sensitive"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			directory := writeTestSkill(t, root, "review-code", "", "body")
			if kind == "symlink" {
				outside := filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(directory, "escape.txt")); err != nil {
					t.Skip(err)
				}
			} else if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("sensitive"), 0600); err != nil {
				t.Fatal(err)
			}
			store := SkillStore{Root: filepath.Join(t.TempDir(), "skills")}
			if _, err := store.Install(directory); err == nil {
				t.Fatal("unsafe package installed")
			}
			if _, err := os.Stat(filepath.Join(store.Root, "review-code")); !os.IsNotExist(err) {
				t.Fatal("partial public package remains")
			}
		})
	}
}

// TestSkillActivationReceiptsManualVisibilityAndDrift 只有成功接纳后才可读资源，重复加载沿用历史正文。
func TestSkillActivationReceiptsManualVisibilityAndDrift(t *testing.T) {
	root := t.TempDir()
	directory := writeTestSkill(t, root, "review-code", "disable-model-invocation: true\n", "ORIGINAL_SKILL_BODY")
	if err := os.WriteFile(filepath.Join(directory, "guide.txt"), []byte("original guide"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := DiscoverSkills([]SkillRoot{{Path: root, Scope: "workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	skill := &Skill{Catalog: catalog}
	registry, err := NewRegistry(skill)
	if err != nil {
		t.Fatal(err)
	}
	load := json.RawMessage(`{"name":"review-code"}`)
	if strings.Contains(skill.Spec().Description, "review-code") || skill.Execute(context.Background(), load).OK {
		t.Fatal("manual skill automatically visible")
	}
	registry.PrepareSkills("检查以下引用资料： $review-code")
	if skill.Execute(context.Background(), load).OK {
		t.Fatal("incidental mention in task material unlocked manual skill")
	}
	registry.PrepareSkills("/skill:review-code inspect this change")
	if strings.Contains(skill.Spec().Description, "ORIGINAL_SKILL_BODY") {
		t.Fatal("skill body leaked into discovery")
	}
	resource := json.RawMessage(`{"name":"review-code","file":"guide.txt"}`)
	result := skill.Execute(context.Background(), load)
	if !result.OK || len(registry.ActiveSkills()) != 0 {
		t.Fatalf("execution activated without receipt: %#v", result)
	}
	if skill.Execute(context.Background(), resource).Code != "skill_not_loaded" {
		t.Fatal("unactivated resource readable")
	}
	if err := registry.AcceptSkillResult("skill", result); err != nil {
		t.Fatal(err)
	}
	if !skill.Execute(context.Background(), resource).OK {
		t.Fatal("activated resource unavailable")
	}
	for _, file := range []string{"../outside", "/etc/passwd", "unknown.txt"} {
		arguments, _ := json.Marshal(SkillArguments{Name: "review-code", File: file})
		if skill.Execute(context.Background(), arguments).OK {
			t.Fatalf("unsafe resource %q readable", file)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "guide.txt"), []byte("changed guide"), 0600); err != nil {
		t.Fatal(err)
	}
	if skill.Execute(context.Background(), resource).OK {
		t.Fatal("changed resource read from old snapshot")
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	repeated := skill.Execute(context.Background(), load)
	if !repeated.OK || !strings.Contains(string(repeated.Data), "ORIGINAL_SKILL_BODY") {
		t.Fatal("repeat load reread changed instructions")
	}
	if strings.Contains(string(registry.ModelResult("skill", result).Data), "ORIGINAL_SKILL_BODY") {
		t.Fatal("body duplicated in model receipt")
	}
}

// TestSkillMetadataRedactedBeforeModelDeclaration 目录提示词不能在工具声明阶段泄露凭据。
func TestSkillMetadataRedactedBeforeModelDeclaration(t *testing.T) {
	root := t.TempDir()
	directory := writeTestSkill(t, root, "review-code", "", "body")
	secret := "sk-" + strings.Repeat("a", 40)
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: review-code\ndescription: key "+secret+"\n---\nbody"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := DiscoverSkills([]SkillRoot{{Path: root, Scope: "workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	skill := &Skill{Catalog: catalog}
	if strings.Contains(skill.Spec().Description, secret) {
		t.Fatal("tool declaration leaked secret")
	}
	writeTestSkill(t, root, secret, "", "body")
	catalog, err = DiscoverSkills([]SkillRoot{{Path: root, Scope: "workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(catalog.Diagnostics)
	if strings.Contains(string(encoded), secret) || len(catalog.Entries()) != 1 {
		t.Fatal("credential-like skill name reached catalog or diagnostics")
	}
}

// TestSkillSharedUserLinksStayInsideCanonicalPackage 接纳用户选择的包根链接后，资源仍不能链接到根外。
func TestSkillSharedUserLinksStayInsideCanonicalPackage(t *testing.T) {
	shared, source := t.TempDir(), t.TempDir()
	directory := writeTestSkill(t, source, "review-code", "", "shared instructions")
	if err := os.Symlink(directory, filepath.Join(shared, "review-code")); err != nil {
		t.Skip(err)
	}
	strict, err := DiscoverSkills([]SkillRoot{{Path: shared, Scope: "workspace-shared"}})
	if err != nil || len(strict.Entries()) != 0 {
		t.Fatal("workspace link escaped its declared root")
	}
	catalog, err := DiscoverSkills([]SkillRoot{{Path: shared, Scope: "user-shared", AllowLinkedPackages: true}})
	if err != nil || len(catalog.Entries()) != 1 {
		t.Fatalf("user linked package unavailable: %v", err)
	}
	if _, err := catalog.Load("review-code"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Load("review-code"); err == nil {
		t.Fatal("package root link granted access to external resource")
	}
}
