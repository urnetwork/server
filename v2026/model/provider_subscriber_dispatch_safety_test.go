package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestSubscriberDispatchSlowQueryAndFailedReadDoNotCache(t *testing.T) {
	clock, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	_, err := c.lookup(t.Context(), []server.Id{id}, func(_ context.Context, _ []server.Id, dispatch func()) (map[server.Id]bool, error) {
		clock.Add((2 * time.Second).Nanoseconds())
		dispatch()
		clock.Add(time.Second.Nanoseconds())
		return map[server.Id]bool{id: true}, nil
	})
	if err != nil || len(c.negative) != 0 || len(c.flights) != 0 {
		t.Fatal("dispatch clock hid a fact query past the freshness bound", err)
	}
	want := errors.New("synthetic fact-read failure")
	_, err = c.lookup(t.Context(), []server.Id{id}, func(_ context.Context, _ []server.Id, dispatch func()) (map[server.Id]bool, error) {
		dispatch()
		return map[server.Id]bool{id: true}, want
	})
	if !errors.Is(err, want) || len(c.negative) != 0 || len(c.flights) != 0 {
		t.Fatal("failed dispatched read cached a partial result", err)
	}
}

func TestSubscriberDispatchResetKeepsNewFlightOwned(t *testing.T) {
	_, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	oldStarted, oldRelease := make(chan struct{}), make(chan struct{})
	newStarted, newRelease := make(chan struct{}), make(chan struct{})
	oldDone, newDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.lookup(t.Context(), []server.Id{id}, func(_ context.Context, _ []server.Id, dispatch func()) (map[server.Id]bool, error) {
			dispatch()
			close(oldStarted)
			<-oldRelease
			return map[server.Id]bool{id: true}, nil
		})
		oldDone <- err
	}()
	<-oldStarted
	c.reset()
	go func() {
		_, err := c.lookup(t.Context(), []server.Id{id}, func(_ context.Context, _ []server.Id, dispatch func()) (map[server.Id]bool, error) {
			dispatch()
			close(newStarted)
			<-newRelease
			return map[server.Id]bool{id: true}, nil
		})
		newDone <- err
	}()
	<-newStarted
	c.mu.Lock()
	current := c.flights[id]
	c.mu.Unlock()
	close(oldRelease)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	valid := current != nil && c.flights[id] == current && len(c.negative) == 0
	c.mu.Unlock()
	if !valid {
		t.Fatal("old dispatched read deleted or published over the new policy flight")
	}
	close(newRelease)
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}
	if len(c.flights) != 0 || len(c.negative) != 1 {
		t.Fatal("new policy flight failed to publish its own bounded negative")
	}
}
