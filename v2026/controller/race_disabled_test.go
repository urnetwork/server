//go:build !race

// This file identifies ordinary controller test builds.
package controller

// Records that the controller tests were compiled without the race detector.
const controllerRaceEnabled = false
