//go:build sidecar_localstack && sidecar_testhooks

// EVSB-M2-11's evacuation rows on T3 (§22.4, §22.8): the builders sidecartest/fault registers on the evs world, run on
// T3's Nets, where sidecart3 names this package's block as each world's instances (sidecart3/evs_tier.go), so every push
// goes through server/cli/evs's handler on the private Redis and the local stack's MinIO, and each evacuation run reads
// and writes those stores. Each row's registered case runs under every rule it takes with T3's nightly seeds
// (sidecart3.RunRow), with T3's store clock for the cases catalog.yml flags store_clock, which every row here is, and
// each clause registered beside it under the same rules (sidecart3.RunRegisteredClause), each test named for its clause by
// the gate's convention (QD-1084). FS-60's pending-record clause holds an instance at its head CAS, so this needs
// sidecar_testhooks; the stack must be up (sidecar/local/stack.sh), and these fail rather than skip without it.

package evshost_test

import (
	"testing"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs53OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-53", "", nil)
	})
}

func TestFs60OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-60", "", nil)
	})
}

func TestFs60PendingRecordFencedThenDroppedOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-60", "pending record fenced then dropped")
}

func TestFs60ParcelsPackAtSevenDaysOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-60", "parcels pack at seven days")
}
