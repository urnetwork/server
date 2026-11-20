//go:build sidecar_localstack && sidecar_testhooks

package model_test

// The format-2 hold cases of FS-69 and FS-66, and FS-6's --now shred under a hold, on T3 against api's real hold/
// writes (ABUSE-M10-01; §21.18's M10 row, §21.9, §11.12's shred row, §11.13's purge, staged, sweep and holds rows,
// owner decisions 133, 184 and 629, FI-5, FI-16): sidecartest/fault's schedules (hold_cases.go), the builders T2 runs
// on the fixture, on T3 Nets whose holds api places and whose ends api's hold pass drives at each hold's deadline
// (abuse_fs92_t3_test.go's apiHoldApi and holdT3RuleSettings, with the same two readings of the tier: the Net's
// clusters declared settled, and the store clock on, catalog.yml flagging FS-69 and FS-66's hold case store_clock).
// FS-6's case is EVS1's registered race, so its hold clause runs as a clause here, on the same settings.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// FS-69, the registered case: what existed at the placement kept through evacuation's sweep, what came after purged,
// and the hold's end purging what it kept, beside its no-hold twin.
func TestFs69HoldsPlacedByApiOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		sidecart3.RunRow(tb, "FS-69", "", holdT3RuleSettings(tb))
	})
}

// FS-69: a hold api places between two of the sweep's DELETEs.
func TestFs69HoldBetweenSweepDeletesOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		fault.RunClause(tb, "FS-69", "between sweep deletes", fault.Fs69HoldBetweenSweepDeletes, holdT3RuleSettings(tb))
	})
}

// FS-69: an ERASE of a versionless record-class entry under a hold api placed destroys its keys at once.
func TestFs69EraseUnderTheHoldOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		fault.RunClause(tb, "FS-69", "an erase under the hold", fault.Fs69EraseUnderTheHold, holdT3RuleSettings(tb))
	})
}

// FS-66's hold case, registered: a staged record from a batch before api's placement written to seg/ at evacuation,
// one written and replaced under the hold dropped, and the hold's end deleting what it kept.
func TestFs66HoldPlacedByApiOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		sidecart3.RunRow(tb, "FS-66", "hold", holdT3RuleSettings(tb))
	})
}

// FS-6's clause: a --now shred under a hold api placed stops with the keys gone and the ciphertext kept, its records
// packed, until api's expiry ends the hold.
func TestFs6NowShredUnderAHoldOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		requireHoldTablesOnT3(tb)
		fault.RunClause(tb, "FS-6", "a --now shred under a hold", fault.Fs6NowShredUnderAHold, holdT3RuleSettings(tb))
	})
}
