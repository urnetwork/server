package controller

// Phase 14b (connect/EXTENDER.md P6; connect/EXTENDER-UPDATE2.md §3): an
// activation's camouflage key is signed into the extender's records only after
// the activation passes, and only while the network that posted it holds the
// identity key. The activation and uptime probes front the camouflaged attempt
// with the key, so a splicing (Phase B) extender can still be activated and
// probed.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The fixture's activation with its camouflage key posted.
func testCamouflageActivateArgs(fixture *testExtenderFixture) *ExtenderActivateArgs {
	args := fixture.activateArgs()
	args.RealityPublicKeyHex = hex.EncodeToString(fixture.realityPublicKey)
	return args
}

// A fresh X25519 public key in hex, as another extender with camouflage on
// posts.
func testOtherRealityPublicKeyHex(t testing.TB) string {
	t.Helper()
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(privateKey.PublicKey().Bytes())
}

// Signs the extender's record from the store, as the drip, the dns publisher
// and the release do, and requires the camouflage key it carries.
func requireStoredRecordRealityPublicKey(
	t testing.TB,
	ctx context.Context,
	rootPrivateKey ed25519.PrivateKey,
	extenderId server.Id,
	want []byte,
) {
	t.Helper()
	config, err := EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	extender, addresses := model.GetActiveNetworkExtenderForRecord(ctx, extenderId)
	if extender == nil {
		t.Fatal("the extender has no active record")
	}
	record, _, err := SignExtenderRecord(config, rootPrivateKey, extender, addresses, server.NowUtc())
	if err != nil {
		t.Fatal(err)
	}
	recordBase64, err := encodeExtenderRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	body := verifyTestExtenderRecord(t, rootPrivateKey.Public().(ed25519.PublicKey), recordBase64)
	if !bytes.Equal(body.RealityPublicKey, want) {
		t.Fatalf("a record signed from the store carries camouflage key %x, expected %x", body.RealityPublicKey, want)
	}
}

// A passing activation's key is in the record it answers with and in every
// record signed from the store after it. A passing activation without a key,
// camouflage turned off, clears it.
func TestExtenderActivateSignsTheCamouflageKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
		session := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		result, err := ExtenderActivate(testCamouflageActivateArgs(fixture), session)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation with a camouflage key was refused: %s", result.Error)
		}
		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		if !bytes.Equal(body.RealityPublicKey, fixture.realityPublicKey) {
			t.Fatalf("the activation's record carries camouflage key %x, expected %x", body.RealityPublicKey, fixture.realityPublicKey)
		}
		extenderId := *result.ExtenderId
		requireStoredRecordRealityPublicKey(t, ctx, rootPrivateKey, extenderId, fixture.realityPublicKey)

		result, err = ExtenderActivate(fixture.activateArgs(), session)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation without a camouflage key was refused: %s", result.Error)
		}
		body = verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, len(body.RealityPublicKey), 0)
		requireStoredRecordRealityPublicKey(t, ctx, rootPrivateKey, extenderId, nil)
	})
}

// Refusal one: an activation that does not pass stores and signs no key. The
// extender's earlier record, without a key, stays as it was, and a key whose
// first activation fails leaves no row at all.
func TestExtenderActivateThatDoesNotPassSignsNoCamouflageKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		session := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		activated, err := ExtenderActivate(fixture.activateArgs(), session)
		if err != nil {
			t.Fatal(err)
		}
		if !activated.Activated {
			t.Fatalf("the activation was refused: %s", activated.Error)
		}
		extenderId := *activated.ExtenderId

		// the key posted on a tcp port nothing answers: the probe refuses it
		closedListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedPort := closedListener.Addr().(*net.TCPAddr).Port
		closedListener.Close()
		refusedArgs := testCamouflageActivateArgs(fixture)
		refusedArgs.TcpPort = closedPort
		refusedArgs.Carriers = []string{connect.ExtenderCarrierTcp}
		refused, err := ExtenderActivate(refusedArgs, session)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, refused.Activated, false)
		if len(refused.ProbeSteps) == 0 {
			t.Fatalf("the activation was refused before its probe: %q", refused.Error)
		}
		connect.AssertEqual(t, refused.Record, "")
		stored, _ := model.Testing_GetNetworkExtenderStoredRealityPublicKey(ctx, extenderId)
		if len(stored) != 0 {
			t.Fatalf("a refused activation stored camouflage key %x", stored)
		}
		requireStoredRecordRealityPublicKey(t, ctx, rootPrivateKey, extenderId, nil)

		// a new key whose first activation fails is not stored at all
		keySeed, err := connect.NewExtenderKeySeed()
		if err != nil {
			t.Fatal(err)
		}
		newPublicKey, err := connect.ExtenderPublicKeyFromSeed(keySeed)
		if err != nil {
			t.Fatal(err)
		}
		newRealityPublicKey, err := connect.ExtenderRealityStaticPublicKey(keySeed)
		if err != nil {
			t.Fatal(err)
		}
		refusedArgs.PublicKeyHex = hex.EncodeToString(newPublicKey)
		refusedArgs.RealityPublicKeyHex = hex.EncodeToString(newRealityPublicKey)
		refused, err = ExtenderActivate(refusedArgs, session)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, refused.Activated, false)
		if _, registered := model.GetNetworkExtenderKeyNetworkId(ctx, newPublicKey); registered {
			t.Fatal("a key whose first activation failed was stored")
		}
	})
}

