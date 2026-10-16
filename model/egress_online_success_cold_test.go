// Barriers exercise cold admission and publication/reset ordering at the
// shared guard cache used by provider status and inspection.
package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Before any result exists, concurrent callers share the one held read.
func TestProviderOnlineSuccessBaselineColdReadIsCoalesced(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "panic"} {
		published := onlineSuccessTestBaseline(0.36, nil)
		started, release := make(chan struct{}), make(chan struct{})
		completed := make(chan *providerOnlineSuccessBaseline, 1)
		var readCount atomic.Int32
		reads := &providerOnlineSuccessBaselineReads{
			maxAge: providerOnlineSuccessBaselineReadMaxAge,
			now:    func() time.Time { return onlineSuccessTestNow },
			read: func(context.Context, time.Time) (*providerOnlineSuccessBaseline, error) {
				if readCount.Add(1) != 1 {
					return published, nil
				}
				close(started)
				<-release
				if outcome == "failure" {
					return nil, errors.New("synthetic cold read failure")
				}
				if outcome == "panic" {
					panic("synthetic cold read panic")
				}
				return published, nil
			},
			errorf: func(string, ...any) {},
			infof:  func(string, ...any) {},
		}
		go func() { completed <- reads.get(t.Context()) }()
		<-started
		for range 10 {
			if got := reads.get(t.Context()); got != nil {
				t.Errorf("%s: concurrent cold reader got a guard before the first read completed", outcome)
			}
		}
		close(release)
		got := <-completed
		want := published
		if outcome != "success" {
			want = nil
		}
		if got != want || reads.get(t.Context()) != want || readCount.Load() != 1 {
			t.Errorf("%s: result %p, want %p; %d upstream reads", outcome, got, want, readCount.Load())
		}
	}
}

// A publication wins over every older read outcome, both for the reader that
// was in flight and for subsequent callers, without opening a false failure.
func TestProviderOnlineSuccessBaselinePublicationFencesOlderRead(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "panic"} {
		old := onlineSuccessTestBaseline(0.36, nil)
		published := onlineSuccessTestBaseline(0.5, nil)
		started, release := make(chan struct{}), make(chan struct{})
		completed := make(chan *providerOnlineSuccessBaseline, 1)
		var readCount, logCount atomic.Int32
		reads := &providerOnlineSuccessBaselineReads{
			maxAge: providerOnlineSuccessBaselineReadMaxAge,
			now:    func() time.Time { return onlineSuccessTestNow },
			read: func(context.Context, time.Time) (*providerOnlineSuccessBaseline, error) {
				readCount.Add(1)
				close(started)
				<-release
				if outcome == "failure" {
					return nil, errors.New("synthetic superseded read failure")
				}
				if outcome == "panic" {
					panic("synthetic superseded read panic")
				}
				return old, nil
			},
			errorf: func(string, ...any) { logCount.Add(1) },
			infof:  func(string, ...any) { logCount.Add(1) },
		}
		go func() { completed <- reads.get(t.Context()) }()
		<-started
		reads.store(published)
		close(release)
		if got := <-completed; got != published {
			t.Errorf("%s: superseded caller returned %p, want published %p", outcome, got, published)
		}
		if reads.get(t.Context()) != published || readCount.Load() != 1 || logCount.Load() != 0 {
			t.Errorf("%s: publication was replaced or logged as failed; reads=%d logs=%d", outcome, readCount.Load(), logCount.Load())
		}
	}
}

// A pre-reset completion cannot publish into the new lifetime or release its
// read admission while the new lifetime still has an upstream read pending.
func TestProviderOnlineSuccessBaselineResetFencesOlderRead(t *testing.T) {
	firstStarted, firstRelease := make(chan struct{}), make(chan struct{})
	secondStarted, secondRelease := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan *providerOnlineSuccessBaseline, 1), make(chan *providerOnlineSuccessBaseline, 1)
	old, published := onlineSuccessTestBaseline(0.36, nil), onlineSuccessTestBaseline(0.5, nil)
	var readCount atomic.Int32
	reads := &providerOnlineSuccessBaselineReads{
		maxAge: providerOnlineSuccessBaselineReadMaxAge,
		now:    func() time.Time { return onlineSuccessTestNow },
		read: func(context.Context, time.Time) (*providerOnlineSuccessBaseline, error) {
			switch readCount.Add(1) {
			case 1:
				close(firstStarted)
				<-firstRelease
				return old, nil
			case 2:
				close(secondStarted)
				<-secondRelease
			}
			return published, nil
		},
		errorf: func(string, ...any) {},
		infof:  func(string, ...any) {},
	}
	go func() { firstDone <- reads.get(t.Context()) }()
	<-firstStarted
	reads.reset()
	go func() { secondDone <- reads.get(t.Context()) }()
	<-secondStarted
	close(firstRelease)
	if got := <-firstDone; got != nil {
		t.Errorf("pre-reset caller returned %p after reset", got)
	}
	if got := reads.get(t.Context()); got != nil || readCount.Load() != 2 {
		t.Errorf("old completion released new read admission: guard=%p reads=%d", got, readCount.Load())
	}
	close(secondRelease)
	if got := <-secondDone; got != published || reads.get(t.Context()) != published {
		t.Error("new lifetime did not keep its own guard")
	}
}
