package main

// shared helpers. Per the design, only performance stats go to stdout; all
// simulation logs go to stderr via logf.

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
)

var logLock sync.Mutex

func logf(format string, args ...any) {
	logLock.Lock()
	defer logLock.Unlock()
	fmt.Fprintf(os.Stderr, "[sim-latency] "+format+"\n", args...)
}

func fatalf(format string, args ...any) {
	logf(format, args...)
	os.Exit(1)
}

// signalContext returns a context cancelled on SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
	}()
	return ctx, cancel
}

func absPath(path string) (string, error) {
	return filepath.Abs(path)
}
