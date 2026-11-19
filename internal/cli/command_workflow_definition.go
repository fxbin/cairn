// [INPUT]: 依赖定义草案、不可变修订与 CLI 作用域参数。
// [OUTPUT]: 暴露定义列表、显式保存、按修订启动与历史读取。
// [POS]: 客户端校验 JSON 边界。定义验证和启动由应用层执行。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件提供版本化工作流定义及启动入口。
// Author: fxbin
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/canonicaljson"
)

// workflowDraftFileLimit 限制 CLI 为草案读取的字节数，应用层另验冻结制品大小。
const workflowDraftFileLimit = 1024 * 1024

// workflowDefinitions 列出工作区内每个定义的最新修订。
func (command Command) workflowDefinitions(ctx context.Context, arguments []string) int {
	flags := command.appFlags("workflow definitions", "")
	if !flags.parse(arguments, 0) {
		return ExitUsage
	}
	scope, err := command.workflowScope(flags)
	if err != nil {
		return ExitUsage
	}
	definitions, err := command.Service.ListWorkflowDefinitions(ctx, scope)
	if err != nil {
		fmt.Fprintln(command.Stderr, "workflow definitions failed:", err)
		return exitFor(err)
	}
	if flags.jsonOutput {
		return command.writeJSONOutput("workflow definitions", definitions)
	}
	for _, definition := range definitions {
		fmt.Fprintf(command.Stdout, "%s · revision %d · %d nodes · %s\n", definition.ID, definition.Revision, len(definition.Nodes), definition.Name)
	}
	return ExitOK
}

// workflowSaveDefinition 按明确的预期修订提交调用方指定的 JSON 草案。
func (command Command) workflowSaveDefinition(ctx context.Context, arguments []string) int {
	flags := command.appFlags("workflow save-definition", "<draft.json> [--expected-revision <n>]")
	expected := flags.Uint64("expected-revision", 0, "expected definition revision (0 creates a definition)")
	if !flags.parse(arguments, 1) {
		return ExitUsage
	}
	draft, err := readWorkflowDefinitionDraft(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(command.Stderr, "workflow definition draft rejected:", err)
		return ExitUsage
	}
	scope, err := command.workflowScope(flags)
	if err != nil {
		return ExitUsage
	}
	definition, err := command.Service.SaveWorkflowDefinition(ctx, scope, draft, *expected)
	if err != nil {
		fmt.Fprintln(command.Stderr, "workflow save-definition failed:", err)
		return exitFor(err)
	}
	if flags.jsonOutput {
		return command.writeJSONOutput("workflow save-definition", definition)
	}
	fmt.Fprintf(command.Stdout, "definition: %s\nrevision: %d\n", definition.ID, definition.Revision)
	return ExitOK
}

// readWorkflowDefinitionDraft 读取有界普通文件，拒绝未知字段与额外 JSON 文档。
func readWorkflowDefinitionDraft(path string) (app.WorkflowDefinitionDraft, error) {
	var draft app.WorkflowDefinitionDraft
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return draft, errors.New("a readable regular JSON file is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return draft, errors.New("open workflow definition draft failed")
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, workflowDraftFileLimit+1))
	if err != nil || len(body) > workflowDraftFileLimit {
		return draft, errors.New("workflow definition draft is unreadable or too large")
	}
	body = bytes.Trim(body, " \t\r\n")
	if len(body) == 0 || body[0] != '{' || !utf8.Valid(body) {
		return draft, errors.New("workflow definition must be a UTF-8 JSON object")
	}
	if _, err := canonicaljson.Marshal(json.RawMessage(body)); err != nil {
		return draft, errors.New("invalid workflow definition JSON")
	}
	if err := validateWorkflowDraftFields(body); err != nil {
		return draft, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&draft); err != nil {
		return draft, errors.New("invalid workflow definition JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return draft, errors.New("extra content after workflow definition JSON")
	}
	return draft, nil
}

