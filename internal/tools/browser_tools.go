// browser_tools.go 将受控 Chrome CDP 驱动暴露为模型工具：
// open/navigate/evaluate/screenshot，默认不注册；无驱动时失败关闭。
// Author: fxbin
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/browser"
	"github.com/fxbin/cairn/internal/ports"
)

// BrowserMaxExpressionBytes 限制求值表达式长度。
const BrowserMaxExpressionBytes = 8 * 1024

// BrowserScreenshotMaxBytes 限制返回给模型的截图 base64 规模。
const BrowserScreenshotMaxBytes = 2 * 1024 * 1024

// BrowserDriver 抽象可注入浏览器后端，便于测试。
type BrowserDriver interface {
	Navigate(ctx context.Context, url string) error
	Evaluate(ctx context.Context, expression string) (string, error)
	Screenshot(ctx context.Context) ([]byte, error)
	Close() error
}

// Browser 依赖显式装配的驱动；未启用时不出现在注册表。
type Browser struct {
	Driver BrowserDriver
}

// SideEffect 声明浏览器导航与求值为进程级副作用（外部页面可产生请求）。
func (Browser) SideEffect() SideEffectClass { return SideEffectProcess }

// Spec 返回 browser 工具的统一入口（action 分发）。
func (tool Browser) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "browser",
		Description: "Control an attached headless Chrome via CDP. Actions: open {url}, eval {expression}, screenshot, close. Opt-in only; requires tools.browser. Evaluate runs page JS and returns JSON text; screenshot returns base64 PNG (may be large).",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["action"],"properties":{"action":{"type":"string","enum":["open","eval","screenshot","close"]},"url":{"type":"string"},"expression":{"type":"string"}}}`),
	}
}

// browserArguments 是 browser 工具入参。
type browserArguments struct {
	Action     string `json:"action"`
	URL        string `json:"url"`
	Expression string `json:"expression"`
}

// browserResult 是 browser 工具稳定数据形状。
type browserResult struct {
	Action     string `json:"action"`
	URL        string `json:"url,omitempty"`
	Value      string `json:"value,omitempty"`
	Screenshot string `json:"screenshot,omitempty"`
	Bytes      int    `json:"bytes,omitempty"`
}

// Execute 分发浏览器动作。
func (tool Browser) Execute(ctx context.Context, raw json.RawMessage) Result {
	var args browserArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return Failure("invalid_arguments", "browser arguments must include action")
	}
	if tool.Driver == nil {
		return Failure("not_configured", "browser driver is not configured")
	}
	switch args.Action {
	case "open":
		if strings.TrimSpace(args.URL) == "" {
			return Failure("invalid_arguments", "url is required for open")
		}
		if err := tool.Driver.Navigate(ctx, args.URL); err != nil {
			return Failure("browser_navigate_failed", err.Error())
		}
		return Success(browserResult{Action: "open", URL: args.URL}, "opened "+args.URL, 0, "", false)
	case "eval":
		if strings.TrimSpace(args.Expression) == "" {
			return Failure("invalid_arguments", "expression is required for eval")
		}
		if len(args.Expression) > BrowserMaxExpressionBytes {
			return Failure("invalid_arguments", "expression exceeds limit")
		}
		value, err := tool.Driver.Evaluate(ctx, args.Expression)
		if err != nil {
			return Failure("browser_eval_failed", err.Error())
		}
		return Success(browserResult{Action: "eval", Value: value}, "evaluated", int64(len(value)), "", false)
	case "screenshot":
		png, err := tool.Driver.Screenshot(ctx)
		if err != nil {
			return Failure("browser_screenshot_failed", err.Error())
		}
		encoded := base64.StdEncoding.EncodeToString(png)
		if len(encoded) > BrowserScreenshotMaxBytes {
			return Failure("screenshot_too_large", fmt.Sprintf("screenshot base64 is %d bytes", len(encoded)))
		}
		return Success(browserResult{Action: "screenshot", Screenshot: encoded, Bytes: len(png)}, "screenshot captured", int64(len(png)), "", false)
	case "close":
		if err := tool.Driver.Close(); err != nil {
			return Failure("browser_close_failed", err.Error())
		}
		return Success(browserResult{Action: "close"}, "closed", 0, "", false)
	default:
		return Failure("invalid_arguments", "unknown browser action")
	}
}

// LaunchBrowserDriver 启动或连接 Chrome 并返回 CDP 驱动。
func LaunchBrowserDriver(ctx context.Context, options browser.CDPOptions) (BrowserDriver, error) {
	if options.DebuggingURL == "" {
		options.Headless = true
	}
	return browser.LaunchChrome(ctx, options)
}

// browserLaunchTimeout 覆盖浏览器冷启动。
var browserLaunchTimeout = 20 * time.Second

// NewBrowserFromChrome 启动 headless Chrome 并返回 Browser 工具实例。
func NewBrowserFromChrome(ctx context.Context, options browser.CDPOptions) (Browser, error) {
	launchCtx, cancel := context.WithTimeout(ctx, browserLaunchTimeout)
	defer cancel()
	driver, err := LaunchBrowserDriver(launchCtx, options)
	if err != nil {
		return Browser{}, err
	}
	return Browser{Driver: driver}, nil
}

// ErrBrowserUnavailable 表示无可用浏览器后端。
var ErrBrowserUnavailable = errors.New("browser backend unavailable")

// LazyBrowserDriver 在首次使用时启动 Chrome，失败关闭。
type LazyBrowserDriver struct {
	Options browser.CDPOptions
	inner   BrowserDriver
}

// Navigate 惰性启动后导航。
func (lazy *LazyBrowserDriver) Navigate(ctx context.Context, url string) error {
	if err := lazy.ensure(ctx); err != nil {
		return err
	}
	return lazy.inner.Navigate(ctx, url)
}

// Evaluate 惰性启动后求值。
func (lazy *LazyBrowserDriver) Evaluate(ctx context.Context, expression string) (string, error) {
	if err := lazy.ensure(ctx); err != nil {
		return "", err
	}
	return lazy.inner.Evaluate(ctx, expression)
}

// Screenshot 惰性启动后截图。
func (lazy *LazyBrowserDriver) Screenshot(ctx context.Context) ([]byte, error) {
	if err := lazy.ensure(ctx); err != nil {
		return nil, err
	}
	return lazy.inner.Screenshot(ctx)
}

// Close 关闭底层驱动。
func (lazy *LazyBrowserDriver) Close() error {
	if lazy.inner == nil {
		return nil
	}
	return lazy.inner.Close()
}

// ensure 启动底层驱动（最多一次）。
func (lazy *LazyBrowserDriver) ensure(ctx context.Context) error {
	if lazy.inner != nil {
		return nil
	}
	driver, err := NewBrowserFromChrome(ctx, lazy.Options)
	if err != nil {
		return err
	}
	lazy.inner = driver.Driver
	return nil
}
