// 本文件实现薄 TUI 的 CLI 入口：解析旗标、拦截非 TTY 环境并启动模型配置
// 向导。向导的全部业务语义（目录预填、实测探测、原子落盘）复用应用层
// SetupService；本入口不重复任何校验。
//
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/mattn/go-isatty"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tui"
)

// tuiCommand 解析薄 TUI 旗标并启动模型配置向导：默认生成 version 1 配置，
// 显式给出 --admission 时生成 version 2 身份配置（语义与 provider configure
// 一致）。写入目标默认为用户级配置（一次配置、全部工作区可用，ADR-0016），
// --local 切回工作区。非 TTY 环境失败关闭并指引用 provider configure。
func (command Command) tuiCommand(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	local := flags.Bool("local", false, "write the workspace config instead of the user-level config")
	admission := flags.String("admission", "", "emit a version 2 identity config with this admission semantics (optional|required)")
	catalogURL := flags.String("catalog-url", "", "model metadata catalog URL override")
	catalogCache := flags.String("catalog-cache", "", "model metadata cache file path override")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "tui does not accept positional arguments")
		return ExitUsage
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		fmt.Fprintln(command.Stderr, "tui 需要交互式终端；无 TTY 场景请使用 cairn provider configure")
		return ExitUsage
	}
	input := app.SetupInput{
		Workspace:       *workspace,
		CatalogURL:      *catalogURL,
		CatalogCacheDir: *catalogCache,
		ScopeUserConfig: !*local,
	}
	if *admission != "" {
		if *admission != config.ProviderAdmissionOptional && *admission != config.ProviderAdmissionRequired {
			fmt.Fprintln(command.Stderr, "admission must be optional or required")
			return ExitUsage
		}
		input.ConfigVersion = 2
		input.Admission = *admission
	}
	if err := tui.RunConfigWizard(ctx, input); err != nil {
		fmt.Fprintln(command.Stderr, "tui failed:", err)
		return ExitFailed
	}
	return ExitOK
}
