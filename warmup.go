package server

var warmupLock sync.Mutex
var warmupUnits []func()
var warmedUp = false

func Warm(unit func()) {
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
