package controller

// The camouflage key through the capability issuance, end to end
// (connect/EXTENDER-UPDATE2.md §3.2, contract R, phase 14b): a real activation of
// an in-process extender, the record it is answered with, and the records the
// release route signs from the store. A gated extender in a tcp-camouflage-v1
// cohort requires the capability exactly while its record carries the key its
// latest passing activation posted. A refused activation changes neither, and an
// open extender in the cohort never requires anything.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// As installTestExtenderConfig, with the directory block lines given.
func installTestExtenderDirectoryConfig(t testing.TB, api *testExtenderApi, directoryLines ...string) ed25519.PrivateKey {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		fmt.Sprintf("root_private_key_hex: %s", connect.ExtenderKeySeedHex(rootKeySeed)),
		"root_public_keys_hex:",
		fmt.Sprintf("  - %s", hex.EncodeToString(rootPrivateKey.Public().(ed25519.PublicKey))),
		fmt.Sprintf("network_host: %s", testExtenderNetworkHost),
		"network_hosts:",
		fmt.Sprintf("  - %s", testExtenderNetworkHost),
		fmt.Sprintf("  - %s", testExtenderApiHost),
		fmt.Sprintf("api_url: https://%s", testExtenderApiHost),
		"dns:",
		"  enabled: false",
		"directory:",
	}
	lines = append(lines, directoryLines...)
	installTestExtenderConfigYaml(t, strings.Join(lines, "\n"))
	previousTlsConfig := extenderForwardProbeTlsConfig
	extenderForwardProbeTlsConfig = func() *tls.Config {
		return &tls.Config{RootCAs: api.rootCAs}
	}
	t.Cleanup(func() {
		extenderForwardProbeTlsConfig = previousTlsConfig
	})
	return rootPrivateKey
}

