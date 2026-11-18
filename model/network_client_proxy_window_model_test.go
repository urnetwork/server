package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The window identity store carries a hosted device's live window snapshot
// across a restart (PROXYDRAIN1.md §3.5): read-not-consumed, replaced by
// newer snapshots, cleared by an empty one.
func TestProxyWindowIdentities(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		proxyId := server.NewId()

		// absent: nil
		connect.AssertEqual(t, 0, len(GetProxyWindowIdentities(ctx, proxyId)))

		identityA := &ProxyWindowClientIdentity{
			ClientId:       server.NewId(),
			ByJwt:          "jwt-a",
			InstanceId:     server.NewId(),
			DestinationIds: []server.Id{server.NewId(), server.NewId()},
		}
		identityB := &ProxyWindowClientIdentity{
			ClientId:       server.NewId(),
			ByJwt:          "jwt-b",
			InstanceId:     server.NewId(),
			DestinationIds: []server.Id{server.NewId()},
		}

		SetProxyWindowIdentities(ctx, proxyId, []*ProxyWindowClientIdentity{identityA, identityB})

		identities := GetProxyWindowIdentities(ctx, proxyId)
		connect.AssertEqual(t, 2, len(identities))
		byClientId := map[server.Id]*ProxyWindowClientIdentity{}
		for _, identity := range identities {
			byClientId[identity.ClientId] = identity
		}
		readA := byClientId[identityA.ClientId]
		connect.AssertEqual(t, true, readA != nil)
		connect.AssertEqual(t, "jwt-a", readA.ByJwt)
		connect.AssertEqual(t, identityA.InstanceId, readA.InstanceId)
		connect.AssertEqual(t, 2, len(readA.DestinationIds))
		connect.AssertEqual(t, identityA.DestinationIds[0], readA.DestinationIds[0])
		connect.AssertEqual(t, identityA.DestinationIds[1], readA.DestinationIds[1])

		// a plain read, not consumed
		connect.AssertEqual(t, 2, len(GetProxyWindowIdentities(ctx, proxyId)))

		// a newer snapshot replaces
		SetProxyWindowIdentities(ctx, proxyId, []*ProxyWindowClientIdentity{identityB})
		identities = GetProxyWindowIdentities(ctx, proxyId)
		connect.AssertEqual(t, 1, len(identities))
		connect.AssertEqual(t, identityB.ClientId, identities[0].ClientId)

		// an empty snapshot clears
		SetProxyWindowIdentities(ctx, proxyId, nil)
		connect.AssertEqual(t, 0, len(GetProxyWindowIdentities(ctx, proxyId)))
	})
}

// A proxy whose device closed reopens on the exits it had for two hours
// (P90SPEED.md §4.1, fix 4). Ten minutes from the window's last change
// expired before almost every reopen. The snapshot lives two hours from its
// write, and the device's close restarts that without writing identities; a
// refresh with no snapshot creates none.
func TestProxyWindowIdentitiesRestorableForTwoHoursAfterClose(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		proxyId := server.NewId()
		keyTtl := func() time.Duration {
			var ttl time.Duration
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				ttl, err = r.PTTL(ctx, proxyWindowIdentityKey(proxyId)).Result()
				server.Raise(err)
			})
			return ttl
		}
		// at least two hours, with slack for the time the test itself takes
		assertRestorableForTwoHours := func(stage string) {
			if ttl := keyTtl(); ttl < 2*time.Hour-time.Minute {
				t.Fatalf("%s: the snapshot is restorable for %s, want at least 2h", stage, ttl)
			}
		}

		// no snapshot: the close of a device that recorded none creates none
		connect.AssertEqual(t, false, RefreshProxyWindowIdentities(ctx, proxyId))
		connect.AssertEqual(t, 0, len(GetProxyWindowIdentities(ctx, proxyId)))

		identity := &ProxyWindowClientIdentity{
			ClientId:       server.NewId(),
			ByJwt:          "jwt-a",
			InstanceId:     server.NewId(),
			DestinationIds: []server.Id{server.NewId()},
		}
		SetProxyWindowIdentities(ctx, proxyId, []*ProxyWindowClientIdentity{identity})
		assertRestorableForTwoHours("written")

		// as if the window last changed long before its device closed
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Expire(ctx, proxyWindowIdentityKey(proxyId), time.Minute).Err())
		})
		connect.AssertEqual(t, true, RefreshProxyWindowIdentities(ctx, proxyId))
		assertRestorableForTwoHours("refreshed at close")

		// the refresh keeps the identities as written
		identities := GetProxyWindowIdentities(ctx, proxyId)
		connect.AssertEqual(t, 1, len(identities))
		connect.AssertEqual(t, identity.ClientId, identities[0].ClientId)
		connect.AssertEqual(t, "jwt-a", identities[0].ByJwt)
		connect.AssertEqual(t, identity.InstanceId, identities[0].InstanceId)
	})
}
