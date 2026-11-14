package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

// The old sweep spawned one goroutine per returned provider and put the
// semaphore inside each goroutine. A 5,000-row batch therefore retained 5,000
// goroutines while only a few did work; after cancellation every queued
// goroutine still took a turn and constructed a doomed tunnel. This test holds
// the first worker open, cancels the pass, and proves no queued provider begins.
func TestBlackholeSweepDoesNotQueueOneGoroutinePerProvider(t *testing.T) {
	ids := make([]string, 500)
	for i := range ids {
		ids[i] = "provider"
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/network/provider-blackhole-due" {
			_ = json.NewEncoder(w).Encode(map[string]any{"client_ids": ids})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s := &blackholeSweeper{
		operator:    &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "secret", Http: srv.Client()},
		pins:        &pinSet{pins: map[string][]string{"source.invalid": {"leaf", "intermediate"}}},
		timeout:     time.Second,
		concurrency: 1,
		limit:       len(ids),
		checkOneFn: func(context.Context, string) blackholeResult {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			return blackholeResult{}
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.sweep(ctx)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first worker did not start")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweep did not stop after cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d provider checks began, want 1: canceled work was already queued in per-provider goroutines", got)
	}
}
