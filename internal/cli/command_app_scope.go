// [INPUT]: 依赖 flag、作用域配置与 CLI 参数顺序规范化。
// [OUTPUT]: 为持久任务和工作流解析统一的工作区、状态根与输出旗标。
// [POS]: CLI 参数适配。配置由既有解析入口读取。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件集中跨会话命令的作用域参数。
// Author: fxbin
package cli

import (
	"flag"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/config"
)

// appCommandFlags 保存单个子命令的作用域旗标和参数集。
type appCommandFlags struct {
	*flag.FlagSet
	workspace  string
	stateDir   string
	configPath string
	jsonOutput bool
}

// appFlags 注册公共作用域和输出参数，并在帮助中显示子命令语法。
func (command Command) appFlags(label, signature string) *appCommandFlags {
	flags := &appCommandFlags{FlagSet: flag.NewFlagSet(label, flag.ContinueOnError)}
	flags.SetOutput(command.Stderr)
	flags.StringVar(&flags.workspace, "workspace", "", "workspace path (default: current directory)")
	flags.StringVar(&flags.stateDir, "state-dir", "", "state directory")
	flags.StringVar(&flags.configPath, "config", "", "configuration path")
	flags.BoolVar(&flags.jsonOutput, "json", false, "write NDJSON to stdout")
	flags.Usage = func() {
		fmt.Fprintf(command.Stderr, "Usage: cairn %s %s\n", label, signature)
		flags.PrintDefaults()
	}
	return flags
}

// parse 允许命令参数与旗标交错。已登记旗标决定哪些值与前项相邻。
func (flags *appCommandFlags) parse(arguments []string, count int) bool {
	valueFlags := make(map[string]struct{})
	flags.VisitAll(func(option *flag.Flag) {
		if boolean, ok := option.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			return
		}
		valueFlags["-"+option.Name] = struct{}{}
		valueFlags["--"+option.Name] = struct{}{}
	})
	if err := flags.Parse(scopedArguments(arguments, valueFlags)); err != nil {
		return false
	}
	if flags.NArg() == count {
		for _, argument := range flags.Args() {
			if strings.TrimSpace(argument) == "" {
				flags.Usage()
				return false
			}
		}
		return true
	}
	flags.Usage()
	return false
}

// scopedArguments 保留互相交错的旗标和位置参数，以及 -- 后的字面参数。
func scopedArguments(arguments []string, valueFlags map[string]struct{}) []string {
	flags := make([]string, 0, len(arguments))
	positionals := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			positionals = append(positionals, arguments[index+1:]...)
			break
		}
		if argument == "-" || !strings.HasPrefix(argument, "-") {
			positionals = append(positionals, argument)
			continue
		}
		flags = append(flags, argument)
		if _, consumesValue := valueFlags[argument]; consumesValue {
			if index+1 == len(arguments) {
				return flags
			}
			index++
			flags = append(flags, arguments[index])
		}
	}
	return append(append(flags, "--"), positionals...)
}

// loadAppConfiguration 复用 CLI 配置装配，为跨会话命令加载有效配置。
func (command Command) loadAppConfiguration(flags *appCommandFlags) (config.Config, error) {
	return command.loadConfig(flags.workspace, flags.configPath, config.Overrides{})
}
