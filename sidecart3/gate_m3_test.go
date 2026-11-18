//go:build sidecar_localstack

package sidecart3

// M3's exit sweep on T3 (TEST-M3-02, TEST-M3-01): FI-12, which FS-40's, FS-41's and FS-42's T3 rows name, runs after
// every step of a schedule on T3's stores as it does on T2's fakes: fault's own head-restore fixture, its head
// written, snapshotted, lost and restored on the local stack's MinIO through the oracle's store view.

import (
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// On T3's stores a head restored at its (v, C_v) reads clean, one restored below fails after the step that records
// the recovery, and a schedule naming FI-12 that records nothing fails at the end, as on T2.
func TestFi12RunsAfterEveryStepOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		run := func(builder fault.Builder) *fault.Result {
			return runOn(tb, builder, NetSettings(tb, nil))
		}
		if result := run(fault.Fi12HeadRestoreSchedule(7, true)); result.Err != nil {
			tb.Fatalf("a head restored at its C_v on T3: %s", result)
		}
		result := run(fault.Fi12HeadRestoreSchedule(6, true))
		if result.Err == nil || !strings.Contains(result.Err.Error(), "came back at 6") || result.FailedStep != 3 {
			tb.Fatalf("a head restored below its C_v on T3: %s", result)
		}
		result = run(fault.Fi12HeadRestoreSchedule(7, false))
		if result.Err == nil || !strings.Contains(result.Err.Error(), "FI-12 read no recovery") {
			tb.Fatalf("a schedule that shows nothing on T3: %s", result)
		}
	})
}
