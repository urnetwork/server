//go:build sidecar_localstack && sidecar_testhooks

// FS-74 on T3 (§22.4, §22.8; §11.14's commit path and in-ref row; FI-18; SEARCH-M10-35): the case sidecartest/fault
// registers and exports (fs74.go), run on T3's Nets, where sidecart3 names evshost's block as the evs world's instances
// (sidecart3/evs_tier.go). So two writers' in-ref pushes, each carrying its own head beside its content, go through
// the handler server runs onto the private Redis and the local stack's MinIO, and the reader's view of the index, read
// through EvsRepo's verifying reader, holds both documents. The case runs under every rule FS-74 takes with T3's
// nightly seeds (sidecart3.RunRow).
//
// The stack must be up (sidecar/local/stack.sh), and this fails rather than skips without it.
//
// Not here, and so not run on T3: the rest of FS-74, and FS-75 and FS-79, which run on T2 over the search world's
// stand-in for evs (sidecar's fs74_test.go, fs75_test.go and fs79_test.go) until a world runs their merges, roll-ups,
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
