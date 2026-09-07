package server

import (
	"fmt"
	"sync"
)

// WarmupTarget names one independently selectable eager-initialization
// feature. Keep every production target in this file so a service's warmup
// list is an auditable declaration of the memory it chooses to make resident.
type WarmupTarget string

const (
	WarmupTargetIPDatabase        WarmupTarget = "ip-database"
	WarmupTargetNetworkNameSearch WarmupTarget = "network-name-search"
	WarmupTargetLocationSearch    WarmupTarget = "location-search"
	WarmupTargetCountryLocations  WarmupTarget = "country-locations"
	WarmupTargetLocationDirectory WarmupTarget = "location-directory"
)

var allWarmupTargets = []WarmupTarget{
	WarmupTargetIPDatabase,
	WarmupTargetNetworkNameSearch,
	WarmupTargetLocationSearch,
	WarmupTargetCountryLocations,
	WarmupTargetLocationDirectory,
}

var validWarmupTargets = func() map[WarmupTarget]struct{} {
	targets := make(map[WarmupTarget]struct{}, len(allWarmupTargets))
	for _, target := range allWarmupTargets {
		targets[target] = struct{}{}
	}
	return targets
}()

// AllWarmupTargets returns a copy of the declared target list. It is intended
// for comprehensive test fixtures and registry audits; production services
// should enumerate only the targets they actually use.
func AllWarmupTargets() []WarmupTarget {
	return append([]WarmupTarget(nil), allWarmupTargets...)
}

var warmupLock sync.Mutex
var warmupUnits = map[WarmupTarget][]func(){}
var resetUnits []func()
var warmedUpTargets = map[WarmupTarget]bool{}

func requireWarmupTarget(target WarmupTarget) {
	if _, ok := validWarmupTargets[target]; !ok {
		panic(fmt.Errorf("unknown warmup target %q", target))
	}
}

func OnWarmup(target WarmupTarget, unit func()) {
	requireWarmupTarget(target)
	runNow := false
	func() {
		warmupLock.Lock()
		defer warmupLock.Unlock()

		if warmedUpTargets[target] {
			runNow = true
		} else {
			warmupUnits[target] = append(warmupUnits[target], unit)
		}
	}()
	if runNow {
		unit()
	}
}

// Warmup eagerly initializes only the requested targets, in the supplied
// order. A target runs at most once per process lifecycle. An empty list is an
// explicit no-op, which lets a lightweight service document that it relies on
// the underlying lazy initializers without loading unrelated feature indexes.
func Warmup(targets ...WarmupTarget) {
	for _, target := range targets {
		requireWarmupTarget(target)
	}

	var runUnits []func()
	func() {
		warmupLock.Lock()
		defer warmupLock.Unlock()

		for _, target := range targets {
			if warmedUpTargets[target] {
				continue
			}
			runUnits = append(runUnits, warmupUnits[target]...)
			delete(warmupUnits, target)
			warmedUpTargets[target] = true
		}
	}()
	for _, unit := range runUnits {
		unit()
	}
}

func OnReset(unit func()) {
	warmupLock.Lock()
	defer warmupLock.Unlock()

	resetUnits = append(resetUnits, unit)
}

func Reset() {
	var runUnits []func()
	func() {
		warmupLock.Lock()
		defer warmupLock.Unlock()

		runUnits = resetUnits
		warmupUnits = map[WarmupTarget][]func(){}
		warmedUpTargets = map[WarmupTarget]bool{}
	}()
	for _, unit := range runUnits {
		unit()
	}
}