// Refusal two: another network's post of a held key, with a camouflage key of
// its own, is refused before the probe (fix/extender-key-ownership) and stores
// nothing. The owner's key stays, posted by the owner, and its records carry
// it.
func TestExtenderActivateCamouflageKeyOfAnotherNetworkIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		extenderAddress := fixture.clientAddress("127.0.0.1")

		ownerSession := newTestExtenderSession(t, ctx, extenderAddress)
		owned, err := ExtenderActivate(testCamouflageActivateArgs(fixture), ownerSession)
		if err != nil {
			t.Fatal(err)
		}
		if !owned.Activated {
			t.Fatalf("the owner's activation was refused: %s", owned.Error)
		}
		extenderId := *owned.ExtenderId

		otherSession := newTestExtenderSession(t, ctx, extenderAddress)
		otherArgs := fixture.activateArgs()
		otherArgs.RealityPublicKeyHex = testOtherRealityPublicKeyHex(t)
		relayed, err := ExtenderActivate(otherArgs, otherSession)
		if err != nil {
			t.Fatal(err)
		}
		if !testKeyOtherNetworkRefusal(relayed) {
			t.Fatalf("another network's post was not refused as such: activated %t %q", relayed.Activated, relayed.Error)
		}
		connect.AssertEqual(t, len(relayed.ProbeSteps), 0)
		connect.AssertEqual(t, relayed.Record, "")

		stored, postingNetworkId := model.Testing_GetNetworkExtenderStoredRealityPublicKey(ctx, extenderId)
		if !bytes.Equal(stored, fixture.realityPublicKey) {
			t.Fatalf("stored camouflage key %x after another network's post, expected the owner's %x", stored, fixture.realityPublicKey)
		}
		connect.AssertEqual(t, *postingNetworkId, ownerSession.ByJwt.NetworkId)
		requireStoredRecordRealityPublicKey(t, ctx, rootPrivateKey, extenderId, fixture.realityPublicKey)
	})
}

// A key that is not a usable X25519 key, or is not hex, is refused with the
// other arguments: nothing is probed, spent or stored.
func TestExtenderActivateRefusesAnUnusableCamouflageKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		session := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		// u = 1 is a low-order point, like u = 0
		lowOrder := make([]byte, 32)
		lowOrder[0] = 1
		for _, realityPublicKeyHex := range []string{
			"not hex",
			hex.EncodeToString(make([]byte, 31)),
			hex.EncodeToString(make([]byte, 33)),
			hex.EncodeToString(make([]byte, 32)),
			hex.EncodeToString(lowOrder),
		} {
			args := fixture.activateArgs()
			args.RealityPublicKeyHex = realityPublicKeyHex
			result, err := ExtenderActivate(args, session)
			if err != nil {
				t.Fatal(err)
			}
			if result.Activated || !strings.Contains(result.Error, "camouflage key is not readable") {
				t.Fatalf("camouflage key %q: activated %t %q", realityPublicKeyHex, result.Activated, result.Error)
			}
			connect.AssertEqual(t, len(result.ProbeSteps), 0)
		}
		if _, registered := model.GetNetworkExtenderKeyNetworkId(ctx, fixture.publicKey); registered {
			t.Fatal("an activation with an unusable camouflage key was stored")
		}
	})
}

