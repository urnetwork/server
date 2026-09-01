package mcp

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

func TestMcpWarmupTargetsMatchUsedFeatures(t *testing.T) {
	want := []server.WarmupTarget{
		server.WarmupTargetIPDatabase,
		server.WarmupTargetCountryLocations,
		server.WarmupTargetLocationSearch,
	}
	if got := mcpWarmupTargets(); !slices.Equal(got, want) {
		t.Fatalf("mcp warmup targets = %v, want %v", got, want)
	}

	for _, excluded := range []server.WarmupTarget{
		server.WarmupTargetNetworkNameSearch,
		server.WarmupTargetLocationDirectory,
	} {
		if slices.Contains(mcpWarmupTargets(), excluded) {
			t.Fatalf("mcp warms unused target %q", excluded)
		}
	}
}

func TestStartupDoesNotWarmBeforeReadiness(t *testing.T) {
	readinessErr := errors.New("database migration head 590 is below binary-required head 597")
	warmups := 0
	err := startup(
		context.Background(),
		func(context.Context) error { return readinessErr },
		func() { warmups++ },
	)
	if !errors.Is(err, readinessErr) {
		t.Fatalf("startup error = %v, want readiness error", err)
	}
	if warmups != 0 {
		t.Fatalf("warmup ran %d time(s) before readiness", warmups)
	}
}

func TestStartupWarmsExactlyOnceAfterReadiness(t *testing.T) {
	readinessCalls := 0
	warmups := 0
	err := startup(
		context.Background(),
		func(context.Context) error {
			readinessCalls++
			return nil
		},
		func() { warmups++ },
	)
	if err != nil {
		t.Fatal(err)
	}
	if readinessCalls != 1 || warmups != 1 {
		t.Fatalf("readiness calls/warmups = %d/%d, want 1/1", readinessCalls, warmups)
	}
}

func TestStartupDoesNotWarmAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	warmups := 0
	err := startup(
		ctx,
		func(context.Context) error {
			cancel()
			return nil
		},
		func() { warmups++ },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("startup error = %v, want context canceled", err)
	}
	if warmups != 0 {
		t.Fatalf("warmup ran %d time(s) after cancellation", warmups)
	}
}
