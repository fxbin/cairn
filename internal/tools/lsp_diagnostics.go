// lsp_diagnostics.go 提供 lsp_diagnostics 只读工具：优先调用 gopls check，
// 否则回退 go vet / tsc；无可用诊断器时失败关闭，不伪造空诊断。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/ports"
)

// diagnosticsTimeout 限制单次诊断命令墙钟时间。
const diagnosticsTimeout = 60 * time.Second

// diagnosticsMaxItems 限制返回的诊断条数。
const diagnosticsMaxItems = 200

// LspDiagnostics 在工作区读取语言诊断（只读，不改文件）。
type LspDiagnostics struct{ Workspace Workspace }

// SideEffect 声明诊断命令无工作区副作用。
func (LspDiagnostics) SideEffect() SideEffectClass { return SideEffectNone }

// Spec 返回 lsp_diagnostics 的受限 JSON 参数定义。
func (tool LspDiagnostics) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "lsp_diagnostics",
		Description: "Read type/lint diagnostics for the workspace (gopls check, go vet, or tsc). Returns structured file/line/severity/message items. Fails closed when no diagnostic engine is available.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","description":"Optional workspace-relative file or directory to diagnose."},"engine":{"type":"string","enum":["auto","gopls","go_vet","tsc"],"description":"Force an engine; default auto."}}}`),
	}
}

// diagnosticItem 是一条可定位诊断。
type diagnosticItem struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// diagnosticsResult 是 lsp_diagnostics 的稳定数据形状。
type diagnosticsResult struct {
	Engine       string           `json:"engine"`
	Path         string           `json:"path,omitempty"`
	Items        []diagnosticItem `json:"items"`
	ItemCount    int              `json:"item_count"`
	OutputTail   string           `json:"output_tail,omitempty"`
	EngineBinary string           `json:"engine_binary,omitempty"`
}

// diagnosticsArguments 是 lsp_diagnostics 的内部解码参数。
type diagnosticsArguments struct {
	Path   string `json:"path"`
	Engine string `json:"engine"`
}

var (
	reGoplsLine = regexp.MustCompile(`(?m)^(.+?):(\d+):(\d+):\s+(.+)$`)
	reGoVetLine = regexp.MustCompile(`(?m)^(.+?):(\d+):(\d+):\s+(.+)$`)
	reTscLine   = regexp.MustCompile(`(?m)^(.+?)\((\d+),(\d+)\):\s+(error|warning)\s+TS\d+:\s*(.+)$`)
)

// Execute 选择引擎并返回结构化诊断。
func (tool LspDiagnostics) Execute(ctx context.Context, raw json.RawMessage) Result {
	var args diagnosticsArguments
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return Failure("invalid_arguments", "lsp_diagnostics arguments must be an object")
		}
	}
	root := tool.Workspace.Root
	target := root
	if strings.TrimSpace(args.Path) != "" {
		resolved, relative, err := tool.Workspace.Resolve(args.Path)
		if err != nil {
			return Failure("invalid_path", err.Error())
		}
		target = resolved
		_ = relative
	}
	engine := strings.TrimSpace(args.Engine)
	if engine == "" {
		engine = "auto"
	}
	switch engine {
	case "auto", "gopls", "go_vet", "tsc":
	default:
		return Failure("invalid_arguments", "engine must be auto|gopls|go_vet|tsc")
	}

	picked, binary, argv, err := planDiagnostics(engine, root, target)
	if err != nil {
		return Failure("diagnostics_unavailable", err.Error())
	}
	output, err := runDiagnosticsCommand(ctx, root, diagnosticsTimeout, argv)
	if err != nil {
		// 诊断器自身崩溃与「有诊断」不同：有输出时仍解析，无输出则失败关闭。
		if strings.TrimSpace(output) == "" {
			return Failure("diagnostics_failed", err.Error())
		}
	}
	items := parseDiagnostics(picked, output)
	if len(items) > diagnosticsMaxItems {
		items = items[:diagnosticsMaxItems]
	}
	data := diagnosticsResult{
		Engine:       picked,
		Items:        items,
		ItemCount:    len(items),
		OutputTail:   tailText(output, 4*1024),
		EngineBinary: binary,
	}
	if strings.TrimSpace(args.Path) != "" {
		data.Path = args.Path
	}
	preview := fmt.Sprintf("%s · %d diagnostic(s)", picked, len(items))
	if len(items) > 0 {
		preview += " · " + items[0].File
	}
	return Success(data, preview, int64(len(output)), "", false)
}

// planDiagnostics 按引擎偏好解析可执行文件与 argv；找不到时返回可读错误。
func planDiagnostics(engine, root, target string) (string, string, []string, error) {
	try := func(names ...string) (string, error) {
		for _, name := range names {
			if path, err := exec.LookPath(name); err == nil {
				return path, nil
			}
		}
		return "", fmt.Errorf("executable not found: %s", strings.Join(names, ", "))
	}
	switch engine {
	case "gopls":
		bin, err := try("gopls")
		if err != nil {
			return "", "", nil, err
		}
		return "gopls", bin, []string{bin, "check", target}, nil
	case "go_vet":
		bin, err := try("go")
		if err != nil {
			return "", "", nil, err
		}
		return "go_vet", bin, []string{bin, "vet", "./..."}, nil
	case "tsc":
		bin, err := try("npx", "tsc")
		if err != nil {
			return "", "", nil, err
		}
		return "tsc", bin, []string{bin, "--noEmit", "-p", filepath.Dir(target)}, nil
	default:
		// auto：Go 仓库优先 gopls，其次 go vet；存在 tsconfig 时补 tsc 不在此层叠加。
		if hasGoModule(root) {
			if bin, err := try("gopls"); err == nil {
				return "gopls", bin, []string{bin, "check", target}, nil
			}
			if bin, err := try("go"); err == nil {
				return "go_vet", bin, []string{bin, "vet", "./..."}, nil
			}
		}
		if bin, err := try("npx"); err == nil {
			return "tsc", bin, []string{bin, "--noEmit", "-p", root}, nil
		}
		return "", "", nil, errors.New("no diagnostic engine available (gopls/go/tsc)")
	}
}

// hasGoModule 判断目录是否为 Go 模块根。
func hasGoModule(root string) bool {
	if _, err := exec.LookPath("go"); err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(root, "go.mod"))
	return err == nil && !info.IsDir()
}

// runDiagnosticsCommand 执行诊断器并收集输出。
func runDiagnosticsCommand(ctx context.Context, dir string, timeout time.Duration, argv []string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	command.Dir = dir
	var combined limitedBuffer
	combined.limit = 256 * 1024
	command.Stdout = &combined
	command.Stderr = &combined
	err := command.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return string(artifact.Redact([]byte(combined.String()))), fmt.Errorf("diagnostics timed out after %s", timeout)
	}
	if err != nil {
		return string(artifact.Redact([]byte(combined.String()))), err
	}
	return string(artifact.Redact([]byte(combined.String()))), nil
}

// parseDiagnostics 按引擎解析诊断行。
func parseDiagnostics(engine, output string) []diagnosticItem {
	items := make([]diagnosticItem, 0, 8)
	lineRe := reGoVetLine
	if engine == "gopls" {
		lineRe = reGoplsLine
	}
	if engine == "tsc" {
		for _, match := range reTscLine.FindAllStringSubmatch(output, -1) {
			line, _ := strconv.Atoi(match[2])
			column, _ := strconv.Atoi(match[3])
			items = append(items, diagnosticItem{
				File:     match[1],
				Line:     line,
				Column:   column,
				Severity: match[4],
				Message:  match[5],
			})
		}
		return items
	}
	for _, raw := range strings.Split(output, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		match := lineRe.FindStringSubmatch(raw)
		if match == nil {
			continue
		}
		line, _ := strconv.Atoi(match[2])
		column, _ := strconv.Atoi(match[3])
		items = append(items, diagnosticItem{
			File:     match[1],
			Line:     line,
			Column:   column,
			Severity: "error",
			Message:  match[4],
		})
	}
	return items
}
