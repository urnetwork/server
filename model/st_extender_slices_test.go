// The SN25 extender amendment's sliced epoch read, pure: a slice's reader
// merged into the epoch's adds exactly what reading its rows there would.
package model

import (
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// §8.6.5's contracts with an unfunded and a measurement one, read by one
// reader or split across two readers and merged, give the same usages and
// pool, whichever way they are split. A client with two networks across the
// two readers is refused, as one reader refuses it.
func TestStEpochAmendedUsageMergeIsTheSameRead(t *testing.T) {
	p := ContractParticipant{ClientId: contractPayoutTestId(10), NetworkId: contractPayoutTestId(110)}
	q := ContractParticipant{ClientId: contractPayoutTestId(11), NetworkId: contractPayoutTestId(111)}
	x := ContractParticipant{ClientId: contractPayoutTestId(12), NetworkId: contractPayoutTestId(240)}
	tlsedge := ContractParticipant{ClientId: contractPayoutTestId(5), NetworkId: contractPayoutTestId(160)}
	o2, o3, probe := contractPayoutTestId(102), contractPayoutTestId(103), contractPayoutTestId(104)
	contracts := []stAmendedTestContract{
		{name: "c1", bytes: 3_000_000_002, origin: ContractParticipant{ClientId: contractPayoutTestId(201), NetworkId: o2}, egress: p, hops: []ContractParticipant{q, x}, payer: &o2},
		{name: "c2", bytes: 3_000_000_001, origin: ContractParticipant{ClientId: contractPayoutTestId(202), NetworkId: o2}, egress: tlsedge, hops: []ContractParticipant{x}, payer: &o2},
		{name: "c3", bytes: 1_000_000_001, origin: ContractParticipant{ClientId: contractPayoutTestId(203), NetworkId: o3}, egress: ContractParticipant{ClientId: contractPayoutTestId(7), NetworkId: o3}, hops: []ContractParticipant{x}, payer: &o3},
		{name: "unfunded", bytes: 1_001, origin: ContractParticipant{ClientId: contractPayoutTestId(203), NetworkId: o3}, egress: ContractParticipant{ClientId: contractPayoutTestId(7), NetworkId: o3}, hops: []ContractParticipant{x}},
		{name: "measurement", bytes: 1_001, origin: ContractParticipant{ClientId: contractPayoutTestId(204), NetworkId: probe}, egress: ContractParticipant{ClientId: contractPayoutTestId(8), NetworkId: probe}, hops: []ContractParticipant{x}, payer: &probe},
		{name: "c1 again", bytes: 9, origin: ContractParticipant{ClientId: contractPayoutTestId(201), NetworkId: o2}, egress: p, hops: []ContractParticipant{q}, payer: &o2},
	}
	newReader := func() *stEpochAmendedUsage {
		return newStEpochAmendedUsage(8, stExtenderAmendmentTestPolicy(tlsedge.ClientId), map[server.Id]bool{probe: true})
	}
	one := newReader()
	for _, c := range contracts {
		if err := stAmendedTestAdd(t, one, c); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	wantUsages, wantPool := one.result()
	for split := 0; split <= len(contracts); split++ {
		epochReader, sliceReader := newReader(), newReader()
		for i, c := range contracts {
			reader := epochReader
			if split <= i {
				reader = sliceReader
			}
			if err := stAmendedTestAdd(t, reader, c); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		}
		if err := epochReader.merge(sliceReader); err != nil {
			t.Fatalf("split at %d: %v", split, err)
		}
		usages, pool := epochReader.result()
		if !slices.EqualFunc(usages, wantUsages, func(a, b *StProviderUsage) bool { return *a == *b }) || stExtenderPoolTotals(pool) != stExtenderPoolTotals(wantPool) || len(pool.ClientIdPooledBytes) != len(wantPool.ClientIdPooledBytes) {
			t.Fatalf("split at %d: %+v %+v, want %+v %+v", split, usages, pool, wantUsages, wantPool)
		}
		for clientId, bytes := range wantPool.ClientIdPooledBytes {
			if pool.ClientIdPooledBytes[clientId] != bytes {
				t.Fatalf("split at %d: client %s pooled %d, want %d", split, clientId, pool.ClientIdPooledBytes[clientId], bytes)
			}
		}
	}
	// p on another network in the second reader
	epochReader, sliceReader := newReader(), newReader()
	if err := stAmendedTestAdd(t, epochReader, contracts[0]); err != nil {
		t.Fatal(err)
	}
	moved := contracts[5]
	moved.egress = ContractParticipant{ClientId: p.ClientId, NetworkId: contractPayoutTestId(119)}
	if err := stAmendedTestAdd(t, sliceReader, moved); err != nil {
		t.Fatal(err)
	}
	if err := epochReader.merge(sliceReader); err == nil || !strings.Contains(err.Error(), "ambiguous network") {
		t.Fatalf("a client on two networks: %v", err)
	}
}
