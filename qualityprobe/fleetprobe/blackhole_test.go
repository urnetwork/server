package fleetprobe

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

// Tests of the blackhole batch: the fixed worker pool, cancellation, due order,
// defaults, not-measured counting and places.

// A PinSource with one complete pin.
func testPins() map[string][]string {
	return map[string][]string{"source.invalid": {"leaf", "intermediate"}}
}

// A batch runs on exactly Concurrency workers: a fourth provider does not
// start while three are blocked.
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
		_, err := RunBlackhole(context.Background(), ProvidersFromClientIds(providerClientIds), BlackholeOptions{
			Pins:        testPins,
			Timeout:     time.Second,
			Concurrency: concurrency,
			CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
				providerClientId := provider.ClientId
				started <- struct{}{}
				<-release
				return BlackholeResult{Check: ingest.BlackholeCheck{
					ClientId:  providerClientId,
					Ok:        true,
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

// A job admitted to the channel before the batch was cancelled is not started
// after it.
func TestRunBlackholeDoesNotStartAdmittedWorkAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = RunBlackhole(ctx, ProvidersFromClientIds([]string{"first", "second", "third"}), BlackholeOptions{
			Pins:        testPins,
			Timeout:     time.Second,
			Concurrency: 1,
			CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
				providerClientId := provider.ClientId
				if calls.Add(1) == 1 {
					close(entered)
				}
				<-release
				return BlackholeResult{Check: ingest.BlackholeCheck{
					ClientId:  providerClientId,
					Ok:        true,
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
		summary, err := RunBlackhole(ctx, ProvidersFromClientIds([]string{"synthetic-provider"}), BlackholeOptions{
			Pins:        testPins,
			Timeout:     time.Second,
			Concurrency: 1,
			CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
				providerClientId := provider.ClientId
				close(entered)
				<-release
				return BlackholeResult{
					Check:   ingest.BlackholeCheck{ClientId: providerClientId, Ok: false, Failure: "all_destinations_failed", CheckedAt: time.Unix(1, 0).UTC()},
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

// The summary keeps the due list's order whatever order the checks finish in.
func TestRunBlackholePreservesDueOrder(t *testing.T) {
	providerClientIds := []string{"third", "first", "second"}
	var stateLock sync.Mutex
	releaseByProviderClientId := map[string]chan struct{}{}
	for _, providerClientId := range providerClientIds {
		releaseByProviderClientId[providerClientId] = make(chan struct{})
	}

	done := make(chan BlackholeSummary, 1)
	go func() {
		summary, _ := RunBlackhole(context.Background(), ProvidersFromClientIds(providerClientIds), BlackholeOptions{
			Pins:        testPins,
			Timeout:     time.Second,
			Concurrency: len(providerClientIds),
			CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
				providerClientId := provider.ClientId
				stateLock.Lock()
				release := releaseByProviderClientId[providerClientId]
				stateLock.Unlock()
				<-release
				return BlackholeResult{Check: ingest.BlackholeCheck{
					ClientId:  providerClientId,
					Ok:        true,
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

// Zero concurrency is the default, sized for tunnels that mostly wait; only a
// negative value is refused.
func TestRunBlackholeConcurrencyDefaultsAndRejectsNegative(t *testing.T) {
	_, err := RunBlackhole(context.Background(), ProvidersFromClientIds([]string{"provider"}), BlackholeOptions{
		Pins:        testPins,
		Timeout:     time.Second,
		Concurrency: -1,
	})
	if err == nil {
		t.Fatal("negative concurrency was accepted; the job channel would have no reader")
	}
	if got := (BlackholeOptions{}).concurrency(); got != DefaultBlackholeConcurrency || DefaultBlackholeConcurrency != 16 {
		t.Fatalf("zero concurrency = %d (default %d), want the default of 16", got, DefaultBlackholeConcurrency)
	}
	summary, err := RunBlackhole(context.Background(), ProvidersFromClientIds([]string{"a", "b"}), BlackholeOptions{
		Timeout: time.Second,
		CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
			return BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Ok: true, CheckedAt: time.Unix(1, 0).UTC()}}
		},
	})
	if err != nil || len(summary.Checks) != 2 {
		t.Fatalf("a batch with no concurrency and no pins set: %+v %v", summary, err)
	}
}

// A check that measured nothing is submitted as not measured and counted
// apart: it is never dark, and never tunnel-failed.
func TestRunBlackholeCountsNotMeasuredApart(t *testing.T) {
	summary, err := RunBlackhole(context.Background(), ProvidersFromClientIds([]string{"lost", "dark", "ok"}), BlackholeOptions{
		Timeout:     time.Second,
		Concurrency: 1,
		CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
			at := time.Unix(1, 0).UTC()
			switch provider.ClientId {
			case "lost":
				return BlackholeResult{Check: ingest.BlackholeCheck{ClientId: "lost", Failure: "not_measured", NotMeasured: true, CheckedAt: at}, NotMeasured: true}
			case "dark":
				return BlackholeResult{Check: ingest.BlackholeCheck{ClientId: "dark", Failure: "all_destinations_failed", CheckedAt: at}, Dark: true}
			default:
				return BlackholeResult{Check: ingest.BlackholeCheck{ClientId: "ok", Ok: true, CheckedAt: at}}
			}
		},
	})
	if err != nil {
		t.Fatalf("RunBlackhole: %v", err)
	}
	if len(summary.Checks) != 3 || summary.Dark != 1 || summary.NotMeasured != 1 || summary.TunnelFailed != 0 {
		t.Fatalf("summary = %+v, want 3 checks, 1 dark, 1 not measured", summary)
	}
}

// The places a due list carries reach the per-provider check.
func TestRunBlackholePassesEachProvidersPlace(t *testing.T) {
	providers := ProvidersFromDue([]ingest.DueProvider{
		{ClientId: "a", CountryCode: " US ", Region: "Texas"},
		{ClientId: "b"},
	})
	seen := map[string]prober.Provider{}
	_, err := RunBlackhole(context.Background(), providers, BlackholeOptions{
		Timeout:     time.Second,
		Concurrency: 1,
		CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
			seen[provider.ClientId] = provider
			return BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Ok: true, CheckedAt: time.Unix(1, 0).UTC()}}
		},
	})
	if err != nil {
		t.Fatalf("RunBlackhole: %v", err)
	}
	if got := seen["a"].Place; got.Country != "us" || got.Region != "Texas" {
		t.Errorf("provider a place = %+v, want us/Texas", got)
	}
	if got := seen["b"].Place; got.Country != "" || got.Region != "" {
		t.Errorf("provider b place = %+v, want none", got)
	}
}
