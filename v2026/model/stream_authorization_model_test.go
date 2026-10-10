package model

import (
	"context"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"testing"
	"time"
)

func TestStreamAuthorizationRemoteRevokeWithoutNotifications(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network, user, deviceA, deviceB, clientA, clientB := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, network, "lease-network", user)
		Testing_CreateDevice(ctx, network, deviceA, clientA, "a", "test")
		Testing_CreateDevice(ctx, network, deviceB, clientB, "b", "test")
		sidA, sidB := server.NewId(), server.NewId()
		a := session.NewByJwt(network, user, "lease-network", false, false).Client(deviceA, clientA)
		a.SessionId = &sidA
		b := session.NewByJwt(network, user, "lease-network", false, false).Client(deviceB, clientB)
		b.SessionId = &sidB
		for _, value := range []*session.ByJwt{a, b} {
			if _, err := session.RegisterNetworkSession(ctx, value, "password", nil, false, now); err != nil {
				t.Fatal(err)
			}
		}
		genA, genB, streamId := server.NewId(), server.NewId(), server.NewId()
		if err := session.PublishConnectionAuthority(ctx, a, genA, 1, now.Add(90*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := session.PublishConnectionAuthority(ctx, b, genB, 1, now.Add(90*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := storeStreamParticipants(ctx, streamId, newStreamKey(clientA, clientB, nil)); err != nil {
			t.Fatal(err)
		}
		first, err := AuthorizeStream(ctx, streamId, now)
		if err != nil || !first.Allowed || first.Legacy || first.LeaseMillis > 90000 {
			t.Fatal(first, err)
		}
		// A different server performs the cutoff. No subscriber, voluntarily closed
		// attacker socket, or client logout is used by this grant authority test.
		networkActor := session.NewByJwt(network, user, "lease-network", false, false)
		networkActor.SessionId = &sidB
		request := session.Testing_CreateClientSession(ctx, networkActor)
		defer request.Cancel()
		_, err = session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: sidA, OperationId: server.NewId()}, request)
		if err != nil {
			t.Fatal(err)
		}
		latest, err := AuthorizeStream(ctx, streamId, now.Add(89*time.Second))
		if err != nil || !latest.Allowed {
			t.Fatal("valid in-flight endpoint lease changed", latest, err)
		}
		if err = session.PublishConnectionAuthority(ctx, b, genB, 1, now.Add(180*time.Second)); err != nil {
			t.Fatal(err)
		}
		retired, err := AuthorizeStream(ctx, streamId, now.Add(90*time.Second))
		if err != nil || retired.Allowed || retired.Generation != first.Generation {
			t.Fatal("old endpoint authority survived its bound", retired, err)
		}
		// Even a newer independent sign-in on the same client cannot resurrect the
		// revoked endpoint's stream across an exchange/resident migration.
		a.SessionId = &sidB
		if err = session.PublishConnectionAuthority(ctx, a, server.NewId(), 1, now.Add(180*time.Second)); err != nil {
			t.Fatal(err)
		}
		replay, err := AuthorizeStream(ctx, streamId, now.Add(91*time.Second))
		if err != nil || replay.Allowed {
			t.Fatal("migration revived a retired binding", replay, err)
		}
	})
}
func TestTaggedStreamRequiresBothEndpointLeaseSupport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		a, b := server.NewId(), server.NewId()
		network, user := server.NewId(), server.NewId()
		sid := server.NewId()
		credential := session.NewByJwt(network, user, "compat", false, false).Client(server.NewId(), a)
		credential.SessionId = &sid
		if err := session.PublishConnectionAuthority(ctx, credential, server.NewId(), 1, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		legacy := session.NewByJwt(network, user, "compat", false, false).Client(server.NewId(), b)
		if err := session.PublishConnectionAuthority(ctx, legacy, server.NewId(), 0, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		id := server.NewId()
		if err := storeStreamParticipants(ctx, id, newStreamKey(a, b, nil)); err != nil {
			t.Fatal(err)
		}
		grant, err := AuthorizeStream(ctx, id, now)
		if err != nil || grant.Allowed {
			t.Fatal("tagged endpoint got an unleased P2P path", grant, err)
		}
		// Compatibility only withholds P2P grants; the relay stream participants and
		// contract remain intact for existing platform data routing.
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			if r.Exists(ctx, streamAuthorityKey(id, "p")).Val() != 1 {
				t.Fatal("compatibility destroyed relay stream")
			}
			return nil
		}))
	})
}
