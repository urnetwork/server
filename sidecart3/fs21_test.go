//go:build sidecar_localstack && sidecar_pending

// FS-21 on T3 (ACL-M4-04; fs_acl_test.go has ACL-M2-16's other rows): its clients run on server's exchange under
// server's contract gate, whose clients api marks in MG0's tables, so the file builds only with sidecar_pending, as the
// gates and sidecar-t3.sh run it; without the pending groups the server cannot mark a client and the row fails at its
// first one.

package sidecart3

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// ACL-M4-04's FS-21 (sidecartest/fault/fs21.go): a personal network's devices with no reach to each other, every client
// on server's exchange under server's contract gate over the network's log on server's sequencer: HeadCheck over
// control contracts, a data RPC, a stream and IP on them refused, the server partitioning its relays, and a grant's
// data contract carrying all three. The fixed schedule, FI-10 after every step; TestT3Rows runs it under §22.8's rules.
func TestFs21OnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs21, nil)
	})
}
