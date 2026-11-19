//go:build sidecar_localstack

package sidecart3

// M4's exit sweep on T3 (TEST-M4-04, TEST-M4-01): FI-11, path integrity, which FS-38's and FS-56's rows name, runs
// after every step of a schedule on a T3 Net as it does on T2's: fault's own backend fixture, one RecordingBackend on
// the run's Net and a client on loopback, the same builder fault's TestFi11FixtureReadsEachCaseOnT2 runs. The sweep's
// T3 rows themselves run where their owners wrote them: in this package by convention, in controller (FS-38, FS-56)
// and in taskworker/work (FS-37), as sidecar's gate_m4.go names them.

import (
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// On a T3 Net a request that reaches its backend admitted reads clean; a byte before the audience check and one after
// the flow's revocation each fail at the step they arrived in; and a positive twin whose backend nothing reaches
// fails at the end, as on T2.
func TestFi11RunsAfterEveryStepOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		run := func(fixtureCase fault.Fi11FixtureCase) *fault.Result {
			return runOn(tb, fault.Fi11BackendSchedule(fixtureCase, nil), NetSettings(tb, nil))
		}
		if result := run(fault.Fi11FixtureTwin); result.Err != nil {
			tb.Fatalf("a request admitted on T3: %s", result)
		}
		for fixtureCase, text := range map[fault.Fi11FixtureCase]string{
			fault.Fi11FixtureBeforeCheck:     "bytes before its audience check",
			fault.Fi11FixtureAfterRevocation: "after its flow's revocation",
			fault.Fi11FixtureDeadPath:        "dead path",
		} {
			result := run(fixtureCase)
			if result.Err == nil || result.FailedStep != fault.Fi11FixtureFailedStep(fixtureCase) || !strings.Contains(result.Err.Error(), text) {
				tb.Fatalf("%s on T3: %s", fixtureCase, result)
			}
			if len(result.Violations) == 0 || result.Violations[0].Invariant != "FI-11" {
				tb.Fatalf("%s on T3: violations %v", fixtureCase, result.Violations)
			}
		}
	})
}
