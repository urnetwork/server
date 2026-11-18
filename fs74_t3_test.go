//go:build sidecar_localstack && sidecar_testhooks

// FS-74 and FS-75 on T3 (§22.4, §22.8; §11.14's commit path, in-ref, roll-up, lock and crash rows; FI-18, FI-20;
// SEARCH-M10-35): the cases sidecartest/fault registers and exports (fs74.go, fs75.go), run on T3's Nets, where
// sidecart3 names evshost's block as the evs world's instances (sidecart3/evs_tier.go). So two writers' in-ref pushes,
// each carrying its own head beside its content, go through the handler server runs onto the private Redis and the
// local stack's MinIO, and the reader's view of the index, read through EvsRepo's verifying reader, holds both
// documents; and a bucket those pushes fill, expired, is rolled up through EvsRepo by a second runner after the first
// is killed holding the lock, one roll-up standing with every document in all. Each case runs under every rule its
// row takes with T3's nightly seeds (sidecart3.RunRow), FS-75's with each of its writes' answers lost.
//
// The stack must be up (sidecar/local/stack.sh), and these fail rather than skip without it.
//
// Not here, and so not run on T3: FS-74's and FS-75's other clauses, and FS-79, which run on T2 over the search world's
// stand-in for evs (sidecar's fs74_test.go, fs75_test.go and fs79_test.go) until the evs world runs their merges,
// builds and backfills over EvsRepo, and a device searches through an EvsRepo-backed sync source (SEARCH-M10-19's).

package server_test

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// The private stores, and no schema: an evs world opens no connect client.
func fs74T3TestEnv() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: false}
}

func TestFs74OnT3(t *testing.T) {
	fs74T3TestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-74", "", nil)
	})
}

func TestFs75OnT3(t *testing.T) {
	fs74T3TestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-75", "", nil)
	})
}
