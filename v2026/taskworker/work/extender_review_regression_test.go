// Covers the server's canary-only discovery and signed redistribution boundaries.
package work

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Ordinary peers in another region cannot hide a region's only healthy canary.
func TestReviewDnsCanaryIsPublishedWithoutOrdinaryRegionalPeers(t *testing.T) {
	canary := testExtenderDnsTieredAddress("DE", 4, 1, connect.ExtenderDirectoryTierOpen, connect.ExtenderChannelDns)
	addresses := append(testExtenderDnsAddresses("US", 4, 2, 3, 4), canary)
	sets := sampleExtenderDnsRecordSets(addresses, 3, testExtenderDnsSecret, 1, nil)
	for _, set := range sets {
		if slices.Contains(set.ips, canary.Ip.String()) {
			return
		}
	}
	t.Fatal("healthy DNS canary is omitted entirely when it is its region's only extender")
}

// The production signer and DNS decoder preserve an exclusive discovery channel
// through directory caching, feed sampling and stream notification.
func TestReviewDnsCanaryDoesNotLeakIntoFeed(t *testing.T) {
	root := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	extenderSeed := make([]byte, ed25519.SeedSize)
	extenderSeed[0] = 1
	extenderKey := ed25519.NewKeyFromSeed(extenderSeed).Public().(ed25519.PublicKey)
	now := time.Unix(1700000000, 0)
	_, messageBytes, err := controller.SignExtenderRecord(
		&controller.ExtenderConfig{NetworkHost: "review.example"}, root,
		&model.NetworkExtender{PublicKey: extenderKey, TcpPort: 443, UdpPort: 443, DnsPort: 53, DnsTld: "carrier.example.", DirectoryTier: connect.ExtenderDirectoryTierOpen, CanaryChannel: connect.ExtenderChannelDns},
		[]*model.NetworkExtenderAddress{{IpVersion: 4, Ip: netip.MustParseAddr("192.0.2.1"), Carriers: []string{connect.ExtenderCarrierTcp}, Active: true}}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	message, err := connect.DecodeExtenderDnsRecord(base64.StdEncoding.EncodeToString(messageBytes))
	if err != nil {
		t.Fatal(err)
	}
	settings := connect.DefaultExtenderDirectorySettings()
	settings.Now = func() time.Time { return now }
	settings.NetworkHosts = []string{"review.example"}
	directory := connect.NewExtenderDirectory(context.Background(), settings)
	defer directory.Close()
	directory.SetRootKeys(connect.NewExtenderRootKeySet(root.Public().(ed25519.PublicKey)))
	body, err := directory.RootKeys().VerifyRecord(message.GetRecord())
	if err != nil {
		t.Fatal(err)
	}
	if body.CanaryChannel != connect.ExtenderChannelDns || !connect.ExtenderRecordGated(body) {
		t.Fatalf("canary lacks signed channel and legacy relay protection: %v", body)
	}
	messages, unsubscribe := directory.Subscribe()
	defer unsubscribe()
	if _, err := directory.ApplySource(message, connect.ExtenderSourceDns); err != nil {
		t.Fatal(err)
	}
	if got := directory.SampleRecords(8, nil, []byte("review-vantage")); len(got) != 0 {
		t.Fatal("server-signed DNS-only canary was offered on the client feed after the normal DNS bootstrap path")
	}
	select {
	case <-messages:
		t.Fatal("DNS canary entered the feed stream")
	default:
	}
	if directory.OpenPartitionContains([]byte("review-vantage"), extenderKey) {
		t.Fatal("DNS canary entered the feed partition")
	}
}

// A region/family containing only canaries still publishes its address and TXT
// sets, while unrelated regions and the default do not learn that canary.
func TestExtenderDnsCanaryOnlyFamilyIsPublishedInItsRegion(t *testing.T) {
	for _, version := range []int{4, 6} {
		canary := testExtenderDnsTieredAddress("DE", version, 1, connect.ExtenderDirectoryTierOpen, connect.ExtenderChannelDns)
		sets := sampleExtenderDnsRecordSets([]*model.NetworkExtenderDnsAddress{canary}, 3, testExtenderDnsSecret, 1,
			func(_ server.Id) (string, bool) { return "synthetic-signed-record", true })
		addressSets, recordSets := 0, 0
		for _, set := range sets {
			if set.continentCode != "EU" {
				t.Fatalf("family %d canary leaked to %q", version, set.continentCode)
			}
			if slices.Equal(set.ips, []string{canary.Ip.String()}) {
				addressSets++
			}
			if slices.Equal(set.records, []string{"synthetic-signed-record"}) {
				recordSets++
			}
		}
		if addressSets != 1 || recordSets != 1 {
			t.Fatalf("family %d produced %d address and %d TXT sets", version, addressSets, recordSets)
		}
	}
}
