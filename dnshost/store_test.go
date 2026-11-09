//go:build sidecar_pending

package dnshost

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// sidecartest.RunDnsStoreSuite on Postgres and Redis, the cases the fake passes (D-TEST-2): the serial
// never lowers, one journal row a write, a gap reported, a snapshot at one serial, restoring refusing
// writes, the restore's jump and its idempotence, the compare-and-swap, reload diffs, and a raise
// waking a watcher through the process's one key-event subscriber.
func TestStoreRunsTheDnsStoreSuite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Testing_EnableKeyspaceNotifications(ctx)
		store, err := NewStore(sidecarhost.NewRedis(nil))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		sidecartest.RunDnsStoreSuite(t, store)
	})
}
