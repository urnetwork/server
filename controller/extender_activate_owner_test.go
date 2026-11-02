package controller

// An extender key belongs to the network that first activated it
// (model.ActivateNetworkExtender). The activation probes prove that the
// caller's address answers for the posted key, not that the caller holds it:
// an open extender signs any challenge and forwards any request to the api
// (connect/EXTENDER.md A4, A5). Every post here arrives from the fixture
// extender's own address, as a post relayed through it does, and the fixture
// answers the probes for its key whoever posted it.

import (
	"context"
	"encoding/hex"
	"net"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func testKeyOtherNetworkRefusal(result *ExtenderActivateResult) bool {
	return !result.Activated && strings.Contains(result.Error, "registered to another network")
}

// The review repro (fix/extender-activation-budget, connect#230): a post with
// another network's extender key, relayed through that extender, passed every
// probe and moved the extender row -- its network, its client, the record and
// the contract parties the row names -- to the poster's network. The post is
// refused before anything is probed, and the row stays its owner's.
func TestExtenderActivateRefusesAKeyRelayedFromAnotherNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		extenderAddress := fixture.clientAddress("127.0.0.1")

		ownerSession := newTestExtenderSession(t, ctx, extenderAddress)
		owned, err := ExtenderActivate(fixture.activateArgs(), ownerSession)
		if err != nil {
			t.Fatal(err)
		}
		if !owned.Activated {
			t.Fatalf("the owner's activation was refused: %s", owned.Error)
		}
		extenderId := testExtenderIdForKey(ctx, t, fixture.publicKey)
		publishCount := len(model.Testing_GetNetworkExtenderPublishes(ctx))

		// another account's client, posting the owner's key from the
		// extender's address
		otherSession := newTestExtenderSession(t, ctx, extenderAddress)
		relayed, err := ExtenderActivate(fixture.activateArgs(), otherSession)
		if err != nil {
			t.Fatal(err)
		}
		stored := model.Testing_GetNetworkExtender(ctx, extenderId)
		t.Logf(
			"relayed post: activated %t %q; the row's network is the poster's: %t",
			relayed.Activated,
			relayed.Error,
			stored.Extender.NetworkId == otherSession.ByJwt.NetworkId,
		)
		if !testKeyOtherNetworkRefusal(relayed) {
			t.Fatalf("the relayed post with another network's key was not refused as such: activated %t %q", relayed.Activated, relayed.Error)
		}
		// refused before the probe dialed anything
		connect.AssertEqual(t, len(relayed.ProbeSteps), 0)
		connect.AssertEqual(t, relayed.Record, "")

		connect.AssertEqual(t, stored.Extender.NetworkId, ownerSession.ByJwt.NetworkId)
		connect.AssertEqual(t, stored.Extender.ClientId, *ownerSession.ByJwt.ClientId)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), publishCount)

		// the owner's own re-activation is untouched by the refusal
		reactivated, err := ExtenderActivate(fixture.activateArgs(), ownerSession)
		if err != nil {
			t.Fatal(err)
		}
		if !reactivated.Activated {
			t.Fatalf("the owner's re-activation was refused: %s", reactivated.Error)
		}
		connect.AssertEqual(t, *reactivated.ExtenderId, extenderId)
		stored = model.Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Extender.NetworkId, ownerSession.ByJwt.NetworkId)
	})
}

// A refusal for the key's network spends none of the poster's budget: with a
// budget of one activation, three posts of another network's key leave it
// whole, and the poster's next activation of a key of its own is judged by the
// probe rather than refused by the budget.
func TestExtenderActivateKeyOtherNetworkRefusalSpendsNoBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		extenderAddress := fixture.clientAddress("127.0.0.1")

		ownerSession := newTestExtenderSession(t, ctx, extenderAddress)
		owned, err := ExtenderActivate(fixture.activateArgs(), ownerSession)
		if err != nil {
			t.Fatal(err)
		}
		if !owned.Activated {
			t.Fatalf("the owner's activation was refused: %s", owned.Error)
		}

		settings := DefaultExtenderActivateSettings()
		settings.ClientRateLimit = 1
		otherSession := newTestExtenderSession(t, ctx, extenderAddress)
		for i := range 3 {
			relayed, err := extenderActivate(fixture.activateArgs(), otherSession, settings)
			if err != nil {
				t.Fatal(err)
			}
			if !testKeyOtherNetworkRefusal(relayed) {
				t.Fatalf("post %d of another network's key: activated %t %q", i+1, relayed.Activated, relayed.Error)
			}
		}

		// a key of the poster's own on a closed tcp port: the probe refuses it,
		// so the budget had room for it
		keySeed, err := connect.NewExtenderKeySeed()
		if err != nil {
			t.Fatal(err)
		}
		publicKey, err := connect.ExtenderPublicKeyFromSeed(keySeed)
		if err != nil {
			t.Fatal(err)
		}
		closedListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedPort := closedListener.Addr().(*net.TCPAddr).Port
		closedListener.Close()
		own := fixture.activateArgs()
		own.PublicKeyHex = hex.EncodeToString(publicKey)
		own.TcpPort = closedPort
		own.Carriers = []string{connect.ExtenderCarrierTcp}
		result, err := extenderActivate(own, otherSession, settings)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Activated, false)
		if strings.Contains(result.Error, "maximum number of attempts") {
			t.Fatalf("the refusals for another network's key spent the budget: %q", result.Error)
		}
		if len(result.ProbeSteps) == 0 {
			t.Fatalf("the poster's own key was refused before the probe: %q", result.Error)
		}
	})
}

// Another client of the owning network re-activates the key and becomes its
// client, as a reinstall on the same account does: only another network is
// refused.
func TestExtenderActivateKeepsAKeyInItsNetworkForAnotherClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		extenderAddress := fixture.clientAddress("127.0.0.1")

		firstSession := newTestExtenderSession(t, ctx, extenderAddress)
		first, err := ExtenderActivate(fixture.activateArgs(), firstSession)
		if err != nil {
			t.Fatal(err)
		}
		if !first.Activated {
			t.Fatalf("the first client's activation was refused: %s", first.Error)
		}

		secondSession := newTestExtenderAccountSession(ctx, firstSession)
		second, err := ExtenderActivate(fixture.activateArgs(), secondSession)
		if err != nil {
			t.Fatal(err)
		}
		if !second.Activated {
			t.Fatalf("another client of the owning network was refused: %s", second.Error)
		}
		connect.AssertEqual(t, *second.ExtenderId, *first.ExtenderId)
		stored := model.Testing_GetNetworkExtender(ctx, *first.ExtenderId)
		connect.AssertEqual(t, stored.Extender.NetworkId, firstSession.ByJwt.NetworkId)
		connect.AssertEqual(t, stored.Extender.ClientId, *secondSession.ByJwt.ClientId)
	})
}
