package connect

// The connect service's timer census for the performance harness's opt-in
// report (connect_perf_test.go, CPUOPT7.md §11). The harness runs in package
// connect_test, which can't reach startTimerCensus, so this test file exports
// it, with every call counted. None of it is production API.

import (
	"github.com/urnetwork/server"
)

// Starts the census with its library hooks, every call counted, and returns
// the stop.
func Testing_StartTimerCensus() (func(), error) {
	return startTimerCensus(&server.TimerCensusSettings{SamplePeriod: 1})
}
