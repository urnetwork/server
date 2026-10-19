package model

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The first owner waits for a connection before any fact is read. Every
// follower is already coalesced before that wait ends. A ten-millisecond fact
// read remains fresh even after a two-second acquisition queue.
func TestSubscriberDispatchQueuedReadRetainsFreshNegativeForFollowers(t *testing.T) {
	clock, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	queued, release, followers := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var waiting, reads atomic.Int32
	c.observe = func(event string, count int) {
		if event == "coalesced_wait" && waiting.Add(int32(count)) == 63 {
			close(followers)
		}
	}
	read := func(_ context.Context, _ []server.Id, dispatch func()) (map[server.Id]bool, error) {
		if reads.Add(1) == 1 {
			close(queued)
			<-release
			dispatch()
			clock.Add((10 * time.Millisecond).Nanoseconds())
		}
		return map[server.Id]bool{id: true}, nil
	}
	type outcome struct {
		excluded bool
		err      error
	}
	completed := make(chan outcome, 64)
	lookup := func() {
		got, err := negativeDispatchLookup(c, t.Context(), []server.Id{id}, read)
		completed <- outcome{excluded: got[id], err: err}
	}
	go lookup()
	<-queued
	for range 63 {
		go lookup()
	}
	<-followers
	clock.Add((2 * time.Second).Nanoseconds())
	close(release)
	for range 64 {
		result := <-completed
		if result.err != nil || !result.excluded {
			t.Fatal("queued negative result lost", result.err)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("fresh negative multiplied fact reads after connection wait: got %d want 1", reads.Load())
	}
}

// A later chunk cannot refresh the observation clock of an earlier negative.
// Cache age includes fact-query time and remains exactly one second from the
// first dispatch, irrespective of when the complete batch returns.
func TestSubscriberDispatchFirstFactBoundsMultiChunkAge(t *testing.T) {
	clock, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	reads := 0
	read := func(_ context.Context, _ []server.Id, dispatch func()) (map[server.Id]bool, error) {
		reads++
		if reads == 1 {
			clock.Add((2 * time.Second).Nanoseconds())
			dispatch()
			clock.Add((900 * time.Millisecond).Nanoseconds())
			dispatch() // A second SQL chunk cannot extend the first fact's age.
			return map[server.Id]bool{id: true}, nil
		}
		return nil, nil
	}
	for range 2 {
		got, err := negativeDispatchLookup(c, t.Context(), []server.Id{id}, read)
		if err != nil || !got[id] {
			t.Fatal("fresh dispatch-bound negative missing", err)
		}
	}
	if reads != 1 {
		t.Fatal("queued fresh result did not survive until its fact deadline", reads)
	}
	clock.Add((100 * time.Millisecond).Nanoseconds())
	got, err := negativeDispatchLookup(c, t.Context(), []server.Id{id}, read)
	if err != nil || got[id] || reads != 2 {
		t.Fatal("negative exceeded one second from first dispatch", err)
	}
}
