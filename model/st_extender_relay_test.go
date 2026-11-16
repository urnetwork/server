// The relay bytes rule of the SN25 extender pool's relay half (sn WHITEPAPER.md
// §8.6.2, owner 2026-11-16; st_extender_relay.go): an extender party's share
// of a contract counts as its relay bytes only when it stays provider usage
// under the amendment and the contract was funded by a payer that is no
// measurement network. The tests are arithmetic over usage snapshots, so a
// failure names the rule that broke.
package model

import (
	"errors"
	"maps"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One contract of the earning window as the relay read hands it to its reader:
// the live row, its snapshot of providers, and the provider clients of its
// extender parties.
type stRelayTestContract struct {
	name      string
	payer     *server.Id
	origin    server.Id
	providers []contractProviderUsage
	parties   []server.Id
}

func (self *stRelayTestContract) row() (*stEpochUsageRow, *contractUsageSnapshot) {
	var total ByteCount
	for _, provider := range self.providers {
		total += provider.ByteCount
	}
	origin, created, closed := self.origin, stExtenderTestCreateTime, stExtenderTestCreateTime.Add(time.Minute)
	row := &stEpochUsageRow{contractId: server.NewId(), closedAt: &closed, live: true, originNetworkId: &origin, payerNetworkId: self.payer, createTime: &created}
	return row, &contractUsageSnapshot{Version: 1, ByteCount: total, Providers: self.providers}
}

// An extender party's share that stays provider usage is its relay bytes; a
// share the amendment pools is not, whether it is on the origin's network or a
// platform endpoint's; another provider's share of the contract is not, nor is
// any share of a contract no balance funded or a measurement network paid for,
// or of a contract with no bytes. A provider client named by several of a
// contract's parties counts its share once. A contract counts when it adds any
// relay bytes.
func TestStEpochRelayCountsAnExtenderPartysUnpooledShare(t *testing.T) {
	ext, ext2, edge, receiver, provider := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	extNetwork, ext2Network, edgeNetwork, origin, providerNetwork := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	funded, measurement := server.NewId(), server.NewId()
	share := func(clientId server.Id, networkId server.Id, bytes ByteCount) contractProviderUsage {
		return contractProviderUsage{ClientId: clientId, NetworkId: networkId, ByteCount: bytes}
	}
	contracts := []*stRelayTestContract{
		{name: "an extender party's hop, and a party's share on the origin's network",
			payer: &funded, origin: origin, providers: []contractProviderUsage{share(ext, extNetwork, 500), share(receiver, origin, 300)}, parties: []server.Id{ext, receiver}},
		{name: "a platform endpoint's share, pooled even off the origin's network",
			payer: &funded, origin: origin, providers: []contractProviderUsage{share(edge, edgeNetwork, 200), share(ext2, ext2Network, 100)}, parties: []server.Id{edge, ext2}},
		{name: "a provider that is no party",
			payer: &funded, origin: origin, providers: []contractProviderUsage{share(ext, extNetwork, 50), share(provider, providerNetwork, 70)}, parties: []server.Id{ext}},
		{name: "a party named for both sides",
			payer: &funded, origin: origin, providers: []contractProviderUsage{share(ext2, ext2Network, 9)}, parties: []server.Id{ext2, ext2}},
		{name: "unfunded",
			payer: nil, origin: origin, providers: []contractProviderUsage{share(ext, extNetwork, 1_000)}, parties: []server.Id{ext}},
		{name: "paid by a measurement network",
			payer: &measurement, origin: origin, providers: []contractProviderUsage{share(ext, extNetwork, 2_000)}, parties: []server.Id{ext}},
		{name: "no bytes",
			payer: &funded, origin: origin, providers: []contractProviderUsage{}, parties: []server.Id{ext}},
		{name: "no party",
			payer: &funded, origin: origin, providers: []contractProviderUsage{share(ext, extNetwork, 4_000)}},
	}
	reader := newStEpochRelayReader(8, stExtenderAmendmentTestPolicy(edge), map[server.Id]bool{measurement: true})
	for _, contract := range contracts {
		row, snapshot := contract.row()
		if err := reader.add(row, snapshot, contract.parties); err != nil {
			t.Fatalf("%s: %v", contract.name, err)
		}
	}
	want := map[server.Id]ByteCount{ext: 500 + 50, ext2: 100 + 9}
	if !maps.Equal(reader.relay.ClientIdRelayBytes, want) || reader.relay.Contracts != 4 || reader.relay.Bytes() != 659 {
		t.Fatalf("relay %v in %d contracts, want %v in 4", reader.relay.ClientIdRelayBytes, reader.relay.Contracts, want)
	}

	// slices merge to the totals of one reader
	first := newStEpochRelayReader(8, stExtenderAmendmentTestPolicy(edge), map[server.Id]bool{measurement: true})
	second := newStEpochRelayReader(8, stExtenderAmendmentTestPolicy(edge), map[server.Id]bool{measurement: true})
	for i, contract := range contracts {
		row, snapshot := contract.row()
		part := first
		if i%2 == 1 {
			part = second
		}
		if err := part.add(row, snapshot, contract.parties); err != nil {
			t.Fatal(err)
		}
	}
	merged := newStEpochRelayReader(8, stExtenderAmendmentTestPolicy(edge), nil)
	if err := errors.Join(merged.merge(first), merged.merge(second)); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(merged.relay.ClientIdRelayBytes, want) || merged.relay.Contracts != 4 {
		t.Fatalf("merged relay %v in %d contracts, want %v in 4", merged.relay.ClientIdRelayBytes, merged.relay.Contracts, want)
	}
}

// A funded contract with bytes needs its usage origin and create time to
// classify its shares, as the amended usage read does; a relay total past the
// byte count's range is refused rather than wrapped.
func TestStEpochRelayRefusesWhatItCannotClassify(t *testing.T) {
	ext, extNetwork, origin, funded := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	contract := &stRelayTestContract{payer: &funded, origin: origin, providers: []contractProviderUsage{{ClientId: ext, NetworkId: extNetwork, ByteCount: 10}}, parties: []server.Id{ext}}
	reader := newStEpochRelayReader(8, stExtenderAmendmentTestPolicy(), nil)
	row, snapshot := contract.row()
	row.originNetworkId = nil
	if err := reader.add(row, snapshot, contract.parties); err == nil || !strings.Contains(err.Error(), "no usage origin") {
		t.Fatalf("a contract without its origin: %v", err)
	}
	row, snapshot = contract.row()
	row.createTime = nil
	if err := reader.add(row, snapshot, contract.parties); err == nil || !strings.Contains(err.Error(), "no create time") {
		t.Fatalf("a contract without its create time: %v", err)
	}
	if len(reader.relay.ClientIdRelayBytes) != 0 || reader.relay.Contracts != 0 {
		t.Fatalf("refused contracts counted %+v", reader.relay)
	}
	reader.relay.ClientIdRelayBytes[ext] = math.MaxInt64 - 5
	row, snapshot = contract.row()
	if err := reader.add(row, snapshot, contract.parties); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("an overflowing total: %v", err)
	}
	other := newStEpochRelayReader(8, stExtenderAmendmentTestPolicy(), nil)
	other.relay.ClientIdRelayBytes[ext] = 6
	if err := reader.merge(other); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("an overflowing merge: %v", err)
	}
}
