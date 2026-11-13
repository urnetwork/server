package task

import (
	"context"
	"testing"
	"time"
)

// A blocked journal writer used to prevent the cancel stage from running.
// The test holds one synthetic run open so Drain must reach that stage.
func TestDrainCancellationDoesNotWaitForLogSink(t *testing.T) {
	settings := DefaultTaskWorkerSettings()
	settings.DrainFinishTimeout = 20 * time.Millisecond
	settings.DrainCancelTimeout = 20 * time.Millisecond
	worker := NewTaskWorker(context.Background(), settings)
	defer worker.cancel()
	worker.runWg.Add(1)
	defer worker.runWg.Done()
	releaseLog := make(chan struct{})
	defer close(releaseLog)
	logEntered := make(chan struct{}, 4)
	worker.drainLogf = func(string, ...any) {
		logEntered <- struct{}{}
		<-releaseLog
	}

	drained := make(chan struct{})
	go func() {
		worker.Drain()
		close(drained)
	}()
	select {
	case <-worker.drainCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("drain cancellation waited for a blocked log sink")
	}
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain completion waited for a blocked log sink")
	}
	select {
	case <-logEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic blocked drain log was never exercised")
	}
}
