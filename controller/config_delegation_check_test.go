//go:build sidecar_pending

package controller

// api's host-client check of a forwarding Delegation (TEAM-M8-01; D-TEAM-33; owner decision 197), through POST
// /network/config/delta on real bindings and client rows: a host client that is not the source client of the
// sidecar's current client is refused, as is another key for the right client, a host client with no registered key
// and a sidecar whose client is top-level; the same Delegation naming the source client and its registered key is
// sequenced, and so is one naming no host client. Bindings are MG1's, so this runs under sidecar_pending.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A top-level client of the network's owner, and its session.
func newForwardingHostTestClient(t testing.TB, ctx context.Context, networkId server.Id, userId server.Id, label string) *session.ClientSession {
	t.Helper()
	userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: label})
	authResult, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, userSession)
	if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, authResult)
	}
	byJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return session.Testing_CreateClientSession(ctx, byJwt)
}

// A fresh Ed25519 public key.
func newForwardingHostTestKey(t testing.TB) []byte {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

// The owner sidecar's forwarding Delegation at the world's tip, naming hostClientId and hostClientPublicKey, or no
// host client when hostClientId is nil.
func forwardingHostTestDelegation(t testing.TB, world *sidecartest.ConfigLogSuiteWorld, sidecarId sidecar.Id, hostClientId *server.Id, hostClientPublicKey []byte) []byte {
	t.Helper()
	readerKey := make([]byte, 1216)
	if _, err := rand.Read(readerKey); err != nil {
		t.Fatal(err)
	}
	forward := &bridge.ForwardScope{ForwardSidecarId: sidecarId.Bytes(), HostClientPublicKey: hostClientPublicKey}
	if hostClientId != nil {
		forward.HostClientId = sidecar.Id(*hostClientId).Bytes()
	}
	return world.SignAsOwner(bridge.SignedType_SIGNED_DELEGATION, &protocol.Delegation{
		Header:               world.Header(),
		Pubkey:               newForwardingHostTestKey(t),
		ReaderXwingPublicKey: readerKey,
		Forward:              forward,
	})
}

func TestAForwardingDelegationsHostClientIsCheckedAtApi(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// the app device's client, another client of the network, and the sidecar's own client
		sidecarSession, userId := newSidecarBindTestClient(t, ctx, "forwarding-host")
		networkId := sidecarSession.ByJwt.NetworkId
		hostSession := newForwardingHostTestClient(t, ctx, networkId, userId, "forwarding-host")
		otherSession := newForwardingHostTestClient(t, ctx, networkId, userId, "forwarding-host")
		sidecarClientId, hostClientId, otherClientId := *sidecarSession.ByJwt.ClientId, *hostSession.ByJwt.ClientId, *otherSession.ByJwt.ClientId
		keys := newSidecarBindTestKeys(t)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, sidecarSession, 1)}, sidecarSession); err != nil {
			t.Fatal(err)
		}
		sidecarId := sidecar.Id(keys.sidecarId)
		world := sidecartest.NewConfigLogSuiteWorldFor(ctx, nil, sidecar.Id(networkId), sidecar.Id(userId), sidecarId)
		installTestConfigLogSequencer(t, world.Clock())
		signedHeader, rows, signedGenesis, configPushes := world.BaseOne()
		base, err := NetworkConfigProjectionPublish(&NetworkConfigProjectionPublishArgs{SignedHeader: signedHeader, Rows: rows, Genesis: signedGenesis, ConfigPushes: configPushes}, sidecarSession)
		if err != nil || base.N != 1 {
			t.Fatalf("base 1: %v %+v", err, base)
		}
		world.SetTip(&bridge.AuthPin{Version: base.N, Chain: base.Chain})

		hostKey, otherKey := newForwardingHostTestKey(t), newForwardingHostTestKey(t)
		model.SetClientPublicKey(ctx, otherClientId, otherKey)
		post := func(hostId *server.Id, hostPublicKey []byte) (*NetworkConfigPinResult, error) {
			return NetworkConfigDelta(&NetworkConfigDeltaArgs{Signed: forwardingHostTestDelegation(t, world, sidecarId, hostId, hostPublicKey)}, sidecarSession)
		}
		refused := func(name string, hostId *server.Id, hostPublicKey []byte) {
			t.Helper()
			pin, err := post(hostId, hostPublicKey)
			if status, code := configLogRefusal(err); status != http.StatusForbidden || code != "config_refused" {
				t.Errorf("%s: %v %+v", name, err, pin)
			}
		}

		// the sidecar's client is top-level: it has no source client to be a host
		refused("a host client for a top-level sidecar client", &hostClientId, hostKey)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`, sidecarClientId, hostClientId))
		})
		// its source client has registered no key yet
		refused("a host client with no registered key", &hostClientId, hostKey)
		model.SetClientPublicKey(ctx, hostClientId, hostKey)

		refused("another client of the network", &otherClientId, otherKey)
		refused("another key for the source client", &hostClientId, otherKey)
		refused("a short key for the source client", &hostClientId, hostKey[:16])
		stranger := server.NewId()
		refused("a client of no network", &stranger, hostKey)
		refused("a host client key with no host client", nil, hostKey)

		// the twin: the source client and its registered key
		pin, err := post(&hostClientId, hostKey)
		if err != nil || pin.N != 2 {
			t.Fatalf("the source client and its registered key: %v %+v", err, pin)
		}
		world.SetTip(&bridge.AuthPin{Version: pin.N, Chain: pin.Chain})
		// and a Delegation naming no host client has nothing for api to check
		pin, err = post(nil, nil)
		if err != nil || pin.N != 3 {
			t.Fatalf("no host client: %v %+v", err, pin)
		}
		world.SetTip(&bridge.AuthPin{Version: pin.N, Chain: pin.Chain})

		// a newer binding moves the sidecar's current client: the old client's source is no host any more
		rebound := newForwardingHostTestClient(t, ctx, networkId, userId, "forwarding-host")
		if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, rebound, 2)}, rebound); err != nil {
			t.Fatal(err)
		}
		pin, err = NetworkConfigDelta(&NetworkConfigDeltaArgs{Signed: forwardingHostTestDelegation(t, world, sidecarId, &hostClientId, hostKey)}, rebound)
		if status, code := configLogRefusal(err); status != http.StatusForbidden || code != "config_refused" {
			t.Errorf("the host of a client the sidecar no longer binds: %v %+v", err, pin)
		}

		read, err := NetworkConfigProjection(&NetworkConfigProjectionArgs{}, sidecarSession)
		if err != nil || len(read.Entries) != 3 {
			t.Errorf("the log holds %d entries: %v", len(read.Entries), err)
		}
	})
}

// What the check leaves to the log's rule: every statement that is no forwarding Delegation naming a host client.
func TestTheHostClientCheckJudgesOnlyForwardingDelegations(t *testing.T) {
	ctx := context.Background()
	networkId := server.NewId()
	world := sidecartest.NewConfigLogSuiteWorldFor(ctx, nil, sidecar.Id(networkId), sidecar.NewId(), sidecar.NewId())
	for name, signed := range map[string][]byte{
		"bytes that are no Signed":                      {0xff, 0xff, 0xff},
		"a MemberAdmit":                                 world.MemberAdmit(),
		"a forwarding Delegation naming no host client": forwardingHostTestDelegation(t, world, world.OwnerSidecarId(), nil, nil),
		"an agent's Delegation": world.SignAsOwner(bridge.SignedType_SIGNED_DELEGATION, &protocol.Delegation{
			Header: world.Header(), Pubkey: newForwardingHostTestKey(t), ReaderXwingPublicKey: bytes.Repeat([]byte{1}, 1216),
			TagIds: [][]byte{sidecar.NewId().Bytes()}, MaxAdmits: 1, NotAfter: 1,
		}),
	} {
		host, err := configDelegationForwardingHost(signed)
		if host != nil || err != nil {
			t.Errorf("%s: %+v %v", name, host, err)
		}
		if err := configDelegationHostCheck(ctx, networkId, signed); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a forward whose ids are malformed is refused before any lookup
	hostClientId := server.NewId()
	malformed := world.SignAsOwner(bridge.SignedType_SIGNED_DELEGATION, &protocol.Delegation{
		Header: world.Header(), Pubkey: newForwardingHostTestKey(t), ReaderXwingPublicKey: bytes.Repeat([]byte{1}, 1216),
		Forward: &bridge.ForwardScope{ForwardSidecarId: []byte{1, 2, 3}, HostClientId: sidecar.Id(hostClientId).Bytes(), HostClientPublicKey: newForwardingHostTestKey(t)},
	})
	if status, code := configLogRefusal(configDelegationHostCheck(ctx, networkId, malformed)); status != http.StatusForbidden || code != "config_refused" {
		t.Errorf("a malformed forward_sidecar_id: %d %s", status, code)
	}
}
