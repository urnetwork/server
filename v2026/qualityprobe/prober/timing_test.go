package prober

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

type timingAttemptReporter func(context.Context, string, string) error

func (f timingAttemptReporter) ReportAttempt(ctx context.Context, id, failure string) error {
	return f(ctx, id, failure)
}

func timingResult() *egresshealth.Result {
	return &egresshealth.Result{OkCount: 1, Total: 1}
}

func TestProbeTimingPartitionsOneReturnedCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got ProbeTiming
		observed, attempts, closes := 0, 0, 0
		p := &Prober{
			ObserveTiming: func(value ProbeTiming) { observed++; got = value },
			Open: func(context.Context, string) (*http.Client, func() error, error) {
				time.Sleep(2 * time.Second)
				return &http.Client{}, func() error { closes++; time.Sleep(5 * time.Second); return nil }, nil
			},
			Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
				time.Sleep(3 * time.Second)
				return timingResult(), nil
			},
			HealthResults: &stubHealthReporter{},
			Attempts: timingAttemptReporter(func(context.Context, string, string) error {
				attempts++
				time.Sleep(7 * time.Second)
				return nil
			}),
		}
		if err := p.ProbeOne(t.Context(), Provider{ClientId: "synthetic"}); err != nil {
			t.Fatal(err)
		}
		want := ProbeTiming{Open: 2 * time.Second, CheckAndBuffer: 3 * time.Second,
			CloseJoin: 5 * time.Second, AttemptReport: 7 * time.Second, Total: 17 * time.Second}
		if got != want || observed != 1 || attempts != 1 || closes != 1 {
			t.Fatalf("timing or protocol changed: got=%+v observed=%d attempts=%d closes=%d", got, observed, attempts, closes)
		}
	})
}

func TestProbeTimingBlockedCloseDoesNotCompleteOnCancellation(t *testing.T) {
	for _, cancelDuringClose := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "cancel"}[cancelDuringClose], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				entered, release := make(chan struct{}), make(chan struct{})
				var got ProbeTiming
				observed, attempts := 0, 0
				p := &Prober{
					ObserveTiming: func(value ProbeTiming) { observed++; got = value },
					Open: func(context.Context, string) (*http.Client, func() error, error) {
						return &http.Client{}, func() error { close(entered); <-release; return nil }, nil
					},
					Health: answers(timingResult()), HealthResults: &stubHealthReporter{},
					Attempts: timingAttemptReporter(func(context.Context, string, string) error { attempts++; return nil }),
				}
				done := make(chan error, 1)
				go func() { done <- p.ProbeOne(ctx, Provider{ClientId: "synthetic"}) }()
				<-entered
				if cancelDuringClose {
					cancel()
				}
				time.Sleep(11 * time.Second)
				synctest.Wait()
				if observed != 0 || attempts != 0 {
					t.Fatal("blocked cleanup was counted or reported complete")
				}
				select {
				case <-done:
					t.Fatal("probe returned before terminal cleanup")
				default:
				}
				close(release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if observed != 1 || attempts != 1 || got.CloseJoin != 11*time.Second || got.Total != 11*time.Second {
					t.Fatalf("cleanup interval lost or duplicated: %+v observations=%d attempts=%d", got, observed, attempts)
				}
			})
		})
	}
}

func TestProbeTimingErrorAndDisabledObserverPreserveOutcomes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				entered := make(chan struct{})
				closed, observed, reported := 0, 0, ""
				p := &Prober{
					Open: func(context.Context, string) (*http.Client, func() error, error) {
						return &http.Client{}, func() error { closed++; time.Sleep(2 * time.Second); return nil }, nil
					},
					Health: func(ctx context.Context, _ *http.Client, _ egresshealth.Place) (*egresshealth.Result, error) {
						close(entered)
						<-ctx.Done()
						return nil, ctx.Err()
					},
					Attempts: timingAttemptReporter(func(_ context.Context, _, failure string) error { reported = failure; return nil }),
				}
				if enabled {
					p.ObserveTiming = func(value ProbeTiming) {
						observed++
						if value.CloseJoin != 2*time.Second {
							t.Error("cleanup time missing")
						}
					}
				}
				done := make(chan error, 1)
				go func() { done <- p.ProbeOne(ctx, Provider{ClientId: "synthetic"}) }()
				<-entered
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) || reported != FailureHealthNotRun || closed != 1 || observed != map[bool]int{false: 0, true: 1}[enabled] {
					t.Fatalf("observer changed cancellation: err=%v failure=%s close=%d observed=%d", err, reported, closed, observed)
				}
			})
		})
	}
}

func TestProbeTimingOpenFailureAndPanicCoverage(t *testing.T) {
	for _, panics := range []bool{false, true} {
		observed := 0
		failure := errors.New("synthetic open failure")
		p := &Prober{ObserveTiming: func(value ProbeTiming) {
			observed++
			if value.CheckAndBuffer != 0 || value.CloseJoin != 0 {
				t.Error("unopened tunnel invented work")
			}
		}, Open: func(context.Context, string) (*http.Client, func() error, error) {
			if panics {
				panic(failure)
			}
			return nil, nil, failure
		}}
		func() {
			defer func() {
				if recovered := recover(); panics && recovered != failure {
					t.Error("panic changed")
				}
			}()
			if err := p.ProbeOne(t.Context(), Provider{ClientId: "synthetic"}); !errors.Is(err, failure) {
				t.Error("open failure changed")
			}
		}()
		if observed != map[bool]int{false: 1, true: 0}[panics] {
			t.Fatal("panic became completed work or open failure disappeared")
		}
	}
}

func TestProbeTimingConcurrentCallsKeepIndependentRecords(t *testing.T) {
	var mu sync.Mutex
	var values []ProbeTiming
	p := &Prober{Open: okOpen, Health: answers(timingResult()), HealthResults: &stubHealthReporter{},
		ObserveTiming: func(value ProbeTiming) { mu.Lock(); values = append(values, value); mu.Unlock() }}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if err := p.ProbeOne(t.Context(), Provider{ClientId: "synthetic"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(values) != 32 {
		t.Fatal("completed call count changed")
	}
	for _, value := range values {
		if value.Total < 0 || value.Open+value.CheckAndBuffer+value.CloseJoin+value.AttemptReport+value.Other != value.Total {
			t.Fatalf("mixed call timing: %+v", value)
		}
	}
}
