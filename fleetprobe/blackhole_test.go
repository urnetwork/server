package fleetprobe

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/ingest"
)

func testPins() map[string][]string {
	return map[string][]string{"source.invalid": {"leaf", "intermediate"}}
}

func TestRunBlackholeUsesFixedWorkerCount(t *testing.T) {
	const concurrency = 3
	started := make(chan struct{}, 100)
	release := make(chan struct{})
	providerClientIds := make([]string, 100)
	for index := range providerClientIds {
		providerClientIds[index] = fmt.Sprintf("provider-%d", index)
	}

	done := make(chan error, 1)
	go func() {
		_, err := RunBlackhole(context.Background(), providerClientIds, BlackholeOptions{
			Pins:        testPins,
			Timeout:     time.Second,
			Concurrency: concurrency,
			CheckOne: func(_ context.Context, providerClientId string) BlackholeResult {
				started <- struct{}{}
				<-release
				return BlackholeResult{Check: ingest.BlackholeCheck{
					ClientId:  providerClientId,
					OK:        true,
					CheckedAt: time.Unix(1, 0).UTC(),
				}}
			},
		})
		done <- err
	}()

	for range concurrency {
		<-started
	}
	select {
	case <-started:
		t.Fatal("a fourth provider started while all three workers were blocked")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RunBlackhole: %v", err)
	}
}

func TestRunBlackholeDoesNotStartAdmittedWorkAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = RunBlackhole(ctx, []string{"first", "second", "third"}, BlackholeOptions{
			Pins:        testPins,
			Timeout:     time.Second,
			Concurrency: 1,
			CheckOne: func(_ context.Context, providerClientId string) BlackholeResult {
				if calls.Add(1) == 1 {
					close(entered)
				}
				<-release
				return BlackholeResult{Check: ingest.BlackholeCheck{
					ClientId:  providerClientId,
					OK:        true,
					CheckedAt: time.Unix(1, 0).UTC(),
				}}
			},
		})
	}()

	<-entered
	cancel()
	close(release)
	<-done
	if got := calls.Load(); got != 1 {
		t.Fatalf("checks started after cancellation = %d, want only the in-flight check", got)
	}
}

// A cancellation that arrives while an already-admitted check is blocked is a
// prober lifecycle failure, not evidence that the provider blackholed traffic.
// Before the guard, this synthetic all-destinations-failed result was emitted
// and persisted even though every request had been canceled by the owner.
func TestRunBlackholeDoesNotPersistInFlightFailureAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan BlackholeSummary, 1)
	go func() {
		summary, err := RunBlackhole(ctx, []string{"synthetic-provider"}, BlackholeOptions{
			Pins:        testPins,
			Timeout:     time.Second,
			Concurrency: 1,
			CheckOne: func(_ context.Context, providerClientId string) BlackholeResult {
				close(entered)
				<-release
				return BlackholeResult{
					Check:   ingest.BlackholeCheck{ClientId: providerClientId, OK: false, Failure: "all_destinations_failed", CheckedAt: time.Unix(1, 0).UTC()},
					Dark:    true,
					Details: "synthetic canceled control-plane request",
				}
			},
		})
		if err != nil {
			t.Errorf("RunBlackhole: %v", err)
		}
		done <- summary
	}()

	<-entered
	cancel()
	close(release)
	summary := <-done
	if len(summary.Checks) != 0 || summary.Dark != 0 || summary.TunnelFailed != 0 {
		t.Fatalf("canceled in-flight check became provider verdict: %+v", summary)
	}
}

func TestRunBlackholePreservesDueOrder(t *testing.T) {
	providerClientIds := []string{"third", "first", "second"}
	var stateLock sync.Mutex
	releaseByProviderClientId := map[string]chan struct{}{}
	for _, providerClientId := range providerClientIds {
		releaseByProviderClientId[providerClientId] = make(chan struct{})
	}

	done := make(chan BlackholeSummary, 1)
	go func() {
		summary, _ := RunBlackhole(context.Background(), providerClientIds, BlackholeOptions{
			Pins:        testPins,
			Timeout:     time.Second,
			Concurrency: len(providerClientIds),
			CheckOne: func(_ context.Context, providerClientId string) BlackholeResult {
				stateLock.Lock()
				release := releaseByProviderClientId[providerClientId]
				stateLock.Unlock()
				<-release
				return BlackholeResult{Check: ingest.BlackholeCheck{
					ClientId:  providerClientId,
					OK:        true,
					CheckedAt: time.Unix(1, 0).UTC(),
				}}
			},
		})
		done <- summary
	}()

	close(releaseByProviderClientId["second"])
	close(releaseByProviderClientId["first"])
	close(releaseByProviderClientId["third"])
	summary := <-done
	if len(summary.Checks) != len(providerClientIds) {
		t.Fatalf("check count = %d, want %d", len(summary.Checks), len(providerClientIds))
	}
	for index, providerClientId := range providerClientIds {
		if summary.Checks[index].ClientId != providerClientId {
			t.Fatalf("check %d = %q, want %q", index, summary.Checks[index].ClientId, providerClientId)
		}
	}
}

func TestRunBlackholeRejectsZeroConcurrency(t *testing.T) {
	_, err := RunBlackhole(context.Background(), []string{"provider"}, BlackholeOptions{
		Pins:        testPins,
		Timeout:     time.Second,
		Concurrency: 0,
	})
	if err == nil {
		t.Fatal("zero concurrency was accepted; the job channel would have no reader")
	}
}