// The key is parsed with the other arguments, ahead of the probe guard and the
// budgets, so an unusable one spends neither. With the guard full it is refused
// for itself and not as busy, and a budget of one activation is still whole
// after it.
func TestExtenderActivateUnusableCamouflageKeySpendsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newTestExtenderFixture(t)
		installTestExtenderConfig(t, fixture.api)
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		settings := DefaultExtenderActivateSettings()
		settings.ClientRateLimit = 1
		settings.MaxConcurrentProbes = 1
		service := newExtenderActivateService(settings)

		unusable := fixture.activateArgs()
		unusable.RealityPublicKeyHex = "not hex"
		post := func(when string) {
			t.Helper()
			for range 3 {
				result, err := service.activate(unusable, clientSession)
				if err != nil {
					t.Fatal(err)
				}
				if result.Activated || !strings.Contains(result.Error, "camouflage key is not readable") {
					t.Fatalf("%s: activated %t %q", when, result.Activated, result.Error)
				}
			}
		}

		// an activation that reached the budget would spend it, and be refused
		// by it from the second post on
		post("with the probe guard free")

		// an activation that reached the guard would be refused as busy
		service.probeSlots <- struct{}{}
		post("with the probe guard full")
		<-service.probeSlots

		// the budget of one is whole, and spent by this activation, so it was
		// live all along
		result, err := service.activate(fixture.activateArgs(), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation after the unusable keys was refused: %s", result.Error)
		}
		result, err = service.activate(fixture.activateArgs(), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.Activated {
			t.Fatal("a budget of one allowed a second activation")
		}
	})
}

// A borrowed site for the splice that accepts and hangs up: a spliced hello
// meets something that is not the extender, and the B3 leaf check fails it.
func newTestBorrowSite(t testing.TB) connect.DialContextFunction {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
}

// The name the splicing fixture verified, and the one the operator's bundled
// list fronts the camouflaged probe with.
const testExtenderBorrowName = "borrow.example"

// A Phase B extender splices the operator's legacy probe to its borrowed site,
// so an activation without the extender's camouflage key is refused at the
// tcp step, and the uptime probe of a stored key-less target fails. With the
// key posted, the activation probe authenticates and passes, the record
// carries the key, and the uptime probe of the stored target passes too.
func TestExtenderActivatePhaseBExtenderNeedsItsCamouflageKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restore := connect.Testing_SetBorrowDomains([]string{testExtenderBorrowName})
		defer restore()
		spliceDial := newTestBorrowSite(t)
		fixture := newTestExtenderFixtureWithSettings(t, 1, func(settings *extender.ExtenderSettings) {
			settings.CamouflageEnabled = true
			settings.ExtenderCamouflageSplice = true
			settings.CamouflageBorrowNames = []string{testExtenderBorrowName}
			settings.CamouflageSpliceDialContext = spliceDial
		})
		rootPrivateKey := installTestExtenderConfig(t, fixture.api)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
		session := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))

		legacy, err := ExtenderActivate(fixture.activateArgs(), session)
		if err != nil {
			t.Fatal(err)
		}
		if legacy.Activated {
			t.Fatal("an activation without the camouflage key passed though the extender splices its probe")
		}
		if fixture.server.CamouflageStats().SplicedCount == 0 {
			t.Fatalf("the legacy probe was not spliced: %q", legacy.Error)
		}

		result, err := ExtenderActivate(testCamouflageActivateArgs(fixture), session)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation with the camouflage key was refused: %s", result.Error)
		}
		if fixture.server.CamouflageStats().AuthenticatedCount == 0 {
			t.Fatal("the activation probe did not authenticate")
		}
		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		if !bytes.Equal(body.RealityPublicKey, fixture.realityPublicKey) {
			t.Fatalf("the record carries camouflage key %x, expected %x", body.RealityPublicKey, fixture.realityPublicKey)
		}

		config, err := EnvExtenderConfig()
		if err != nil {
			t.Fatal(err)
		}
		serverName, err := config.ProbeServerName()
		if err != nil {
			t.Fatal(err)
		}
		apiHost, err := config.ApiHost()
		if err != nil {
			t.Fatal(err)
		}
		probed := 0
		for _, target := range model.GetActiveNetworkExtenderProbeTargets(ctx) {
			if target.ExtenderId != *result.ExtenderId {
				continue
			}
			probed += 1
			if !bytes.Equal(target.RealityPublicKey, fixture.realityPublicKey) {
				t.Fatalf("the probe target carries camouflage key %x, expected %x", target.RealityPublicKey, fixture.realityPublicKey)
			}
			if err := ProbeExtenderUptime(ctx, target, serverName, apiHost); err != nil {
				t.Fatalf("the uptime probe with the stored key failed: %v", err)
			}
			keyless := *target
			keyless.RealityPublicKey = nil
			if err := ProbeExtenderUptime(ctx, &keyless, serverName, apiHost); err == nil {
				t.Fatal("the uptime probe without the key passed though the extender splices it")
			}
		}
		connect.AssertEqual(t, probed, 1)
	})
}
