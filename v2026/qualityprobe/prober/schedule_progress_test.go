// Actual worker lifecycle is visible before the batch joins; no network,
// sleeps or provider identity in an observation is needed to prove ordering.
package prober

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestSchedulerProgressSpansOpenAndTeardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		openRelease, closeRelease := make(chan struct{}), make(chan struct{})
		var stateLock sync.Mutex
		var events []Progress
		scheduler := &Scheduler{
			Concurrency: 1,
			Prober: &Prober{
				Open: func(context.Context, string) (*http.Client, func() error, error) {
					<-openRelease
					return &http.Client{}, func() error { <-closeRelease; return nil }, nil
				},
				Health: answers(exitResult()), Submit: &stubSubmitter{},
			},
			ObserveProgress: func(event Progress) {
				stateLock.Lock()
				defer stateLock.Unlock()
				events = append(events, event)
			},
		}
		check := func(want []Progress) {
			t.Helper()
			stateLock.Lock()
			defer stateLock.Unlock()
			if !slices.Equal(events, want) {
				t.Errorf("worker progress=%v want=%v", events, want)
			}
		}
		done := make(chan Summary, 1)
		go func() { done <- scheduler.Run(context.Background(), providersOf("synthetic-worker")) }()
		synctest.Wait()
		check([]Progress{ProbeStarted})
		close(openRelease)
		synctest.Wait()
		check([]Progress{ProbeStarted})
		close(closeRelease)
		summary := <-done
		check([]Progress{ProbeStarted, ProbeFinished})
		if summary.Attempted != 1 || summary.Submitted != 1 {
			t.Errorf("progress changed summary: %+v", summary)
		}
	})
}

func TestSchedulerProgressDistinguishesFailureFromNonAdmission(t *testing.T) {
	var events []Progress
	scheduler := &Scheduler{
		Concurrency: 1,
		Prober: &Prober{
			Open: func(context.Context, string) (*http.Client, func() error, error) {
				return nil, nil, errors.New("synthetic open failure")
			},
			Health: answers(exitResult()), Submit: &stubSubmitter{},
		},
		ObserveProgress: func(event Progress) { events = append(events, event) },
	}
	summary := scheduler.Run(context.Background(), providersOf("synthetic-failed", "synthetic-failed"))
	if summary.Attempted != 1 || summary.Failed != 1 || summary.Skipped != 1 || !slices.Equal(events, []Progress{ProbeStarted, ProbeFinished}) {
		t.Fatalf("failed/duplicate progress: %+v events=%v", summary, events)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events = nil
	summary = scheduler.Run(ctx, providersOf("synthetic-canceled"))
	if summary.Attempted != 0 || summary.Skipped != 1 || len(events) != 0 {
		t.Fatalf("pre-admission cancellation fabricated activity: %+v events=%v", summary, events)
	}
	scheduler.CacheTtl = time.Hour
	scheduler.markProbed("synthetic-cached")
	summary = scheduler.Run(context.Background(), providersOf("synthetic-cached"))
	if summary.Attempted != 0 || summary.Skipped != 1 || len(events) != 0 {
		t.Fatalf("cached provider fabricated activity: %+v events=%v", summary, events)
	}
}
