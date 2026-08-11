// This file verifies the optional connection-announcement lifecycle observer
// used by integration fixtures to join deferred cleanup before schema removal.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
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
	defer announce.Close()
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
