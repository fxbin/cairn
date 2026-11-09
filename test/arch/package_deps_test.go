// Package arch_test 提供 Cairn 的包依赖守卫：以 `go list` 的真实 import 边为准，
// 阻止 Core、合同层、Runtime、配置层与 Studio 之间新增越层引用。
//
// 守卫规则（Issue #40 P0）：
//  1. 合同层（domain/canonicaljson/buildinfo/contract/ports/decision）只能依赖合同层。
//  2. Core（internal/agent）只能依赖合同层，不得直接持有 Runtime 具体实现。
//  3. Runtime 不得依赖 Core、Application、Studio 与带外工具链；Runtime 内部除
//     adapters 外不得直接 import internal/tools（授权门禁依赖工具注册表属于反转）。
//  4. 配置层（internal/config）不得依赖 Core、Application、Studio 与带外工具链。
//  5. Studio（desktop/tui/cli/cmd/gui）不得直接依赖 Core 与 Runtime，只能经
//     Application、合同层与配置层交互。
//  6. Application 是组合根、benchmark 是带外工具链，可依赖任意内部包。
//
// 当前实现尚未完成解耦，因此把已存在的越层边逐条登记在 knownExceptions 中；
// 新增越层边、未登记分层的新包、以及例外消失后未同步删除，都会让本测试失败。
//
// 本文件只做静态依赖检查，不修改生产代码，也不进入任何构建产物。
// Author: fxbin
package arch_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// packageLayer 是包在目标架构中的分层标识。
type packageLayer string

const (
	// layerContract 是共享合同层：领域事实、事件 schema、窄端口与纯工具。
	layerContract packageLayer = "合同层"
	// layerRuntime 是运行期基础设施与适配器。
	layerRuntime packageLayer = "Runtime"
	// layerConfig 是配置装载与指纹层。
	layerConfig packageLayer = "配置层"
	// layerCore 是 Agent 智能执行核心。
	layerCore packageLayer = "Core"
	// layerApplication 是用例编排与组合根。
	layerApplication packageLayer = "Application"
	// layerStudio 是 CLI、TUI、Desktop 与 GUI 入口。
	layerStudio packageLayer = "Studio"
	// layerToolchain 是带外评测工具链，不进入生产依赖守卫的可达路径。
	layerToolchain packageLayer = "工具链"
)

// 解耦步骤名称，与 Issue #40 P0 底稿的拆分 PR 顺序一一对应。
const (
	stepArtifactPort = "第 2 步（artifact 端口）"
	stepHooksPort    = "第 3 步（hooks 数据合同上移）"
	stepCredential   = "第 4 步（credential 脱敏端口）"
	stepPolicyPort   = "第 5 步（policy.Gate 授权端口）"
	stepToolsPort    = "第 7 步（tools 端口拆分）"
	stepStudio       = "第 8 步（Studio Adapter 收敛）"
	stepMigration    = "第 9 步（目录迁移与合同归位）"
)

// packageLayers 登记 internal 一级包与入口的目标分层。
// 新增 internal 包必须在此登记，否则守卫会把它视为未分类包并失败。
var packageLayers = map[string]packageLayer{
	// 合同层。
	"internal/domain":        layerContract,
	"internal/canonicaljson": layerContract,
	"internal/buildinfo":     layerContract,
	"internal/contract":      layerContract,
	"internal/ports":         layerContract,
	"internal/decision":      layerContract,
	// Runtime 基础能力与适配器。
	"internal/userdir":      layerRuntime,
	"internal/lock":         layerRuntime,
	"internal/credential":   layerRuntime,
	"internal/hooks":        layerRuntime,
	"internal/workflow":     layerRuntime,
	"internal/artifact":     layerRuntime,
	"internal/memory":       layerRuntime,
	"internal/provider":     layerRuntime,
	"internal/modelcatalog": layerRuntime,
	"internal/telemetry":    layerRuntime,
	"internal/browser":      layerRuntime,
	"internal/sandboxeval":  layerRuntime,
	"internal/policy":       layerRuntime,
	"internal/tools":        layerRuntime,
	// 配置层。
	"internal/config": layerConfig,
	// Core。
	"internal/agent": layerCore,
	// Application。
	"internal/app": layerApplication,
	// Studio 与入口。
	"internal/desktop": layerStudio,
	"internal/tui":     layerStudio,
	"internal/cli":     layerStudio,
	"cmd/cairn":        layerStudio,
	// 带外工具链。
	"internal/benchmark": layerToolchain,
}

