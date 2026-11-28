//go:build !race

// This file identifies ordinary builds of this package's internal tests.
package connect

// Whether the internal tests were compiled with the race detector.
const connectInternalRaceEnabled = false
