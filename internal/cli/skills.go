// Package cli 提供本地技能管理命令，JSON 模式每条记录独占一行。
// Author: fxbin
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/tools"
)

// skillsCommand 管理显式本地技能包；安装不执行脚本，也不隐式改写运行配置。
func (command Command) skillsCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "skills requires list, show, install, remove, enable or disable")
		return ExitUsage
	}
	action := arguments[0]
	switch action {
	case "list", "show", "install", "remove", "enable", "disable":
	default:
		fmt.Fprintln(command.Stderr, "unknown skills subcommand")
		return ExitUsage
	}
	flags := flag.NewFlagSet("skills "+action, flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace root (defaults to current directory)")
	scope := flags.String("scope", "workspace", "management target: workspace or user")
	shared := flags.Bool("shared", false, "include cross-client .agents/skills in list and show")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeFlagsThenPositionals(arguments[1:], map[string]struct{}{"--workspace": {}, "--scope": {}})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if action == "list" && flags.NArg() != 0 || action != "list" && flags.NArg() != 1 || *scope != "workspace" && *scope != "user" {
		return ExitUsage
	}
	if ctx.Err() != nil {
		return ExitCancelled
	}
	if *workspace == "" {
		path, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(command.Stderr, "resolve workspace failed")
			return ExitFailed
		}
		*workspace = path
	}
	if action == "list" || action == "show" {
		catalog, err := command.Service.Skills(*workspace, *shared)
		if err != nil {
			fmt.Fprintln(command.Stderr, "skills discovery failed:", err)
			return ExitFailed
		}
		for _, diagnostic := range catalog.Diagnostics {
			fmt.Fprintf(command.Stderr, "skills: %q (%s): %s\n", diagnostic.Name, diagnostic.Scope, diagnostic.Code)
		}
		if action == "list" {
			for _, entry := range catalog.Entries() {
				if *jsonOutput {
					if writeJSONChecked(command.Stdout, entry) != nil {
						return ExitFailed
					}
				} else {
					fmt.Fprintln(command.Stdout, tools.SkillSummary(entry))
				}
			}
			return ExitOK
		}
		content, err := catalog.Inspect(flags.Arg(0))
		if err != nil {
			fmt.Fprintln(command.Stderr, "skills show failed:", err)
			return ExitFailed
		}
		content.Instructions = string(artifact.Redact([]byte(content.Instructions)))
		for index := range content.Resources {
			content.Resources[index].Path = string(artifact.Redact([]byte(content.Resources[index].Path)))
		}
		if *jsonOutput {
			if writeJSONChecked(command.Stdout, content) != nil {
				return ExitFailed
			}
		} else {
			fmt.Fprintf(command.Stdout, "%s (%s)\n%s\n\n%s\n", content.Name, content.Scope, content.Description, content.Instructions)
			for _, resource := range content.Resources {
				fmt.Fprintf(command.Stdout, "%s\t%d bytes\n", resource.Path, resource.Bytes)
			}
		}
		return ExitOK
	}
	store, err := command.Service.SkillStoreFor(*workspace, *scope)
	if err != nil {
		fmt.Fprintln(command.Stderr, "skills target failed:", err)
		return ExitFailed
	}
	name := flags.Arg(0)
	switch action {
	case "install":
		name, err = store.Install(name)
	case "remove":
		err = store.Remove(name)
	case "enable", "disable":
		err = store.SetEnabled(name, action == "enable")
	}
	if err != nil {
		fmt.Fprintln(command.Stderr, "skills "+action+" failed:", err)
		return ExitFailed
	}
	report := struct {
		Action  string `json:"action"`
		Name    string `json:"name"`
		Scope   string `json:"scope"`
		Success bool   `json:"success"`
	}{Action: action, Name: string(artifact.Redact([]byte(name))), Scope: *scope, Success: true}
	if *jsonOutput {
		if writeJSONChecked(command.Stdout, report) != nil {
			return ExitFailed
		}
	} else {
		fmt.Fprintf(command.Stdout, "skills %s: %s (%s)\n", action, report.Name, *scope)
	}
	return ExitOK
}