// packageLayerPrefixes 登记按前缀归层的目录：Runtime 适配器与 GUI 壳。
var packageLayerPrefixes = []struct {
	prefix string
	layer  packageLayer
}{
	{prefix: "internal/adapters/", layer: layerRuntime},
	{prefix: "gui/", layer: layerStudio},
}

// knownException 是一条已存在的越层依赖，按解耦步骤逐条登记、逐条删除。
type knownException struct {
	// Importer 是发起 import 的内部包。
	Importer string
	// Imported 是被 import 的内部包。
	Imported string
	// Step 是计划删除该越层边的解耦步骤。
	Step string
	// Reason 说明该边当前为何存在。
	Reason string
}

// knownExceptions 是当前 main 上已存在、尚未解耦的越层边清单。
// 每次解耦 PR 完成后必须同步删除对应条目；例外消失而未删除同样会让测试失败。
// 清单只允许缩短，不允许为新增越层边追加条目。
var knownExceptions = []knownException{
	// Core 直接持有 Runtime 具体实现的 6 类引用。
	{Importer: "internal/agent", Imported: "internal/artifact", Step: stepArtifactPort, Reason: "Loop.Artifacts 直接持有 *artifact.Store"},
	{Importer: "internal/agent", Imported: "internal/hooks", Step: stepHooksPort, Reason: "Loop.Hooks 与钩子派发直接使用 hooks 数据合同"},
	{Importer: "internal/agent", Imported: "internal/credential", Step: stepCredential, Reason: "sensitive.go 内联调用 credential.Key 做敏感性判定"},
	{Importer: "internal/agent", Imported: "internal/policy", Step: stepPolicyPort, Reason: "Loop.Gate 直接持有 *policy.Gate 并穿透 Patch 规划与工作区观察"},
	{Importer: "internal/agent", Imported: "internal/tools", Step: stepToolsPort, Reason: "Loop.Tools 直接持有 *tools.Registry，21 个生产文件引用 tools 类型"},

	// 合同账本依赖 Runtime 制品与凭据。
	{Importer: "internal/decision", Imported: "internal/artifact", Step: stepArtifactPort, Reason: "decision/text.go 引用 artifact 内容摘要"},
	{Importer: "internal/decision", Imported: "internal/credential", Step: stepCredential, Reason: "decision/text.go 引用 credential 脱敏"},

	// 端口层依赖无运行时消费方的工作流脚手架。
	{Importer: "internal/ports", Imported: "internal/workflow", Step: stepMigration, Reason: "ports/workflow.go 直接引用 workflow 合同，归位后再删"},

	// Runtime 内部反转：授权门禁依赖工具注册表判定副作用。
	{Importer: "internal/policy", Imported: "internal/tools", Step: stepToolsPort, Reason: "policy/gate.go 依赖 tools 注册表做副作用与补丁规划"},

	// Studio 越过 Application 直接使用 Core 与 Runtime。
	{Importer: "internal/desktop", Imported: "internal/agent", Step: stepStudio, Reason: "run.go 直接构造 agent.NewSteeringBridge"},
	{Importer: "internal/desktop", Imported: "internal/tools", Step: stepStudio, Reason: "files.go 直接构造 tools.NewWorkspace 与工具常量"},
	{Importer: "internal/desktop", Imported: "internal/userdir", Step: stepStudio, Reason: "桌面端直接解析用户配置根"},
	{Importer: "internal/tui", Imported: "internal/agent", Step: stepStudio, Reason: "run.go 直接引用 Core 类型"},
	{Importer: "internal/tui", Imported: "internal/artifact", Step: stepStudio, Reason: "终端界面直接读取制品"},
	{Importer: "internal/tui", Imported: "internal/credential", Step: stepStudio, Reason: "终端界面直接装载凭据状态"},
	{Importer: "internal/tui", Imported: "internal/modelcatalog", Step: stepStudio, Reason: "终端界面直接读取模型目录建议"},
	{Importer: "internal/tui", Imported: "internal/tools", Step: stepStudio, Reason: "补丁预览、引用展开直接使用 tools 内部类型"},
	{Importer: "internal/cli", Imported: "internal/artifact", Step: stepStudio, Reason: "CLI 直接读取制品"},
	{Importer: "internal/cli", Imported: "internal/memory", Step: stepStudio, Reason: "CLI 直接访问记忆合同与存储"},
	{Importer: "internal/cli", Imported: "internal/modelcatalog", Step: stepStudio, Reason: "CLI 直接读取模型目录建议"},
	{Importer: "internal/cli", Imported: "internal/provider", Step: stepStudio, Reason: "CLI 直接装配 Provider 身份与准入"},
	{Importer: "internal/cli", Imported: "internal/sandboxeval", Step: stepStudio, Reason: "CLI 直接调用沙箱评测入口"},
	{Importer: "internal/cli", Imported: "internal/tools", Step: stepStudio, Reason: "package.go 直接调用 tools.InspectPackage 等实现"},
	{Importer: "internal/cli", Imported: "internal/userdir", Step: stepStudio, Reason: "CLI 直接解析用户配置根"},
	{Importer: "internal/cli", Imported: "internal/workflow", Step: stepStudio, Reason: "CLI 直接读取工作流定义"},
	{Importer: "cmd/cairn", Imported: "internal/credential", Step: stepStudio, Reason: "进程入口直接装载凭据"},
	{Importer: "cmd/cairn", Imported: "internal/sandboxeval", Step: stepStudio, Reason: "进程入口直接调用沙箱评测"},
}

