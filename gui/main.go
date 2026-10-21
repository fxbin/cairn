// Cairn GUI 桌面入口：Wails v3 应用壳，绑定 internal/desktop.API。
//
// Author: fxbin
package main

import (
	"context"
	"embed"
	"log"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"

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

	window := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "Cairn",
		Width:     1280,
		Height:    800,
		MinWidth:  960,
		MinHeight: 640,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			TitleBar:                application.MacTitleBarHiddenInset,
			Backdrop:                application.MacBackdropTranslucent,
		},
		BackgroundColour: application.NewRGB(15, 17, 21),
		URL:              "/",
	})
	_ = window

	if err := wailsApp.Run(); err != nil {
		log.Println("cairn gui failed:", err)
		os.Exit(1)
	}
	_ = context.Background()
}
