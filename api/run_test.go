package api

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

func TestAPIWarmupTargetsCoverCompleteFeatureSurface(t *testing.T) {
	want := []server.WarmupTarget{
		server.WarmupTargetIPDatabase,
		server.WarmupTargetNetworkNameSearch,
		server.WarmupTargetLocationSearch,
		server.WarmupTargetCountryLocations,
		server.WarmupTargetLocationDirectory,
	}
	if got := apiWarmupTargets(); !slices.Equal(got, want) {
		t.Fatalf("api warmup targets = %v, want explicit feature targets %v", got, want)
	}
}

func TestRunRejectsInvalidInputsBeforeEnvironmentAccess(t *testing.T) {
	if err := Run(nil, RunOptions{Port: 1}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 0}); err == nil {
		t.Fatal("zero port was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 65_536}); err == nil {
		t.Fatal("overflow port was accepted")
	}
}

func TestActivateAfterReadinessDoesNotStartBackgroundOnFailure(t *testing.T) {
	wantErr := errors.New("database migration head 590 is below binary-required head 597")
	activations := 0
	err := activateAfterReadiness(
		context.Background(),
		func(context.Context) error { return wantErr },
		func() { activations++ },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("activation error = %v, want readiness error", err)
	}
	if activations != 0 {
		t.Fatalf("background activation ran %d time(s) before readiness", activations)
	}
}

func TestActivateAfterReadinessRejectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	activations := 0
	err := activateAfterReadiness(
		ctx,
		func(context.Context) error {
			cancel()
			return nil
		},
		func() { activations++ },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("activation error = %v, want context canceled", err)
	}
	if activations != 0 {
		t.Fatalf("background activation ran %d time(s) after cancellation", activations)
	}
}
