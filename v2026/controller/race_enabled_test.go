//go:build race

// This file identifies race-instrumented controller test builds.
package controller

// Records that the controller tests were compiled with the race detector,
// which instruments the arithmetic a rate test measures.
const controllerRaceEnabled = true
