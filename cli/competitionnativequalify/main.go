//go:build linux

// A separate qualification-only binary; it is never installed or selected by
// the production evaluator merely because source preparation built it.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server/competitionagent"
)

// Finite operations retain signal cancellation and emit only bounded facts.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()
	if err := competitionagent.RunNativeQualification(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "native qualification operation incomplete")
		os.Exit(1)
	}
}
