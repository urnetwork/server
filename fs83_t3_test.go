//go:build sidecar_localstack && sidecar_testhooks

// FS-83 on T3 (§22.4, §22.8; §11.14's who, eligible, order and failover rows, §18.4's notice row; FI-24;
// SEARCH-M10-39): the case sidecartest/fault registers (fs83.go), run on T3's Net, where sidecart3 names server's own
// config log sequencer over the local stack's MinIO as the admission world's api, so the Team the remote team builds,
// its consents, shares, grants and IndexerSets, is a log server's api appended and every reader verified, and each
// search's eligibility rests on that state. The case runs under every rule its row takes with T3's nightly seeds
// (sidecart3.RunRow).
//
// The stack must be up (sidecar/local/stack.sh), and this fails rather than skips without it.
//
// Not here, and so not run on T3: the pair's relays, which the remote team's transport stands in for on both tiers
// (§22.8's remote row measures them with real relays and netem), the indexers' and phone's index sources, which are
// search worlds standing for evs, and every clause fs83.go's header lists as not run.

package server_test

import (
	"testing"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs83OnT3(t *testing.T) {
	fs74T3TestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-83", "", nil)
	})
}
