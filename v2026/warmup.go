package server

import (
	"sync"
)

var warmupLock sync.Mutex
var warmupUnits []func()
var resetUnits []func()
var warmedUp = false

func OnWarmup(unit func()) {
	runNow := false
	func() {
		warmupLock.Lock()
		defer warmupLock.Unlock()

		if warmedUp {
			runNow = true
		} else {
			warmupUnits = append(warmupUnits, unit)
		}
	}()
	if runNow {
		unit()
	}
}

func Warmup() {
	var runUnits []func()
	func() {
		warmupLock.Lock()
		defer warmupLock.Unlock()

		runUnits = warmupUnits
		warmupUnits = nil
		warmedUp = true
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
		warmupUnits = nil
		warmedUp = false
	}()
	for _, unit := range runUnits {
		unit()
	}
}
