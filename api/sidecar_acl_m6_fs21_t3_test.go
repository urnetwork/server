//go:build sidecar_localstack && sidecar_pending

package api_test

// FS-21's team on T3 (ACL-M6-05, sidecartest/fault/fs_acl_m6.go; sidecar_acl_m6_t3_test.go has the rest of its
// schedules): a Team's clients on server's exchange under server's contract gate, whose clients api marks in MG0's
// tables, so it builds only with sidecar_pending, as the gates and sidecar-t3.sh run it; without the pending groups the
// server cannot mark a client and the row fails at its first one.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// The fixed schedule, FI-10 after every step; sidecart3's TestT3Rows runs the case under §22.8's rules.
func TestFs21TeamOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.Fs21Team, nil)
	})
}
