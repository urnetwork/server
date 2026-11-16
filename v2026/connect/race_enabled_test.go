//go:build race

// This file identifies race-instrumented server/connect correctness builds.
package connect_test

// Records that server/connect tests were compiled with the race detector.
const serverConnectRaceEnabled = true
