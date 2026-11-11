// Package main 提供 Cairn CLI 进程入口：建立可取消根上下文，并将两次中断
// 分别映射为优雅取消和强制退出；启动期桥接经 Application 门面完成。
//
// Author: fxbin
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/cli"
)

// main 建立可取消根上下文，并将两次中断分别映射为优雅取消和强制退出。
func main() {
	if handled, code := app.LaunchTrustedEvaluationChild(os.Args[1:]); handled {
		os.Exit(code)
	}
	app.LoadLocalCredentials()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		<-signals
		cancel()
		// 第一次信号由 Loop 持久化为取消；第二次表示不再等待优雅清理。
		<-signals
		os.Exit(130)
	}()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
