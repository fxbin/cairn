// Package cli 提供提示模板命令；模板是用户级文本资产，渲染不执行脚本、不读秘密。
// Author: fxbin
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
	"github.com/fxbin/cairn/internal/userdir"
)

// templateCommand 管理显式本地提示模板：list/show/render；渲染参数显式传入。
func (command Command) templateCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "template requires list, show or render")
		return ExitUsage
	}
	action := arguments[0]
	switch action {
	case "list", "show", "render":
	default:
		fmt.Fprintln(command.Stderr, "unknown template subcommand")
		return ExitUsage
	}
	flags := flag.NewFlagSet("template "+action, flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	root := flags.String("root", "", "template directory (defaults to ~/.cairn/templates)")
	paramFlags := stringListFlag{}
	flags.Var(&paramFlags, "param", "render parameter as name=value (repeatable)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeFlagsThenPositionals(arguments[1:], map[string]struct{}{"--root": {}, "--param": {}})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if action == "list" && flags.NArg() != 0 || action != "list" && flags.NArg() != 1 {
		return ExitUsage
	}
	if ctx.Err() != nil {
		return ExitCancelled
	}
	templateRoot := strings.TrimSpace(*root)
	if templateRoot == "" {
		path, err := userdir.Path("templates")
		if err != nil {
			fmt.Fprintln(command.Stderr, "resolve template root failed")
			return ExitFailed
		}
		templateRoot = path
	}
	store, err := tools.NewTemplateStore(templateRoot)
	if err != nil {
		fmt.Fprintln(command.Stderr, "template store failed:", err)
		return ExitFailed
	}
	switch action {
	case "list":
		catalog, listErr := store.List()
		if listErr != nil {
			fmt.Fprintln(command.Stderr, "template list failed:", listErr)
			return ExitFailed
		}
		for _, diagnostic := range catalog.Diagnostics {
			fmt.Fprintf(command.Stderr, "template: %s: %s\n", diagnostic.Name, diagnostic.Code)
		}
		for _, entry := range catalog.Entries {
			if *jsonOutput {
				if writeJSONChecked(command.Stdout, entry) != nil {
					return ExitFailed
				}
			} else {
				fmt.Fprintln(command.Stdout, tools.TemplateSummary(entry))
			}
		}
		return ExitOK
	case "show":
		document, loadErr := store.Load(flags.Arg(0))
		if loadErr != nil {
			fmt.Fprintln(command.Stderr, "template show failed:", loadErr)
			return ExitFailed
		}
		if *jsonOutput {
			if writeJSONChecked(command.Stdout, document) != nil {
				return ExitFailed
			}
			return ExitOK
		}
		fmt.Fprintf(command.Stdout, "name: %s\n", document.Name)
		if document.Description != "" {
			fmt.Fprintf(command.Stdout, "description: %s\n", document.Description)
		}
		fmt.Fprintf(command.Stdout, "sha256: %s\n", document.SHA256)
		for _, parameter := range document.Parameters {
			mark := "optional"
			if parameter.Required {
				mark = "required"
			}
			fmt.Fprintf(command.Stdout, "param %s (%s) %s\n", parameter.Name, mark, parameter.Hint)
		}
		fmt.Fprintln(command.Stdout, "---")
		fmt.Fprintln(command.Stdout, document.Body)
		return ExitOK
	default:
		document, loadErr := store.Load(flags.Arg(0))
		if loadErr != nil {
			fmt.Fprintln(command.Stderr, "template render failed:", loadErr)
			return ExitFailed
		}
		values := make(map[string]string, len(paramFlags))
		for _, item := range paramFlags {
			name, value, ok := strings.Cut(item, "=")
			if !ok || strings.TrimSpace(name) == "" {
				fmt.Fprintln(command.Stderr, "template param requires name=value")
				return ExitUsage
			}
			values[strings.TrimSpace(name)] = value
		}
		rendered, renderErr := tools.RenderTemplate(document, values)
		if renderErr != nil {
			fmt.Fprintln(command.Stderr, "template render failed:", renderErr)
			return ExitFailed
		}
		if *jsonOutput {
			return command.writeJSONOutput("template render", struct {
				Name   string `json:"name"`
				SHA256 string `json:"sha256"`
				Text   string `json:"text"`
			}{Name: document.Name, SHA256: document.SHA256, Text: rendered})
		}
		fmt.Fprintln(command.Stdout, rendered)
		return ExitOK
	}
}

// stringListFlag 保存可重复的 --param name=value。
type stringListFlag []string

// String 展示已收集的参数，不进行环境或脚本展开。
func (values *stringListFlag) String() string {
	return strings.Join(*values, ",")
}

// Set 追加一个原样参数值，由渲染入口校验名称。
func (values *stringListFlag) Set(raw string) error {
	*values = append(*values, raw)
	return nil
}
