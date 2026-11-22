//go:build race

// This file identifies race-instrumented builds of this package's internal
// tests, whose sync.Pool drops a quarter of its puts.
package connect

// Whether the internal tests were compiled with the race detector.
const connectInternalRaceEnabled = true
