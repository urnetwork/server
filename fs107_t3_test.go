//go:build sidecar_localstack && sidecar_testhooks

// FS-107 and FS-108 on T3 (§22.4, §22.8; §11.14's dependency, reads, tombstones and roll-up rows; owner decision 584;
// FI-18, FI-20; SEARCH-M10-36): the cases sidecartest/fault registers and exports (fs107.go, fs108.go), run on T3's
// Nets, where sidecart3 names evshost's block as the evs world's instances (sidecart3/evs_tier.go). So writers' in-ref
// pushes, each carrying its own full-entry head beside its content, a removal's tombstone, two roll-ups through EvsRepo
// and evacuation's sweep with format2.purge on go through the handler server runs onto the private Redis and the local
// stack's MinIO, and the reader's view of the index, read through EvsRepo's verifying reader, holds each document once
// at its newest version, and nothing of a removed one once its bucket rolls. Each case runs under every rule its row
// takes with T3's nightly seeds (sidecart3.RunRow).
//
// The stack must be up (sidecar/local/stack.sh), and these fail rather than skip without it.
//
// Not here, and so not run on T3: FS-107's and FS-108's other clauses (seals, large entries, merges, slot reuse,
// backfills, DROPs, the reserved kind, the skewed writers' promotion, the decoder's refusals and a fresh device's
// search), and FS-76 whole, which run on T2 over the search world's stand-in for evs (sidecar's fs76_test.go,
// fs107_test.go and fs108_test.go) until merges, backfills and a device's sync run over EvsRepo (SEARCH-M10-19's
// source).

package server_test

import (
	"testing"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs107OnT3(t *testing.T) {
	fs74T3TestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-107", "", nil)
	})
}

func TestFs108OnT3(t *testing.T) {
	fs74T3TestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-108", "", nil)
	})
}
