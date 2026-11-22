package session

import (
	"testing"

	"github.com/urnetwork/server"
)

// S1 W11: connect's hint read ran the inventory script, which prunes, returns
// every live session and extends the shared keys. The revision a hint carries
// is now two key reads that write nothing; only a missing revision starts its
// incarnation through the inventory read, as before.
func TestSessionRevisionReadWritesNothing(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		generation, eventId, err := ReadNetworkSessionRevision(ctx, networkId)
		if err != nil || generation == "" || eventId != 1 {
			t.Fatalf("a missing revision did not start its incarnation: %q %d %v", generation, eventId, err)
		}
		credential := sessionFixtureCredential(networkId, server.NowUtc())
		if _, err := RegisterNetworkSession(ctx, credential, "password", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		before := sessionSharedKeyExpireTimes(t, ctx, networkId)
		for range 3 {
			read, readEventId, err := ReadNetworkSessionRevision(ctx, networkId)
			if err != nil || read != generation || readEventId != 2 {
				t.Fatalf("the read returned %q %d %v, want the new session's revision", read, readEventId, err)
			}
		}
		after := sessionSharedKeyExpireTimes(t, ctx, networkId)
		if len(after) != len(before) {
			t.Fatalf("the reads changed the shared keys: %v, was %v", after, before)
		}
		for key, expireTime := range before {
			if !after[key].Equal(expireTime) {
				t.Fatalf("a read moved %s's expiry from %s to %s", key, expireTime, after[key])
			}
		}
	})
}
