package server

import (
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestWarmup(t *testing.T) {
	withIsolatedWarmupRegistry(t)

	aRun := false
	a := func() {
		aRun = true
	}
	bRun := false
	b := func() {
		bRun = true
	}
	cRun := false
	c := func() {
		cRun = true
	}

	OnWarmup(WarmupTargetNetworkNameSearch, a)
	connect.AssertEqual(t, aRun, false)
	OnWarmup(WarmupTargetLocationSearch, b)
	connect.AssertEqual(t, bRun, false)
	Warmup(WarmupTargetNetworkNameSearch)
	connect.AssertEqual(t, aRun, true)
	connect.AssertEqual(t, bRun, false)
	connect.AssertEqual(t, cRun, false)
	OnWarmup(WarmupTargetNetworkNameSearch, c)
	connect.AssertEqual(t, cRun, true)
	Warmup(WarmupTargetLocationSearch)
	connect.AssertEqual(t, bRun, true)
}

func withIsolatedWarmupRegistry(t *testing.T) {
	t.Helper()
	warmupLock.Lock()
	previousUnits := warmupUnits
	previousWarmedTargets := warmedUpTargets
	warmupUnits = map[WarmupTarget][]func(){}
	warmedUpTargets = map[WarmupTarget]bool{}
	warmupLock.Unlock()
	t.Cleanup(func() {
		warmupLock.Lock()
		warmupUnits = previousUnits
		warmedUpTargets = previousWarmedTargets
		warmupLock.Unlock()
	})
}

func TestAllWarmupTargetsReturnsIndependentCompleteList(t *testing.T) {
	targets := AllWarmupTargets()
	want := []WarmupTarget{
		WarmupTargetIPDatabase,
		WarmupTargetNetworkNameSearch,
		WarmupTargetLocationSearch,
		WarmupTargetCountryLocations,
		WarmupTargetLocationDirectory,
	}
	if !slices.Equal(targets, want) {
		t.Fatalf("all warmup targets = %v, want %v", targets, want)
	}
	targets[0] = "mutated"
	if slices.Equal(AllWarmupTargets(), targets) {
		t.Fatal("AllWarmupTargets returned mutable registry storage")
	}
}
