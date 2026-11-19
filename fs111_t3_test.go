//go:build sidecar_localstack && sidecar_testhooks

// FS-111 on T3 (§22.4, §22.8; §11.14's cut, epoch, erase and writers rows; §11.12's cut and reconcile rows; D-EVS-66,
// I-EVS-17, FI-2, FI-19; SEARCH-M10-37): the case sidecartest/fault registers and exports (fs111.go), run on T3's Nets,
// where sidecart3 names evshost's block as the evs world's instances (sidecart3/evs_tier.go). So a writer's in-ref push
// paused at its instance just before its head CAS, an admin's CUT moving the ref's index_epoch through EvsRepo, the
// paused push's 409 INDEX_EPOCH and its rebuild without the listed document, the eraser's CLOSURE, its death right after
// it and its device's resume from evs's state 20 min on all go through the handler server runs onto the private Redis
// and the local stack's MinIO, and the reader's view of the index holds the document nowhere live. The case runs under
// every rule its row takes with T3's nightly seeds (sidecart3.RunRow).
//
// The stack must be up (sidecar/local/stack.sh), and this fails rather than skips without it.
//
// Not here, and so not run on T3: FS-111's other clauses (a merge's shard, a backfill, a build and a reused slot paused
// before the CUT, another device of the eraser's member resuming 25 h on, a CLOSURE built before another eraser's CUT,
// and FI-19's sweep of every stored version under a slot-keyed repo's keys), which run on T2 over the search world's
// stand-in for evs (sidecar's fs111_test.go) until the evs world runs those builders and a slot-keyed repo's index
// over EvsRepo; and FS-78's and FS-112's cases, which run on that stand-in alone, since ?closure=1 uploads at the limit
// are not built on the client (EvsRepo.Closure refuses them) and no erasure through EvsRepo says complete yet.

package server_test

import (
	"testing"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs111OnT3(t *testing.T) {
	fs74T3TestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-111", "", nil)
	})
}