// validateWorkflowDraftFields 在类型解码前按原拼写核对根对象和节点键。
func validateWorkflowDraftFields(body []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return errors.New("invalid workflow definition JSON fields")
	}
	for name := range fields {
		switch name {
		case "id", "name", "nodes":
		default:
			return errors.New("unknown or incorrectly cased workflow definition field")
		}
	}
	encodedNodes, exists := fields["nodes"]
	if !exists {
		return nil
	}
	var nodes []map[string]json.RawMessage
	if err := json.Unmarshal(encodedNodes, &nodes); err != nil {
		return errors.New("workflow definition nodes must be JSON objects in an array")
	}
	for _, node := range nodes {
		if node == nil {
			return errors.New("workflow definition node must be a JSON object")
		}
		for name, encodedValue := range node {
			switch name {
			case "id", "kind", "prompt", "model", "read_only", "max_attempts", "inputs", "retry_from":
			case "argv":
				var arguments []json.RawMessage
				if err := json.Unmarshal(encodedValue, &arguments); err != nil {
					return errors.New("workflow script argv must be an array")
				}
				for _, commandArgument := range arguments {
					if !losslessWorkflowArgument(commandArgument) {
						return errors.New("workflow script argv must contain lossless Unicode strings")
					}
				}
			default:
				return errors.New("unknown or incorrectly cased workflow node field")
			}
		}
	}
	return nil
}

// losslessWorkflowArgument 拒绝 Go JSON 会替换成 U+FFFD 的孤立代理码元。
func losslessWorkflowArgument(encoded json.RawMessage) bool {
	const (
		escapeLength = 6
		highStart    = 0xD800
		lowStart     = 0xDC00
		surrogateEnd = 0xDFFF
	)
	if len(encoded) < 2 || encoded[0] != '"' || encoded[len(encoded)-1] != '"' {
		return false
	}
	end := len(encoded) - 1
	for index := 1; index < end; {
		if encoded[index] != '\\' {
			index++
			continue
		}
		if index+1 >= end {
			return false
		}
		if encoded[index+1] != 'u' {
			index += 2
			continue
		}
		if index+escapeLength > end {
			return false
		}
		unit, err := strconv.ParseUint(string(encoded[index+2:index+escapeLength]), 16, 16)
		if err != nil || (unit >= lowStart && unit <= surrogateEnd) {
			return false
		}
		index += escapeLength
		if unit < highStart || unit >= lowStart {
			continue
		}
		if index+escapeLength > end || encoded[index] != '\\' || encoded[index+1] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(encoded[index+2:index+escapeLength]), 16, 16)
		if err != nil || low < lowStart || low > surrogateEnd {
			return false
		}
		index += escapeLength
	}
	return true
}

// workflowStart 显式选择已保存的定义身份和修订。
func (command Command) workflowStart(ctx context.Context, arguments []string) int {
	flags := command.appFlags("workflow start", "<definition-id> --revision <n>")
	revision := flags.Uint64("revision", 0, "explicit stored definition revision")
	if !flags.parse(arguments, 1) {
		return ExitUsage
	}
	if *revision == 0 {
		flags.Usage()
		return ExitUsage
	}
	scope, err := command.workflowScope(flags)
	if err != nil {
		return ExitUsage
	}
	outcome, err := command.Service.StartWorkflow(ctx, app.StartWorkflowOptions{
		Scope: scope, DefinitionID: app.WorkflowDefinitionID(flags.Arg(0)), Revision: *revision,
	})
	return command.reportWorkflowOutcome("workflow start", outcome, err, flags.jsonOutput)
}

// workflowHistory 展示实例及其持久尝试历史。
func (command Command) workflowHistory(ctx context.Context, arguments []string) int {
	flags := command.appFlags("workflow history", "")
	if !flags.parse(arguments, 0) {
		return ExitUsage
	}
	scope, err := command.workflowScope(flags)
	if err != nil {
		return ExitUsage
	}
	views, err := command.Service.WorkflowHistory(ctx, scope)
	if err != nil {
		fmt.Fprintln(command.Stderr, "workflow history failed:", err)
		return exitFor(err)
	}
	if flags.jsonOutput {
		return command.writeJSONOutput("workflow history", views)
	}
	for _, view := range views {
		command.printWorkflowView(view)
	}
	return ExitOK
}

// workflowScope 读取生效配置，并将错误归入命令的用法退出。
func (command Command) workflowScope(flags *appCommandFlags) (app.WorkflowScope, error) {
	configuration, err := command.loadAppConfiguration(flags)
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
	}
	return app.WorkflowScope{Workspace: flags.workspace, StateDir: flags.stateDir, Config: configuration}, err
}
