//go:build sidecar_localstack && sidecar_testhooks

// The team directory's format-2 cases on T3 (§22.4, §22.8; §16.11's revoke row and Erasure and removal block, §11.12's
// cut, closure and directory rows, I-EVS-16, owner decision 181; RECORD-M10-06): FS-99's cards case and FS-87's index
// case as sidecartest/fault registers them (fs99_cards_evs.go, fs87_index_evs.go), run on T3's Nets, where sidecart3
// names evshost's block as the evs world's instance (sidecart3/evs_tier.go). So the directory's cards ref, its r/ and
// d/ files, their index files, the writers' streams and every slot object stand on the private Redis and the local
// stack's MinIO; an ordinary agent's CUT, CLOSURE and ERASE are refused 403 SCOPE by the handler server runs, with
// every slot's ETag in the store as it was; and a directory admin's CUTs, CLOSUREs and ERASEs, a runner killed between
// its CLOSURE and its ERASE and its resume from evs's state, moderation's removal of a card and the roll-up past
// searchable_days all go through that handler. Each case runs under every rule its row takes with T3's nightly seeds
// (sidecart3.RunRow).
//
// The stack must be up (sidecar/local/stack.sh), and these fail rather than skip without it.
//
// Not here, and so not run on T3: FS-100's format-2 case, whose versionless notices run on sidecartest's cards fake
// (sidecar's fs100_format2_test.go) until its wake world's directory runs on the evs world; FS-84's and FS-101's
// cases, which are T2's alone; and §7.9's and §8's state, which the directory world answers from sidecartest's fakes
// on both tiers, so which tokens §7.8's exchange mints a writer is the world's own here.

package server_test

import (
	"testing"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs99CardsOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-99", "cards", nil)
	})
}

func TestFs87IndexOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-87", "index", nil)
	})
}
