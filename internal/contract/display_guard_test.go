// display_guard_test 锁定"一次定义、桥接下发"：CLI、TUI 与桌面桥接必须调用
// 同一个 contract 词表函数，任何界面层包都不得自行复制展示词常量。该守卫
// 读取源码而不是运行时状态，因此能在有人重新引入本地映射时立刻失败。
//
// Author: fxbin
package contract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// guardedVocabularyPackages 是必须复用统一词表的界面层包。
var guardedVocabularyPackages = []string{"cli", "tui", "desktop"}

// guardedVocabularyFunctions 是每个界面层包都必须直接调用的词表函数。
var guardedVocabularyFunctions = []string{
	"contract.RunPhaseLabel(",
	"contract.CompletionTrustLabel(",
}

// vocabularyDisplayWords 返回词表独占的展示词；界面层包不得声明同名常量。
func vocabularyDisplayWords() []string {
	words := []string{UnknownDisplayLabel}
	for _, phase := range allRunPhases() {
		words = append(words, RunPhaseLabel(string(phase)))
	}
	for _, trust := range []CompletionTrust{CompletionTrustTrusted, CompletionTrustUntrusted, CompletionTrustUnknown} {
		words = append(words, CompletionTrustLabel(trust))
	}
	return words
}

// TestVocabularyPackagesReuseContractFunctions 锁定 CLI、TUI 与桌面桥接都直接
// 调用同一个 contract 词表函数，而不是各自复制常量。
func TestVocabularyPackagesReuseContractFunctions(t *testing.T) {
	for _, name := range guardedVocabularyPackages {
		source := readPackageSources(t, name)
		for _, function := range guardedVocabularyFunctions {
			if !strings.Contains(source, function) {
				t.Fatalf("包 %s 没有调用 %s，展示词必须来自统一词表", name, function)
			}
		}
	}
}

// TestReconcileHintHasSingleSource 锁定对账动作指引只有一处定义：应用层的
// 下一步提示与 TUI 的等待对账提示都必须取自 contract 词表。
func TestReconcileHintHasSingleSource(t *testing.T) {
	for _, name := range []string{"app", "tui"} {
		source := readPackageSources(t, name)
		if !strings.Contains(source, "contract.ReconcileActionHint") {
			t.Fatalf("包 %s 没有引用 contract.ReconcileActionHint，对账指引必须来自统一词表", name)
		}
	}
}

// TestInterfacePackagesDoNotCopyDisplayWords 锁定界面层包没有各自声明展示词
// 常量；复制即意味着两套说法会漂移。
func TestInterfacePackagesDoNotCopyDisplayWords(t *testing.T) {
	words := vocabularyDisplayWords()
	for _, name := range guardedVocabularyPackages {
		for file, declarations := range packageStringConstants(t, name) {
			for declaration, value := range declarations {
				for _, word := range words {
					if value == word {
						t.Fatalf("%s 的 %s 复制了展示词 %q：%s", name, file, word, declaration)
					}
				}
			}
		}
	}
}

// readPackageSources 读取指定包的全部 Go 源码并拼成一段文本。
func readPackageSources(t *testing.T, name string) string {
	t.Helper()
	directory := filepath.Join("..", name)
	entries, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatalf("读取包 %s 源码: %v", name, err)
	}
	var builder strings.Builder
	for _, entry := range entries {
		content, err := os.ReadFile(entry)
		if err != nil {
			t.Fatalf("读取 %s: %v", entry, err)
		}
		builder.Write(content)
		builder.WriteString("\n")
	}
	if builder.Len() == 0 {
		t.Fatalf("包 %s 没有源码", name)
	}
	return builder.String()
}

// packageStringConstants 收集指定包内以字符串字面量声明的常量与变量；返回
// 文件名到"声明名=取值"的映射。
func packageStringConstants(t *testing.T, name string) map[string]map[string]string {
	t.Helper()
	result := map[string]map[string]string{}
	directory := filepath.Join("..", name)
	entries, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatalf("读取包 %s 源码: %v", name, err)
	}
	for _, entry := range entries {
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, entry, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s: %v", entry, err)
		}
		declarations := map[string]string{}
		for _, declaration := range parsed.Decls {
			generic, ok := declaration.(*ast.GenDecl)
			if !ok || (generic.Tok != token.CONST && generic.Tok != token.VAR) {
				continue
			}
			for _, specification := range generic.Specs {
				value, ok := specification.(*ast.ValueSpec)
				if !ok || len(value.Values) != len(value.Names) {
					continue
				}
				for index, name := range value.Names {
					literal, ok := value.Values[index].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					unquoted, err := strconv.Unquote(literal.Value)
					if err != nil {
						continue
					}
					declarations[name.Name] = unquoted
				}
			}
		}
		if len(declarations) > 0 {
			result[filepath.Base(entry)] = declarations
		}
	}
	return result
}