func TestExtenderActivateCamouflageKeyDecidesTheCohortRequirement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		required := []string{connect.ExtenderCapabilityTcpCamouflageV1}

		// a gated extender in the cohort: its owner's network is durable
		gated := newTestExtenderFixtureWithSettings(t, 1, func(settings *extender.ExtenderSettings) {
			// every activation here dials from one address, several times per
			// carrier, more often than a subnet's default budget of actions
			settings.AdmissionActionsPerSubnetPerMinute = 1024
		})
		ownerSession := newTestExtenderSession(t, ctx, gated.clientAddress("127.0.0.1"))
		gatedKeyHex := hex.EncodeToString(gated.publicKey)
		directoryLines := append(
			[]string{
				"  durable_network_ids:",
				"    - " + ownerSession.ByJwt.NetworkId.String(),
			},
			testReleaseCohortLines(connect.ExtenderCapabilityTcpCamouflageV1, gatedKeyHex)...,
		)
		rootPrivateKey := installTestExtenderDirectoryConfig(t, gated.api, directoryLines...)
		rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
		rootKeys := connect.NewExtenderRootKeySet(rootPublicKey)

		metrics := newExtenderReleaseMetrics(prometheus.NewRegistry())
		release := func(clientSession *session.ClientSession, capabilities ...string) *ExtenderReleaseResult {
			t.Helper()
			result, err := extenderRelease(&ExtenderReleaseArgs{Capabilities: capabilities}, clientSession, metrics)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		activate := func(args *ExtenderActivateArgs, clientSession *session.ClientSession) *ExtenderActivateResult {
			t.Helper()
			result, err := ExtenderActivate(args, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		// what a client without the capability is dealt: the record with no
		// requirement, or incompatible while the record requires it
		requireReleaseToLegacyClient := func(wantRequirement bool) {
			t.Helper()
			legacy := release(newTestReleaseSession(t, ctx, true))
			if wantRequirement {
				if legacy.Outcome != connect.ExtenderReleaseOutcomeIncompatible || len(legacy.Records) != 0 {
					t.Fatalf("a client without the capability was dealt %+v", legacy)
				}
				return
			}
			body := testReleaseBodies(t, rootKeys, legacy)[gatedKeyHex]
			if legacy.Outcome != connect.ExtenderReleaseOutcomeOk || body == nil || len(body.RequiredCapabilities) != 0 || len(body.RealityPublicKey) != 0 {
				t.Fatalf("a client without the capability was dealt %+v, record %v", legacy, body)
			}
		}

		// the first passing activation posts no key: no key, no requirement
		result := activate(gated.activateArgs(), ownerSession)
		if !result.Activated {
			t.Fatalf("the activation without a key was refused: %s", result.Error)
		}
		body := verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		connect.AssertEqual(t, int(body.DirectoryTier), connect.ExtenderDirectoryTierGated)
		if len(body.RealityPublicKey) != 0 || len(body.RequiredCapabilities) != 0 {
			t.Fatalf("a record with no key posted carries key %x and requires %v", body.RealityPublicKey, body.RequiredCapabilities)
		}
		requireReleaseToLegacyClient(false)

		// refusal one: the key posted on a tcp port nothing answers. The
		// activation does not pass, so the record stays as it was
		closedListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedPort := closedListener.Addr().(*net.TCPAddr).Port
		closedListener.Close()
		refusedArgs := testCamouflageActivateArgs(gated)
		refusedArgs.TcpPort = closedPort
		refusedArgs.Carriers = []string{connect.ExtenderCarrierTcp}
		if refused := activate(refusedArgs, ownerSession); refused.Activated || len(refused.ProbeSteps) == 0 {
			t.Fatalf("the activation on a closed port: activated %t %q", refused.Activated, refused.Error)
		}
		requireReleaseToLegacyClient(false)

		// refusal two: another network's post of the identity with the key
		otherSession := newTestExtenderSession(t, ctx, gated.clientAddress("127.0.0.1"))
		if relayed := activate(testCamouflageActivateArgs(gated), otherSession); !testKeyOtherNetworkRefusal(relayed) {
			t.Fatalf("another network's post: activated %t %q", relayed.Activated, relayed.Error)
		}
		requireReleaseToLegacyClient(false)

		// the owner's passing activation with the key: the record it answers
		// with carries the key and requires the capability
		result = activate(testCamouflageActivateArgs(gated), ownerSession)
		if !result.Activated {
			t.Fatalf("the activation with the key was refused: %s", result.Error)
		}
		body = verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		if !bytes.Equal(body.RealityPublicKey, gated.realityPublicKey) || !slices.Equal(body.RequiredCapabilities, required) {
			t.Fatalf("the record carries key %x and requires %v, expected %x and %v", body.RealityPublicKey, body.RequiredCapabilities, gated.realityPublicKey, required)
		}

		// the release signs the same from the store, and hands it only to a
		// client that declares the capability
		requireReleaseToLegacyClient(true)
		capable := release(newTestReleaseSession(t, ctx, true), connect.ExtenderCapabilityTcpCamouflageV1)
		capableBody := testReleaseBodies(t, rootKeys, capable)[gatedKeyHex]
		if capable.Outcome != connect.ExtenderReleaseOutcomeOk || capableBody == nil {
			t.Fatalf("a capable client was dealt %+v", capable)
		}
		if !bytes.Equal(capableBody.RealityPublicKey, gated.realityPublicKey) || !slices.Equal(capableBody.RequiredCapabilities, required) {
			t.Fatalf("the released record carries key %x and requires %v", capableBody.RealityPublicKey, capableBody.RequiredCapabilities)
		}

		// rollback: the key comes out, a passing activation without one clears
		// the requirement with it, and a client without the capability is dealt
		// the record again
		result = activate(gated.activateArgs(), ownerSession)
		if !result.Activated {
			t.Fatalf("the activation without a key was refused: %s", result.Error)
		}
		body = verifyTestExtenderRecord(t, rootPublicKey, result.Record)
		if len(body.RealityPublicKey) != 0 || len(body.RequiredCapabilities) != 0 {
			t.Fatalf("after the key came out the record carries key %x and requires %v", body.RealityPublicKey, body.RequiredCapabilities)
		}
		requireReleaseToLegacyClient(false)
	})
}

// An open extender in the cohort signs its key and requires nothing: an open
// channel cannot ask a client what it can do.
func TestExtenderActivateCamouflageKeyOfAnOpenCohortMemberRequiresNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		open := newTestExtenderFixture(t)
		clientSession := newTestExtenderSession(t, ctx, open.clientAddress("127.0.0.1"))
		rootPrivateKey := installTestExtenderDirectoryConfig(
			t,
			open.api,
			testReleaseCohortLines(connect.ExtenderCapabilityTcpCamouflageV1, hex.EncodeToString(open.publicKey))...,
		)
		result, err := ExtenderActivate(testCamouflageActivateArgs(open), clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Activated {
			t.Fatalf("the activation was refused: %s", result.Error)
		}
		body := verifyTestExtenderRecord(t, rootPrivateKey.Public().(ed25519.PublicKey), result.Record)
		connect.AssertEqual(t, int(body.DirectoryTier), connect.ExtenderDirectoryTierOpen)
		if !bytes.Equal(body.RealityPublicKey, open.realityPublicKey) || len(body.RequiredCapabilities) != 0 {
			t.Fatalf("the open record carries key %x and requires %v", body.RealityPublicKey, body.RequiredCapabilities)
		}
	})
}
