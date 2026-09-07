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
	"sync/atomic"
	"syscall"
)

var logLock sync.Mutex
var logEvaluationId atomic.Pointer[string]

func setLogEvaluationId(evaluationId string) {
	if evaluationId == "" {
		logEvaluationId.Store(nil)
		return
	}
	value := evaluationId
	logEvaluationId.Store(&value)
}

func logf(format string, args ...any) {
	logLock.Lock()
	defer logLock.Unlock()
	prefix := "[sim-latency]"
	if evaluationId := logEvaluationId.Load(); evaluationId != nil {
		prefix = fmt.Sprintf("[sim-latency eval=%s]", *evaluationId)
	}
	fmt.Fprintf(os.Stderr, prefix+" "+format+"\n", args...)
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
