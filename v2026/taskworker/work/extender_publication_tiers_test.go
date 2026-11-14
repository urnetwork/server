package work

// The publish tick as an open publisher of the tiered directory
// (connect/EXTENDER.md R1, R4; connect/EXTENDER-UPDATE2.md §1.3), end to end
// over the tables: the dns address sets, the TXT records signed for them, and
// the drip's queue. A gated record and a gated canary appear in none of them,
// and a dns canary appears in its own region's dns sets alone, never in the
// drip. The pure sampler rules have their own tests in extender_dns_tiers_test.go.

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestExtenderPublishTickKeepsRestrictedRecordsOutOfOpenChannels(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t, testExtenderDnsConfigYaml()...)
		publisher := stubExtenderDnsPublisher(t)

		bothFamilies := []testExtenderDnsAddressSpec{
			{ipVersion: 4, active: true},
			{ipVersion: 6, active: true},
		}
		// open extenders in two continents, short of a full set in europe
		for _, index := range []int{1, 2, 3} {
			createTestDnsExtender(ctx, index, "us", true, bothFamilies...)
		}
		createTestDnsExtender(ctx, 4, "de", true, bothFamilies...)
		// operator hosts beside them, where a short set would be filled from
		gated := createTestDnsExtender(ctx, 6, "de", true, bothFamilies...)
		if err := model.SetNetworkExtenderDirectoryTier(ctx, gated, connect.ExtenderDirectoryTierGated, false); err != nil {
			t.Fatal(err)
		}
		gatedCanary := createTestDnsExtender(ctx, 7, "us", true, bothFamilies...)
		if err := model.SetNetworkExtenderDirectoryTier(ctx, gatedCanary, connect.ExtenderDirectoryTierGated, false); err != nil {
			t.Fatal(err)
		}
		if err := model.SetNetworkExtenderCanaryChannel(ctx, gatedCanary, connect.ExtenderChannelGated, false); err != nil {
			t.Fatal(err)
		}
		// a dns canary is answered in its own region's sets and nowhere else
		dnsCanary := createTestDnsExtender(ctx, 8, "jp", true, bothFamilies...)
		if err := model.SetNetworkExtenderCanaryChannel(ctx, dnsCanary, connect.ExtenderChannelDns, false); err != nil {
			t.Fatal(err)
		}
		restrictedIps := map[string]string{}
		for _, ipVersion := range []int{4, 6} {
			restrictedIps[testExtenderDnsIp(ipVersion, 6)] = "gated"
			restrictedIps[testExtenderDnsIp(ipVersion, 7)] = "gated canary"
		}
		restrictedKeys := map[string]string{
			string(testDnsFixturePublicKey(6)): "gated",
			string(testDnsFixturePublicKey(7)): "gated canary",
		}
		dnsCanaryIps := []string{testExtenderDnsIp(4, 8), testExtenderDnsIp(6, 8)}

		runTestExtenderPublish(t, ctx)

		batch := publisher.onlyApply(t)
		canaryAnswered := false
		for _, upsertSet := range batch.upsertSets {
			for _, ip := range upsertSet.ips {
				if name, ok := restrictedIps[ip]; ok {
					t.Fatalf("%s holds the %s extender's address", upsertSet.setIdentifier(), name)
				}
				if slices.Contains(dnsCanaryIps, ip) {
					if upsertSet.continentCode != "AS" {
						t.Fatalf("%s holds the dns canary of another region", upsertSet.setIdentifier())
					}
					canaryAnswered = true
				}
			}
			for _, recordBase64 := range upsertSet.records {
				body := testDecodeTxtRecordBody(t, recordBase64)
				if name, ok := restrictedKeys[string(body.PublicKey)]; ok {
					t.Fatalf("%s carries the %s extender's record", upsertSet.setIdentifier(), name)
				}
				if !connect.ExtenderRecordOpen(body) && body.CanaryChannel != connect.ExtenderChannelDns {
					t.Fatalf("%s carries a gated record", upsertSet.setIdentifier())
				}
			}
		}
		if !canaryAnswered {
			t.Fatal("the dns canary was answered in no set of its region")
		}

		// the drip of the same tick queued open records alone
		queued := 0
		for _, publish := range model.Testing_GetNetworkExtenderPublishes(ctx) {
			if publish.ExtenderId == gated || publish.ExtenderId == gatedCanary || publish.ExtenderId == dnsCanary {
				t.Fatalf("the drip queued a %d row for a restricted extender", publish.Kind)
			}
			message := &protocol.ExtenderGossipMessage{}
			if err := proto.Unmarshal(publish.Message, message); err != nil {
				t.Fatal(err)
			}
			body := &protocol.ExtenderRecordBody{}
			if err := proto.Unmarshal(message.GetRecord().GetBody(), body); err != nil {
				t.Fatal(err)
			}
			if !connect.ExtenderRecordOpen(body) {
				t.Fatal("the drip queued a record that is not open")
			}
			queued += 1
		}
		if queued != 4 {
			t.Fatalf("the drip queued %d records, want the four open extenders'", queued)
		}
	})
}

// The identity key createTestDnsExtender gives an index.
func testDnsFixturePublicKey(index int) []byte {
	return []byte(fmt.Sprintf("extender-public-key-dns-%07d", index))
}

// One TXT value: the base64 of a gossip message carrying one record.
func testDecodeTxtRecordBody(t testing.TB, recordBase64 string) *protocol.ExtenderRecordBody {
	t.Helper()
	messageBytes, err := base64.StdEncoding.DecodeString(recordBase64)
	if err != nil {
		t.Fatal(err)
	}
	message := &protocol.ExtenderGossipMessage{}
	if err := proto.Unmarshal(messageBytes, message); err != nil {
		t.Fatal(err)
	}
	body := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(message.GetRecord().GetBody(), body); err != nil {
		t.Fatal(err)
	}
	return body
}