// listedPackage 是 `go list -json` 中守卫需要的字段投影。
type listedPackage struct {
	// ImportPath 是完整 import 路径。
	ImportPath string
	// Imports 是生产代码的 import 列表，不含测试依赖。
	Imports []string
}

// dependencyEdge 表示一条同模块内的 import 边。
type dependencyEdge struct {
	// Importer 是发起 import 的相对包路径。
	Importer string
	// Imported 是被 import 的相对包路径。
	Imported string
}

// layerOf 返回内部包的目标分层；未登记时第二返回值为 false。
func layerOf(relativePath string) (packageLayer, bool) {
	if layer, ok := packageLayers[relativePath]; ok {
		return layer, true
	}
	for _, entry := range packageLayerPrefixes {
		if strings.HasPrefix(relativePath, entry.prefix) {
			return entry.layer, true
		}
	}
	return "", false
}

// forbiddenEdge 判断 importer 层是否禁止依赖 imported 层。
// Application 是组合根、工具链是带外入口，二者不受层内限制。
func forbiddenEdge(importer, imported packageLayer) bool {
	if importer == imported {
		return false
	}
	switch importer {
	case layerApplication, layerToolchain:
		return false
	case layerStudio:
		return imported == layerCore || imported == layerRuntime
	case layerCore, layerContract:
		// Core 与合同层只允许停留在合同层。
		return imported != layerContract
	case layerRuntime, layerConfig:
		return imported == layerCore || imported == layerApplication ||
			imported == layerStudio || imported == layerToolchain
	}
	return true
}

// toolsRegistryLeak 判断 Runtime 内部是否绕过端口直接依赖工具注册表。
// adapters 作为装配适配器可以引用 tools，其余 Runtime 包必须经端口访问。
func toolsRegistryLeak(importer, imported string, importerLayer packageLayer) bool {
	if importerLayer != layerRuntime || imported != "internal/tools" || importer == "internal/tools" {
		return false
	}
	return !strings.HasPrefix(importer, "internal/adapters/")
}

