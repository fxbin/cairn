package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/fxbin/cairn/internal/cli"
)

// main 建立可取消根上下文，并将两次中断分别映射为优雅取消和强制退出。
func main() {
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
