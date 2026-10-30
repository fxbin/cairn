// Cairn GUI 桌面入口：Wails v3 应用壳，绑定 internal/desktop.API。
//
// Author: fxbin
package main

import (
	"context"
	"embed"
	"log"
	"os"
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
