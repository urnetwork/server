// The SN25 extender amendment's epoch reader (sn WHITEPAPER.md §8.6.2): the
// pure accounting each contract of an epoch's usage statement goes through.
// A failure names the rule rather than a database, a clock or an ordering
// accident.
package model

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// An amended reader row of a live contract with the snapshot of its hops.
type stAmendedTestContract struct {
	name     string
	bytes    ByteCount
	origin   ContractParticipant
	egress   ContractParticipant
	hops     []ContractParticipant
	payer    *server.Id
	archived bool
}

func stAmendedTestAdd(t *testing.T, reader *stEpochAmendedUsage, c stAmendedTestContract) error {
	t.Helper()
	snapshot, _ := stExtenderTestSnapshot(t, c.bytes, c.origin, c.egress, c.hops...)
	origin := c.origin.NetworkId
	row := &stEpochUsageRow{contractId: contractPayoutTestId(250), live: !c.archived, originNetworkId: &origin, payerNetworkId: c.payer}
	return reader.add(row, snapshot)
}

func stTestNetwork(n byte) *server.Id {
	id := contractPayoutTestId(n)
	return &id
}

// §8.6.5's three contracts read for epoch 8, funded by their origins: c1's
// three hop shares stay provider usage; c2's extender keeps its half and
// tlsedge's half is intra-platform; c3's extender keeps its half and the
// receiving client's half is intra-network. Z is the two pooled shares,
// 2,000,000,002 bytes from two contracts of two payers, and every byte of
// the epoch is in exactly one place.
func TestStEpochAmendedUsageWorkedExample(t *testing.T) {
	p := ContractParticipant{ClientId: contractPayoutTestId(10), NetworkId: contractPayoutTestId(110)}
	q := ContractParticipant{ClientId: contractPayoutTestId(11), NetworkId: contractPayoutTestId(111)}
	x := ContractParticipant{ClientId: contractPayoutTestId(12), NetworkId: contractPayoutTestId(240)}
	tlsedge := ContractParticipant{ClientId: contractPayoutTestId(5), NetworkId: contractPayoutTestId(160)}
	o2, o3 := contractPayoutTestId(102), contractPayoutTestId(103)
	reader := newStEpochAmendedUsage(8, stExtenderAmendmentTestPolicy(tlsedge.ClientId), map[server.Id]bool{})
	for _, c := range []stAmendedTestContract{
		{name: "c1", bytes: 3_000_000_002, origin: ContractParticipant{ClientId: contractPayoutTestId(201), NetworkId: o2}, egress: p, hops: []ContractParticipant{q, x}, payer: &o2},
		{name: "c2", bytes: 3_000_000_001, origin: ContractParticipant{ClientId: contractPayoutTestId(202), NetworkId: o2}, egress: tlsedge, hops: []ContractParticipant{x}, payer: &o2},
		{name: "c3", bytes: 1_000_000_001, origin: ContractParticipant{ClientId: contractPayoutTestId(203), NetworkId: o3}, egress: ContractParticipant{ClientId: contractPayoutTestId(7), NetworkId: o3}, hops: []ContractParticipant{x}, payer: &o3},
	} {
		if err := stAmendedTestAdd(t, reader, c); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	usages, pool := reader.result()
	want := map[server.Id]int64{p.ClientId: 1_000_000_001, q.ClientId: 1_000_000_001, x.ClientId: 1_000_000_000 + 1_500_000_000 + 500_000_000}
	if len(usages) != len(want) {
		t.Fatalf("usages %+v, want %v", usages, want)
	}
	var providerTotal int64
	for i, usage := range usages {
		if i > 0 && !usages[i-1].ClientId.Less(usage.ClientId) {
			t.Fatal("usages are not in client id order")
		}
		if usage.PayoutByteCount != want[usage.ClientId] {
			t.Fatalf("client %s: %d bytes, want %d", usage.ClientId, usage.PayoutByteCount, want[usage.ClientId])
		}
		providerTotal += usage.PayoutByteCount
	}
	if pool.IntraPlatform != 1_500_000_001 || pool.IntraNetwork != 500_000_001 || pool.Bytes() != 2_000_000_002 || pool.Contracts != 2 || pool.Payers != 2 {
		t.Fatalf("pool %+v, want Z = 2,000,000,002 from 2 contracts of 2 payers", pool)
	}
	if providerTotal+int64(pool.Bytes()) != 7_000_000_004 || pool.UnfundedBytes != 0 || pool.MeasurementBytes != 0 {
		t.Fatalf("provider usage %d and Z %d do not sum to the epoch's 7,000,000,004 bytes", providerTotal, pool.Bytes())
	}
}

// Before E_x the reader is the reader before the amendment: every share is
// provider usage, the platform endpoint's and the same-network client's
// included, and nothing is pooled.
func TestStEpochAmendedUsageBeforeItsFirstEpoch(t *testing.T) {
	x := ContractParticipant{ClientId: contractPayoutTestId(12), NetworkId: contractPayoutTestId(240)}
	tlsedge := ContractParticipant{ClientId: contractPayoutTestId(5), NetworkId: contractPayoutTestId(160)}
	o3 := contractPayoutTestId(103)
	receiver := ContractParticipant{ClientId: contractPayoutTestId(7), NetworkId: o3}
	for _, epoch := range []uint64{1, 7} {
		reader := newStEpochAmendedUsage(epoch, stExtenderAmendmentTestPolicy(tlsedge.ClientId), map[server.Id]bool{})
		for _, c := range []stAmendedTestContract{
			{name: "platform", bytes: 3_000_000_001, origin: ContractParticipant{ClientId: contractPayoutTestId(202), NetworkId: o3}, egress: tlsedge, hops: []ContractParticipant{x}, payer: &o3},
			{name: "same-network", bytes: 1_000_000_001, origin: ContractParticipant{ClientId: contractPayoutTestId(203), NetworkId: o3}, egress: receiver, hops: []ContractParticipant{x}},
		} {
			if err := stAmendedTestAdd(t, reader, c); err != nil {
				t.Fatalf("epoch %d, %s: %v", epoch, c.name, err)
			}
		}
		usages, pool := reader.result()
		got := map[server.Id]int64{}
		for _, usage := range usages {
			got[usage.ClientId] = usage.PayoutByteCount
		}
		if got[tlsedge.ClientId] != 1_500_000_001 || got[x.ClientId] != 1_500_000_000+500_000_000 || got[receiver.ClientId] != 500_000_001 || *pool != (StEpochExtenderPool{}) {
			t.Fatalf("epoch %d: usages %v and pool %+v, want every share as provider usage", epoch, got, pool)
		}
	}
}

// Z counts only funded bytes, free and paid alike, and never the operator's
// declared measurement or test traffic. The pooled shares of a contract no
// balance funded, and of one a measurement network paid for, leave the
// provider rows and enter no Z; the contract's other shares stay provider
// usage either way.
func TestStEpochAmendedUsageExcludesUnfundedAndMeasurement(t *testing.T) {
	x := ContractParticipant{ClientId: contractPayoutTestId(12), NetworkId: contractPayoutTestId(240)}
	network := contractPayoutTestId(103)
	probe := contractPayoutTestId(104)
	sameNetwork := func(name string, origin server.Id, payer *server.Id) stAmendedTestContract {
		return stAmendedTestContract{name: name, bytes: 1_001, origin: ContractParticipant{ClientId: contractPayoutTestId(203), NetworkId: origin},
			egress: ContractParticipant{ClientId: contractPayoutTestId(7), NetworkId: origin}, hops: []ContractParticipant{x}, payer: payer}
	}
	reader := newStEpochAmendedUsage(8, stExtenderAmendmentTestPolicy(), map[server.Id]bool{probe: true})
	for _, c := range []stAmendedTestContract{
		sameNetwork("funded", network, &network),
		sameNetwork("unfunded", network, nil),
		sameNetwork("measurement", probe, &probe),
	} {
		if err := stAmendedTestAdd(t, reader, c); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	usages, pool := reader.result()
	// each contract: the receiving client (7) takes 501 bytes, x 500
	if len(usages) != 1 || usages[0].ClientId != x.ClientId || usages[0].PayoutByteCount != 3*500 {
		t.Fatalf("usages %+v, want only the extender's three hop shares", usages)
	}
	if pool.IntraNetwork != 501 || pool.IntraPlatform != 0 || pool.Contracts != 1 || pool.Payers != 1 ||
		pool.UnfundedBytes != 501 || pool.UnfundedContracts != 1 || pool.MeasurementBytes != 501 || pool.MeasurementContracts != 1 {
		t.Fatalf("pool %+v", pool)
	}
}

// The reader refuses what it cannot classify rather than guess: an archived
// contract with shares, whose origin network the archive does not keep, and
// a contract with shares and no usage origin. A contract with no shares
// needs neither.
func TestStEpochAmendedUsageRefusesWhatItCannotClassify(t *testing.T) {
	x := ContractParticipant{ClientId: contractPayoutTestId(12), NetworkId: contractPayoutTestId(240)}
	network := contractPayoutTestId(103)
	origin := ContractParticipant{ClientId: contractPayoutTestId(203), NetworkId: network}
	egress := ContractParticipant{ClientId: contractPayoutTestId(7), NetworkId: network}
	reader := newStEpochAmendedUsage(8, stExtenderAmendmentTestPolicy(), map[server.Id]bool{})
	if err := stAmendedTestAdd(t, reader, stAmendedTestContract{name: "archived", bytes: 10, origin: origin, egress: egress, hops: []ContractParticipant{x}, payer: &network, archived: true}); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("read an archived contract's shares: %v", err)
	}
	snapshot, _ := stExtenderTestSnapshot(t, 10, origin, egress, x)
	if err := reader.add(&stEpochUsageRow{contractId: contractPayoutTestId(250), live: true, payerNetworkId: &network}, snapshot); err == nil {
		t.Fatal("classified shares without a usage origin")
	}
	empty := &contractUsageSnapshot{Version: 1, ByteCount: 0, Providers: []contractProviderUsage{}}
	for _, row := range []*stEpochUsageRow{{contractId: contractPayoutTestId(250)}, {contractId: contractPayoutTestId(251), live: true}} {
		if err := reader.add(row, empty); err != nil {
			t.Fatalf("a contract without shares: %v", err)
		}
	}
	if usages, pool := reader.result(); len(usages) != 0 || *pool != (StEpochExtenderPool{}) {
		t.Fatalf("refused contracts added %+v and %+v", usages, pool)
	}
	// A zero-byte contract with hops moves no bytes: archived or without an
	// origin, it adds its hops' zero usage as the reader before the amendment
	// does, the same-network hop's included, and pools nothing.
	zero, _ := stExtenderTestSnapshot(t, 0, origin, egress, x)
	for _, row := range []*stEpochUsageRow{{contractId: contractPayoutTestId(252)}, {contractId: contractPayoutTestId(253), live: true}} {
		if err := reader.add(row, zero); err != nil {
			t.Fatalf("a zero-byte contract: %v", err)
		}
	}
	usages, pool := reader.result()
	if len(usages) != 2 || usages[0].PayoutByteCount != 0 || usages[1].PayoutByteCount != 0 || *pool != (StEpochExtenderPool{}) {
		t.Fatalf("zero-byte contracts added %+v and %+v", usages, pool)
	}
}

// The amendment's first epoch comes from its signed entry, as the server
// passes it: InForce holds from it on and never before, and the zero value
// is no amendment.
func TestStExtenderAmendmentInForce(t *testing.T) {
	amendment := StExtenderAmendment{FirstEpoch: 8}
	for epoch, want := range map[uint64]bool{0: false, 1: false, 7: false, 8: true, 9: true, 1 << 40: true} {
		if amendment.InForce(epoch) != want {
			t.Fatalf("epoch %d: in force %v", epoch, !want)
		}
		if (StExtenderAmendment{}).InForce(epoch) {
			t.Fatalf("no amendment is in force at epoch %d", epoch)
		}
	}
}