// TestPackageDependencyGuard 校验同模块依赖边符合分层规则，并逐条比对已知例外。
func TestPackageDependencyGuard(t *testing.T) {
	root := moduleRoot(t)
	modulePath := modulePathOf(t, root)
	packages := listPackages(t, root)

	actual := make(map[dependencyEdge]struct{})
	var unclassified []string
	for _, pkg := range packages {
		importer := strings.TrimPrefix(pkg.ImportPath, modulePath+"/")
		if importer == pkg.ImportPath {
			continue
		}
		importerLayer, ok := layerOf(importer)
		if !ok {
			unclassified = append(unclassified, importer)
			continue
		}
		for _, importedPath := range pkg.Imports {
			imported := strings.TrimPrefix(importedPath, modulePath+"/")
			if imported == importedPath {
				continue
			}
			importedLayer, ok := layerOf(imported)
			if !ok {
				unclassified = append(unclassified, imported)
				continue
			}
			if forbiddenEdge(importerLayer, importedLayer) || toolsRegistryLeak(importer, imported, importerLayer) {
				actual[dependencyEdge{Importer: importer, Imported: imported}] = struct{}{}
			}
		}
	}

	unclassified = uniqueSorted(unclassified)
	for _, path := range unclassified {
		t.Errorf("未登记分层的内部包：%s；请在 packageLayers 或 packageLayerPrefixes 中登记目标层", path)
	}

	expected := make(map[dependencyEdge]knownException, len(knownExceptions))
	for _, exception := range knownExceptions {
		edge := dependencyEdge{Importer: exception.Importer, Imported: exception.Imported}
		if _, duplicated := expected[edge]; duplicated {
			t.Errorf("已知例外重复登记：%s -> %s", exception.Importer, exception.Imported)
		}
		expected[edge] = exception
	}

	for _, edge := range sortedEdges(actual) {
		if _, known := expected[edge]; known {
			continue
		}
		importerLayer, _ := layerOf(edge.Importer)
		importedLayer, _ := layerOf(edge.Imported)
		t.Errorf("新增越层依赖：%s（%s）-> %s（%s）；应改为经端口或 Application 交互，不得直接 import",
			edge.Importer, importerLayer, edge.Imported, importedLayer)
	}
	for _, edge := range sortedExceptionEdges(expected) {
		if _, stillPresent := actual[edge]; stillPresent {
			continue
		}
		exception := expected[edge]
		t.Errorf("例外已消失：%s -> %s（%s，%s）；请从 knownExceptions 删除该条",
			edge.Importer, edge.Imported, exception.Step, exception.Reason)
	}
}

// moduleRoot 从当前工作目录向上查找 go.mod，返回仓库根目录。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取当前目录失败: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("未找到 go.mod，无法确定仓库根目录")
		}
		dir = parent
	}
}

// modulePathOf 读取 go.mod 的 module 声明，避免硬编码模块路径。
func modulePathOf(t *testing.T, root string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("读取 go.mod 失败: %v", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatalf("go.mod 缺少 module 声明")
	return ""
}

// listPackages 调用 go list 获取 internal 与 cmd 的生产 import 边。
func listPackages(t *testing.T, root string) []listedPackage {
	t.Helper()
	command := exec.Command("go", "list", "-json", "./internal/...", "./cmd/...")
	command.Dir = root
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go list 失败: %v\n%s", err, stderr.String())
	}
	decoder := json.NewDecoder(&stdout)
	packages := make([]listedPackage, 0, 32)
	for decoder.More() {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			t.Fatalf("解析 go list 输出失败: %v", err)
		}
		packages = append(packages, pkg)
	}
	return packages
}

// sortedEdges 返回按包名排序的依赖边，保证失败信息稳定可读。
func sortedEdges(edges map[dependencyEdge]struct{}) []dependencyEdge {
	result := make([]dependencyEdge, 0, len(edges))
	for edge := range edges {
		result = append(result, edge)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Importer != result[j].Importer {
			return result[i].Importer < result[j].Importer
		}
		return result[i].Imported < result[j].Imported
	})
	return result
}

// sortedExceptionEdges 返回按包名排序的已知例外边。
func sortedExceptionEdges(edges map[dependencyEdge]knownException) []dependencyEdge {
	result := make([]dependencyEdge, 0, len(edges))
	for edge := range edges {
		result = append(result, edge)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Importer != result[j].Importer {
			return result[i].Importer < result[j].Importer
		}
		return result[i].Imported < result[j].Imported
	})
	return result
}

// uniqueSorted 去重并排序字符串切片。
func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
