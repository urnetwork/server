// Deterministic coverage for the provider measurement-phase boundary.
package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Builds provider state without a live SDK client so scheduling can be tested
// independently from sockets and goroutine timing.
func testDynamicsProvider(seed int64, degradedFraction float64) *simProvider {
	base := &impairParams{latency: 10 * time.Millisecond}
	degraded := &impairParams{latency: 30 * time.Millisecond}
	params := &atomic.Pointer[impairParams]{}
	params.Store(base)
	control := newRng(seed)
	_ = control.float64() // the production ramp-offset draw
	return &simProvider{
		entry: ProviderEntry{
			Seed:             seed,
			UptimeSeconds:    600,
			DowntimeSeconds:  30,
			DegradedFraction: degradedFraction,
		},
		params:    params,
		base:      base,
		degraded:  degraded,
		control:   control,
		connected: true,
	}
}

func TestProviderRampDoesNotAdvanceDynamicsBeforeMeasurement(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	provider := testDynamicsProvider(48, 0.25)
	provider.connected = false
	provider.nextChurn = now.Add(-time.Second)
	fleet := &Fleet{}
	if !fleet.advanceProviderState(provider, now) {
		t.Fatal("ramp transition did not report a connection change")
	}
	if !provider.connected || !provider.nextChurn.IsZero() {
		t.Fatalf("ramped provider connected=%t next churn=%s", provider.connected, provider.nextChurn)
	}
	if fleet.advanceProviderState(provider, now.Add(24*time.Hour)) {
		t.Fatal("provider churned before the measurement boundary")
	}
	if !provider.connected || provider.inDegraded {
		t.Fatalf("pre-measurement provider connected=%t degraded=%t", provider.connected, provider.inDegraded)
	}
}

func TestProviderDynamicsScheduleIsMeasurementAnchoredAndDeterministic(t *testing.T) {
	boundary := time.Unix(1_700_000_000, 123_000_000)
	first := testDynamicsProvider(48, 0.25)
	second := testDynamicsProvider(48, 0.25)
	firstFleet := &Fleet{providers: []*simProvider{first}}
	secondFleet := &Fleet{providers: []*simProvider{second}}
	firstFleet.startDynamicsAt(boundary)
	secondFleet.startDynamicsAt(boundary)

	if !firstFleet.dynamicsStarted || !secondFleet.dynamicsStarted {
		t.Fatal("provider dynamics did not cross the measurement boundary")
	}
	if first.nextChurn != second.nextChurn || first.nextRegime != second.nextRegime ||
		first.inDegraded != second.inDegraded {
		t.Fatalf(
			"same-seed schedules differ: first=%s/%s/%t second=%s/%s/%t",
			first.nextChurn,
			first.nextRegime,
			first.inDegraded,
			second.nextChurn,
			second.nextRegime,
			second.inDegraded,
		)
	}
	if !boundary.Before(first.nextChurn) || !boundary.Before(first.nextRegime) {
		t.Fatalf("schedule does not follow boundary: churn=%s regime=%s", first.nextChurn, first.nextRegime)
	}
}

func TestProviderWithNoDegradationNeverEntersDegradedRegime(t *testing.T) {
	boundary := time.Unix(1_700_000_000, 0)
	provider := testDynamicsProvider(49, 0)
	provider.entry.UptimeSeconds = 365 * 24 * 60 * 60
	fleet := &Fleet{providers: []*simProvider{provider}}
	fleet.startDynamicsAt(boundary)
	if provider.inDegraded || !provider.nextRegime.IsZero() || provider.params.Load() != provider.base {
		t.Fatalf(
			"zero-degradation provider degraded=%t next=%s base=%t",
			provider.inDegraded,
			provider.nextRegime,
			provider.params.Load() == provider.base,
		)
	}
	fleet.advanceProviderState(provider, boundary.Add(24*time.Hour))
	if provider.inDegraded || provider.params.Load() != provider.base {
		t.Fatal("zero-degradation provider changed regime")
	}
}

func TestProviderDynamicsStartIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fleet := &Fleet{
		ctx:           ctx,
		dynamicsStart: make(chan chan struct{}),
		done:          make(chan struct{}),
	}
	runDone := make(chan struct{})
	go func() {
		fleet.run()
		close(runDone)
	}()
	if err := fleet.StartDynamics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fleet.StartDynamics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fleet.dynamicsStarted {
		t.Fatal("provider dynamics remained stopped")
	}
	cancel()
	<-runDone
}
