// 本文件是首跑配置向导的薄 CLI 入口：交互问答收集三要素后把模型配置
// 语义全部委托给应用层 SetupService（目录预填、零期望探测、能力硬性
// 校验、最小 YAML 与回读自证），本文件只负责提问与结论呈现；TUI/GUI 可
// 复用同一服务。密钥永不落盘——向导只生成引用环境变量名的配置。
//
// Author: fxbin
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fxbin/cairn/internal/app"
)

// defaultProviderKeyEnv 是向导的密钥环境变量名默认值：语义中性，不预设厂
// 商；用户可改。
const defaultProviderKeyEnv = "AGENT_API_KEY"

// initOutcome 承载向导的落盘结果与验证结论，面向 CLI 呈现。
type initOutcome struct {
	ConfigPath     string
	APIKeyEnv      string
	CatalogMatched bool
	CatalogName    string
	ProbeRan       bool
	ProbeNotes     []string
}

// initFlags 收集向导输入的三要素；任何一项缺省时进入交互问答。
type initFlags struct {
	BaseURL      string
	Model        string
	APIKeyEnv    string
	CatalogURL   string
	CatalogCache string
}

// interactivePrompt 在 stdin 上读取一行输入；空输入回车接受默认值。
func interactivePrompt(reader *bufio.Reader, writer io.Writer, label, defaultValue string) string {
	if defaultValue != "" {
		fmt.Fprintf(writer, "%s [%s]: ", label, defaultValue)
	} else {
		fmt.Fprintf(writer, "%s: ", label)
	}
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return defaultValue
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return defaultValue
	}
	return answer
}

// runInit 执行向导主流程：三要素问答 → 委托 SetupService 生成并自证配置
// → 逐条呈现注记。向导期间的全部模型配置语义都在应用层，可被 TUI/GUI
// 以不同交互形态复用。
func (command Command) runInit(ctx context.Context, flags initFlags) (*initOutcome, error) {
	reader := bufio.NewReader(os.Stdin)
	if flags.BaseURL == "" {
		flags.BaseURL = interactivePrompt(reader, command.Stdout, "Provider Base URL", "")
	}
	if flags.Model == "" {
		flags.Model = interactivePrompt(reader, command.Stdout, "模型名（如 glm-5.3-flash）", "")
	}
	if flags.APIKeyEnv == "" {
		flags.APIKeyEnv = interactivePrompt(reader, command.Stdout, "API Key 环境变量名", defaultProviderKeyEnv)
	}
	setup, err := app.NewSetupService().Run(ctx, app.SetupInput{
		BaseURL: flags.BaseURL, Model: flags.Model, APIKeyEnv: flags.APIKeyEnv,
		CatalogURL: flags.CatalogURL, CatalogCacheDir: flags.CatalogCache,
	})
	if err != nil {
		return nil, err
	}
	for _, note := range setup.Notes {
		fmt.Fprintln(command.Stdout, note)
	}
	return &initOutcome{
		ConfigPath: setup.ConfigPath, APIKeyEnv: setup.APIKeyEnv,
		CatalogMatched: setup.CatalogMatched, CatalogName: setup.CatalogName,
		ProbeRan: setup.ProbeRan, ProbeNotes: setup.Notes,
	}, nil
}
