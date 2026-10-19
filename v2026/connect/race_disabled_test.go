//go:build !race

// This file identifies ordinary server/connect performance builds.
package connect_test

// Records that server/connect tests were compiled without the race detector.
const serverConnectRaceEnabled = false
