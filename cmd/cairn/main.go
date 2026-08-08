package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/fxbin/cairn/internal/cli"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		<-signals
		cancel()
		// The first signal is persisted by the Loop. A second interrupt is an
		// explicit request to stop waiting for graceful cleanup.
		<-signals
		os.Exit(130)
	}()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
