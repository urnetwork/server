package model

// The publication invariant on the tables (connect/EXTENDER.md R1, R4;
// connect/EXTENDER-UPDATE2.md §1.3): a gated or channel-restricted record is
// never queued for gossip by an activation, never selected or queued by the
// drip (C4), never sampled as an activation's bootstrap, and never counted in
// the population the drip rotates through. The dns sets and the gossip drain
// have their own tests in taskworker/work and gossip.

import (
	"context"
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The extenders of every tier and canary channel an operator can hold, one
// active address each: open, gated, a gated canary, a dns canary of each tier.
type testPublicationFleet struct {
	open            server.Id
	restricted      map[string]server.Id
	restrictedOrder []string
}

func newTestPublicationFleet(t testing.TB, ctx context.Context) *testPublicationFleet {
	t.Helper()
	fleet := &testPublicationFleet{restricted: map[string]server.Id{}}
	_, fleet.open = testOpenPublicationExtender(t, ctx, 0)
	add := func(name string, extenderId server.Id) {
		fleet.restricted[name] = extenderId
		fleet.restrictedOrder = append(fleet.restrictedOrder, name)
	}
	_, gated := testGatedReleaseExtender(t, ctx, 1, "", 4)
	add("gated", gated)
	_, gatedCanary := testGatedReleaseExtender(t, ctx, 2, connect.ExtenderChannelGated, 4)
	add("gated canary", gatedCanary)
	_, gatedDnsCanary := testGatedReleaseExtender(t, ctx, 3, connect.ExtenderChannelDns, 6)
	add("gated dns canary", gatedDnsCanary)
	_, openDnsCanary := testOpenPublicationExtender(t, ctx, 4)
	if err := SetNetworkExtenderCanaryChannel(ctx, openDnsCanary, connect.ExtenderChannelDns, false); err != nil {
		t.Fatal(err)
	}
	add("open dns canary", openDnsCanary)
	return fleet
}

// An open tier extender with one active, probed v4 address.
func testOpenPublicationExtender(t testing.TB, ctx context.Context, index int) (string, server.Id) {
	t.Helper()
	keyHex, extenderId := testGatedReleaseExtender(t, ctx, index, "", 4)
	if err := SetNetworkExtenderDirectoryTier(ctx, extenderId, connect.ExtenderDirectoryTierOpen, false); err != nil {
		t.Fatal(err)
	}
	return keyHex, extenderId
}

// Root cause guarded: an operator host on an open channel. Neither the drip's
// selection nor its publish, called with a restricted extender directly as a
// tier change between the two would, queues anything but the open record.
func TestExtenderDripNeverQueuesARestrictedRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fleet := newTestPublicationFleet(t, ctx)
		sign, _ := testRecordSigner()

		selected := GetNetworkExtenderIdsForPublish(ctx, 100, server.NowUtc().Add(24*time.Hour))
		if !slices.Equal(selected, []server.Id{fleet.open}) {
			t.Fatalf("the drip selected %v, want the open extender alone", selected)
		}
		if count := CountActiveNetworkExtenders(ctx); count != 1 {
			t.Fatalf("the drip rotates through %d extenders, want the open one", count)
		}
		for _, name := range fleet.restrictedOrder {
			if PublishNetworkExtenderRecord(ctx, fleet.restricted[name], sign) {
				t.Fatalf("the drip published the %s record", name)
			}
		}
		if !PublishNetworkExtenderRecord(ctx, fleet.open, sign) {
			t.Fatal("the drip did not publish the open record")
		}
		for _, publish := range Testing_GetNetworkExtenderPublishes(ctx) {
			if publish.ExtenderId != fleet.open {
				t.Fatalf("a %d row was queued for a restricted extender", publish.Kind)
			}
		}
	})
}

// An activation's bootstrap sample is drawn from the open tier alone, however
// many restricted extenders there are to draw.
func TestExtenderActivationBootstrapNeverSamplesARestrictedRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fleet := newTestPublicationFleet(t, ctx)
		for range 8 {
			sampled := GetRandomActiveNetworkExtenders(ctx, 100, server.Id{})
			if len(sampled) != 1 || sampled[0].Extender.ExtenderId != fleet.open {
				ids := []server.Id{}
				for _, extender := range sampled {
					ids = append(ids, extender.Extender.ExtenderId)
				}
				t.Fatalf("the bootstrap sampled %v, want the open extender alone", ids)
			}
		}
		if sampled := GetRandomActiveNetworkExtenders(ctx, 100, fleet.open); len(sampled) != 0 {
			t.Fatalf("excluding the open extender sampled %d restricted ones", len(sampled))
		}
	})
}

// An activation into the gated tier queues no record for gossip, and neither
// does the re-activation of a canary, whatever its tier; an open activation
// does. Revocations name no address and may go anywhere, so only records are
// counted.
func TestExtenderActivationNeverQueuesARestrictedRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()
		recordRows := func(extenderId server.Id) int {
			count := 0
			for _, publish := range Testing_GetNetworkExtenderPublishes(ctx) {
				if publish.ExtenderId == extenderId && publish.Kind == NetworkExtenderPublishKindRecord {
					count += 1
				}
			}
			return count
		}
		key := func(name string) []byte {
			return []byte("synthetic-publication-key-" + name)
		}

		open := ActivateNetworkExtender(ctx, testExtenderActivation(key("open"), 4, "192.0.2.10"), sign)
		if recordRows(open.Extender.ExtenderId) != 1 {
			t.Fatal("an open activation queued no record")
		}

		gatedActivation := testExtenderActivation(key("gated"), 4, "192.0.2.11")
		gatedActivation.DirectoryTier = connect.ExtenderDirectoryTierGated
		gated := ActivateNetworkExtender(ctx, gatedActivation, sign)
		if gated.Extender.DirectoryTier != connect.ExtenderDirectoryTierGated || recordRows(gated.Extender.ExtenderId) != 0 {
			t.Fatal("a gated activation queued a record for gossip")
		}
		// the same key again, from the other family
		gatedActivation = testExtenderActivation(key("gated"), 6, "2001:db8::11")
		gatedActivation.DirectoryTier = connect.ExtenderDirectoryTierGated
		ActivateNetworkExtender(ctx, gatedActivation, sign)
		if recordRows(gated.Extender.ExtenderId) != 0 {
			t.Fatal("a gated re-activation queued a record for gossip")
		}

		for _, channel := range []string{connect.ExtenderChannelDns, connect.ExtenderChannelGated} {
			canary := ActivateNetworkExtender(ctx, testExtenderActivation(key("canary-"+channel), 4, "192.0.2.12"), sign)
			before := recordRows(canary.Extender.ExtenderId)
			if err := SetNetworkExtenderCanaryChannel(ctx, canary.Extender.ExtenderId, channel, false); err != nil {
				t.Fatal(err)
			}
			// the designation outlives a re-activation into the open tier
			reactivated := ActivateNetworkExtender(ctx, testExtenderActivation(key("canary-"+channel), 4, "192.0.2.12"), sign)
			if reactivated.Extender.CanaryChannel != channel || recordRows(canary.Extender.ExtenderId) != before {
				t.Fatalf("re-activating a %s canary queued a record for gossip", channel)
			}
		}

		// the keys the operator holds are the ones it was given
		if hex.EncodeToString(gated.Extender.PublicKey) != hex.EncodeToString(key("gated")) {
			t.Fatal("the gated activation stored another key")
		}
	})
}
