package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
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
