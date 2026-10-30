// Cairn GUI 桌面入口：Wails v3 应用壳，绑定 internal/desktop.API。
//
// Author: fxbin
package main

import (
	"context"
	"embed"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/desktop"
)

//go:embed all:frontend/dist
var assets embed.FS

// runEventName 是 Wails 事件通道名。
const runEventName = "cairn:stream"

// defaultServerPort 是 server 模式（-tags server 编译）的默认监听端口。
//
// 与 wails3 的默认值一致，容器镜像对外暴露的也是这个端口。
const defaultServerPort = 8080

// serverPortEnv 是 server 模式监听端口的环境变量名。
const serverPortEnv = "CAIRN_GUI_PORT"

// serverPort 返回 server 模式应监听的端口。
//
// 取值来自 CAIRN_GUI_PORT；未设置或取值非法时回落到默认端口。端口只影响
// 监听地址，不改变任何运行事实，非法取值按默认值处理而不是启动失败。
func serverPort() int {
	raw := strings.TrimSpace(os.Getenv(serverPortEnv))
	if raw == "" {
		return defaultServerPort
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 || port > 65535 {
		log.Printf("%s 取值 %q 非法，回落默认端口 %d", serverPortEnv, raw, defaultServerPort)
		return defaultServerPort
	}
	return port
}

func main() {
	prefs, err := desktop.NewPrefsStore()
	if err != nil {
		log.Println("preferences store unavailable:", err)
	}
	api := desktop.NewAPI(app.Service{}, prefs, nil)

	wailsApp := application.New(application.Options{
		Name:        "Cairn",
		Description: "Cairn — 事件溯源编码 Agent 桌面端",
		Services: []application.Service{
			application.NewService(desktop.NewService(api)),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		// server 模式（-tags server）才使用这段配置：原生窗口不受影响。
		// 监听主机保持 wails3 的默认（仅本机），只让端口可配。
		Server: application.ServerOptions{Port: serverPort()},
	})

	// 事件出口：推送 StreamEvent 到前端；会话隔离由 desktop 层保证。
	api.AttachEmitter(func(event desktop.StreamEvent) {
		wailsApp.Event.Emit(runEventName, event)
	})

	// 原生目录选择：仅返回用户选中的目录，不授予任意文件读写。
	api.AttachDirectoryPicker(func(title, defaultPath string) (string, error) {
		picker := wailsApp.Dialog.OpenFile().
			CanChooseFiles(false).
			CanChooseDirectories(true).
			CanCreateDirectories(true)
		if title != "" {
			picker = picker.SetTitle(title)
		}
		if defaultPath != "" {
			picker = picker.SetDirectory(defaultPath)
		}
		return picker.PromptForSingleSelection()
	})

	window := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Cairn",
		Width:  1280,
		Height: 800,
		// 最小尺寸对齐桌面端布局验收的窄窗口口径（720×480）：低于它三栏布局
		// 的检查器与输入区会互相遮挡，最小尺寸因此就是布局下界的声明。
		MinWidth:  720,
		MinHeight: 480,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			TitleBar:                application.MacTitleBarHiddenInset,
			Backdrop:                application.MacBackdropTranslucent,
		},
		BackgroundColour: application.NewRGB(15, 17, 21),
		URL:              "/",
	})

	// 注入宿主标记，供前端区分真实 Wails 与演示桥；不授予额外权限。
	window.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
		window.ExecJS(`window.__CAIRN_HOST__ = "wails";`)
	})

	// 退出有界收尾：取消运行并等待 Journal 对齐，不留下失控任务。
	wailsApp.OnShutdown(func() {
		if err := api.Shutdown(5 * time.Second); err != nil {
			log.Println("shutdown:", err)
		}
	})

	if err := wailsApp.Run(); err != nil {
		log.Println("cairn gui failed:", err)
		os.Exit(1)
	}
	_ = context.Background()
}
