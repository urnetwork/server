// Completed-byte and allocation regressions exclude funding state entirely.
package model

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/urnetwork/server"
)

// Bilateral lower bounds, adjudication, and capacity use integer arithmetic.
func TestStContractUsageCompletedBytes(t *testing.T) {
	for _, c := range []struct {
		outcome                             ContractOutcome
		source, destination, capacity, want ByteCount
	}{
		{outcome: ContractOutcomeSettled, source: 121, destination: 120, capacity: 200, want: 120},
		{outcome: ContractOutcomeSettled, source: math.MaxInt64, destination: math.MaxInt64, capacity: math.MaxInt64, want: math.MaxInt64},
		{outcome: ContractOutcomeSettled, source: 121, destination: 130, capacity: 100, want: 100},
		{outcome: ContractOutcomeDisputeResolvedToSource, source: 121, destination: 12, capacity: 200, want: 121},
		{outcome: ContractOutcomeDisputeResolvedToDestination, source: 12, destination: 121, capacity: 200, want: 121},
	} {
		got, err := contractCompletedUsage(c.outcome, c.capacity, map[ContractParty]contractUsageClose{
			ContractPartySource: {ByteCount: c.source}, ContractPartyDestination: {ByteCount: c.destination},
		})
		if err != nil || got != c.want {
			t.Fatalf("%+v got %d, %v", c, got, err)
		}
	}
}

// A pending or missing party never becomes positive verified usage.
func TestStContractUsageRequiresCompletedEvidence(t *testing.T) {
	for _, closes := range []map[ContractParty]contractUsageClose{
		{}, {ContractPartySource: {ByteCount: 12}},
		{ContractPartySource: {ByteCount: 12, Checkpoint: true}, ContractPartyDestination: {ByteCount: 12}},
		{ContractPartySource: {ByteCount: 12}, ContractPartyDestination: {ByteCount: -1}},
		{ContractPartySource: {ByteCount: 12}, ContractPartyDestination: {ByteCount: 12}, "foreign": {ByteCount: 1}},
	} {
		if got, err := contractCompletedUsage(ContractOutcomeSettled, 100, closes); err == nil || got != 0 {
			t.Fatalf("uncompleted credit: %+v => %d,%v", closes, got, err)
		}
	}
}

// Neither same-network membership nor integer remainder removes real work.
func TestStContractUsageExactProviderSplit(t *testing.T) {
	networkId := contractPayoutTestId(1)
	snapshot, err := newContractUsageSnapshot(121, []ContractParticipant{
		{ClientId: contractPayoutTestId(3), NetworkId: networkId},
		{ClientId: contractPayoutTestId(2), NetworkId: networkId},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Providers[0].ClientId != contractPayoutTestId(2) || snapshot.Providers[0].ByteCount != 61 || snapshot.Providers[1].ByteCount != 60 {
		t.Fatalf("lost exact allocation: %+v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeContractUsageSnapshot(encoded); err != nil {
		t.Fatal(err)
	}
}

// A malformed ledger cannot gain a monetary or mutable-membership fallback.
func TestStContractUsageRejectsInvalidSnapshot(t *testing.T) {
	clientId, networkId := contractPayoutTestId(1), contractPayoutTestId(2)
	for _, snapshot := range []contractUsageSnapshot{
		{Version: 2, Providers: []contractProviderUsage{}},
		{Version: 1, ByteCount: 1, Providers: []contractProviderUsage{}},
		{Version: 1, Providers: nil},
		{Version: 1, ByteCount: -1, Providers: []contractProviderUsage{}},
		{Version: 1, ByteCount: 1, Providers: []contractProviderUsage{{ClientId: server.Id{}, NetworkId: networkId, ByteCount: 1}}},
		{Version: 1, ByteCount: 1, Providers: []contractProviderUsage{{ClientId: clientId, NetworkId: networkId, ByteCount: -1}}},
		{Version: 1, ByteCount: 2, Providers: []contractProviderUsage{{ClientId: clientId, NetworkId: networkId, ByteCount: 1}, {ClientId: clientId, NetworkId: networkId, ByteCount: 1}}},
		{Version: 1, ByteCount: math.MaxInt64, Providers: []contractProviderUsage{{ClientId: clientId, NetworkId: networkId, ByteCount: math.MaxInt64}, {ClientId: contractPayoutTestId(3), NetworkId: networkId, ByteCount: 1}}},
	} {
		data, _ := json.Marshal(snapshot)
		if _, err := decodeContractUsageSnapshot(data); err == nil {
			t.Fatalf("invalid accepted: %s", data)
		}
	}
	for _, data := range [][]byte{nil, []byte("null"), []byte("{}"), []byte("[]"), []byte(`{"version":1,"providers":[]}`), []byte(`{"version":1,"byte_count":0,"providers":[],"paid":true}`)} {
		if _, err := decodeContractUsageSnapshot(data); err == nil {
			t.Fatalf("missing/malformed accepted: %s", data)
		}
	}
}
