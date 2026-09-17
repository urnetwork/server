// This file verifies the optional connection-announcement lifecycle observer
// used by integration fixtures to join deferred cleanup before schema removal.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Lifecycle completion is reported after a canceled announcement returns.
func TestConnectionAnnounceLifecycleObserver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := make(chan struct{})
	done := make(chan struct{})
	settings := DefaultConnectionAnnounceSettings()
	settings.LifecycleStarted = func() {
		close(started)
	}
	settings.LifecycleDone = func() {
		close(done)
	}
	announce := NewConnectionAnnounce(
		ctx,
		cancel,
		server.NewId(),
		server.NewId(),
		"127.0.0.1:1",
		server.NewId(),
		time.Second,
		V0TestConfig(),
		settings,
	)
	defer announce.CloseAndWait()
	select {
	case <-started:
	default:
		t.Fatal("connection announcement did not report synchronous lifecycle admission")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection announcement did not report lifecycle completion")
	}
}

// Joining close does not return until the root lifecycle callback has
// completed, which makes handler idle a valid model-cleanup boundary.
func TestConnectHandlerFinishConnectionAnnounceJoinsLifecycleCompletion(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	ctx, cancel := context.WithCancel(context.Background())
	lifecycleStarted := make(chan struct{})
	lifecycleDoneEntered := make(chan struct{})
	releaseLifecycleDone := make(chan struct{})
	settings := DefaultConnectionAnnounceSettings()
	settings.LifecycleStarted = func() {
		close(lifecycleStarted)
	}
	settings.LifecycleDone = func() {
		close(lifecycleDoneEntered)
		<-releaseLifecycleDone
	}
	announce := NewConnectionAnnounce(
		ctx,
		cancel,
		server.NewId(),
		server.NewId(),
		"127.0.0.1:1",
		server.NewId(),
		time.Hour,
		V0TestConfig(),
		settings,
	)
	select {
	case <-lifecycleStarted:
	case <-testCtx.Done():
		t.Fatal("connection announcement did not report lifecycle admission")
	}

	closeDone := make(chan struct{})
	go func() {
		finishConnectionAnnounce(announce)
		close(closeDone)
	}()
	select {
	case <-lifecycleDoneEntered:
	case <-testCtx.Done():
		close(releaseLifecycleDone)
		t.Fatal("connection announcement did not enter final lifecycle callback")
	}
	select {
	case <-closeDone:
		close(releaseLifecycleDone)
		t.Fatal("joining close returned before lifecycle completion")
	default:
	}

	close(releaseLifecycleDone)
	select {
	case <-closeDone:
	case <-testCtx.Done():
		t.Fatal("joining close did not observe lifecycle completion")
	}
}

// Child shutdown closes admission and waits for an exact held worker before
// publishing lifecycle completion.
func TestConnectionAnnounceChildWorkersCloseAdmissionAndJoin(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	waitEntered := make(chan struct{})
	announce := &ConnectionAnnounce{
		cancel: func() {},
		beforeWorkersWaitForTest: func() {
			close(waitEntered)
		},
	}
	if !announce.startWorker(func() {
		close(workerStarted)
		<-releaseWorker
	}) {
		t.Fatal("open announcement rejected child worker")
	}
	select {
	case <-workerStarted:
	case <-testCtx.Done():
		close(releaseWorker)
		t.Fatal("child worker did not reach hold barrier")
	}

	waitDone := make(chan struct{})
	go func() {
		announce.closeWorkersAndWait()
		close(waitDone)
	}()
	select {
	case <-waitEntered:
	case <-testCtx.Done():
		close(releaseWorker)
		t.Fatal("child lifecycle did not close admission")
	}
	if announce.startWorker(func() {}) {
		close(releaseWorker)
		t.Fatal("closing announcement admitted a new child worker")
	}
	select {
	case <-waitDone:
		close(releaseWorker)
		t.Fatal("child lifecycle returned before held worker")
	default:
	}

	close(releaseWorker)
	select {
	case <-waitDone:
	case <-testCtx.Done():
		t.Fatal("child lifecycle did not join held worker")
	}
}
