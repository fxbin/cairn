// Package cli 提供声明式内容包管理命令；安装不执行脚本、不授予权限。
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

// packageCommand 管理本地内容包：inspect/install/list/enable/disable。
func (command Command) packageCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "package requires inspect, install, list, enable or disable")
		return ExitUsage
	}
	action := arguments[0]
	switch action {
	case "inspect", "install", "list", "enable", "disable":
	default:
		fmt.Fprintln(command.Stderr, "unknown package subcommand")
		return ExitUsage
	}
	flags := flag.NewFlagSet("package "+action, flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	root := flags.String("root", "", "package store directory (defaults to ~/.cairn/packages)")
	source := flags.String("source", "", "local package source directory")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeFlagsThenPositionals(arguments[1:], map[string]struct{}{"--root": {}, "--source": {}})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if ctx.Err() != nil {
		return ExitCancelled
	}
	storeRoot := strings.TrimSpace(*root)
	if storeRoot == "" {
		path, err := userdir.Path("packages")
		if err != nil {
			fmt.Fprintln(command.Stderr, "resolve package root failed")
			return ExitFailed
		}
		storeRoot = path
	}
	switch action {
	case "inspect":
		sourcePath := flags.Arg(0)
		if sourcePath == "" {
			sourcePath = strings.TrimSpace(*source)
		}
		if sourcePath == "" {
			fmt.Fprintln(command.Stderr, "package inspect requires source directory")
			return ExitUsage
		}
		manifest, err := tools.InspectPackage(sourcePath)
		if err != nil {
			fmt.Fprintln(command.Stderr, "package inspect failed:", err)
			return ExitFailed
		}
		if *jsonOutput {
			if writeJSONChecked(command.Stdout, manifest) != nil {
				return ExitFailed
			}
			return ExitOK
		}
		fmt.Fprintf(command.Stdout, "name: %s\nversion: %s\nresources: %d\n", manifest.Name, manifest.Package, len(manifest.Resources))
		return ExitOK
	case "install":
		sourcePath := flags.Arg(0)
		if sourcePath == "" {
			sourcePath = strings.TrimSpace(*source)
		}
		if sourcePath == "" {
			fmt.Fprintln(command.Stderr, "package install requires source directory")
			return ExitUsage
		}
		store, err := tools.NewPackageStore(storeRoot)
		if err != nil {
			fmt.Fprintln(command.Stderr, "package store failed:", err)
			return ExitFailed
		}
		entry, err := store.InstallPackage(sourcePath)
		if err != nil {
			fmt.Fprintln(command.Stderr, "package install failed:", err)
			return ExitFailed
		}
		if *jsonOutput {
			if writeJSONChecked(command.Stdout, entry) != nil {
				return ExitFailed
			}
			return ExitOK
		}
		fmt.Fprintln(command.Stdout, tools.PackageSummary(entry))
		return ExitOK
	case "list":
		store, err := tools.NewPackageStore(storeRoot)
		if err != nil {
			fmt.Fprintln(command.Stderr, "package store failed:", err)
			return ExitFailed
		}
		items, err := store.ListPackages()
		if err != nil {
			fmt.Fprintln(command.Stderr, "package list failed:", err)
			return ExitFailed
		}
		for _, item := range items {
			if *jsonOutput {
				if writeJSONChecked(command.Stdout, item) != nil {
					return ExitFailed
				}
			} else {
				fmt.Fprintln(command.Stdout, tools.PackageSummary(item))
			}
		}
		return ExitOK
	default:
		name := flags.Arg(0)
		if name == "" {
			fmt.Fprintln(command.Stderr, "package "+action+" requires name")
			return ExitUsage
		}
		store, err := tools.NewPackageStore(storeRoot)
		if err != nil {
			fmt.Fprintln(command.Stderr, "package store failed:", err)
			return ExitFailed
		}
		if err := store.SetPackageEnabled(name, action == "enable"); err != nil {
			fmt.Fprintln(command.Stderr, "package "+action+" failed:", err)
			return ExitFailed
		}
		if *jsonOutput {
			return command.writeJSONOutput("package "+action, struct {
				Name    string `json:"name"`
				Enabled bool   `json:"enabled"`
			}{Name: name, Enabled: action == "enable"})
		}
		fmt.Fprintf(command.Stdout, "package %s: %s\n", action, name)
		return ExitOK
	}
}
