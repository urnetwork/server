// Expiry uses the original report lower bounds without confusing a lifecycle
// checkpoint, a missing peer, or a synthetic close with new delivered bytes.
package model

import (
	"encoding/json"
	"testing"
)

// Checkpoint orientation never changes the independently reported minimum.
func TestStContractUsageExpiryOriginalLowerBounds(t *testing.T) {
	for _, c := range []struct {
		source, destination contractUsageClose
		capacity, want      ByteCount
	}{
		{source: contractUsageClose{ByteCount: 121}, destination: contractUsageClose{ByteCount: 120, Checkpoint: true}, capacity: 200, want: 120},
		{source: contractUsageClose{ByteCount: 120, Checkpoint: true}, destination: contractUsageClose{ByteCount: 121}, capacity: 200, want: 120},
		{source: contractUsageClose{ByteCount: 120, Checkpoint: true}, destination: contractUsageClose{ByteCount: 121, Checkpoint: true}, capacity: 100, want: 100},
		{source: contractUsageClose{ByteCount: 0}, destination: contractUsageClose{ByteCount: 121, Checkpoint: true}, capacity: 200, want: 0},
	} {
		proof := &contractUsageExpiry{Capacity: c.capacity, Reports: map[ContractParty]contractUsageClose{ContractPartySource: c.source, ContractPartyDestination: c.destination}}
		got, err := contractExpiryCompletedUsage(proof)
		if err != nil || got != c.want {
			t.Fatalf("original reports %+v: got %d, %v", proof, got, err)
		}
	}
	for _, party := range []ContractParty{ContractPartySource, ContractPartyDestination} {
		got, err := contractExpiryCompletedUsage(&contractUsageExpiry{Capacity: 200, Reports: map[ContractParty]contractUsageClose{party: {ByteCount: 121, Checkpoint: true}}})
		if err != nil || got != 0 {
			t.Fatalf("one original party gained credit: %s, %d, %v", party, got, err)
		}
	}
}

// Snapshot decoding checks the retained original proof as well as allocation
// conservation. A fabricated peer cannot be supplied by omitted fields.
func TestStContractUsageExpiryProofIntegrity(t *testing.T) {
	snapshot, err := newContractUsageSnapshot(120, []ContractParticipant{{ClientId: contractPayoutTestId(1), NetworkId: contractPayoutTestId(2)}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Expiry = &contractUsageExpiry{Capacity: 200, Reports: map[ContractParty]contractUsageClose{ContractPartySource: {ByteCount: 121}, ContractPartyDestination: {ByteCount: 120, Checkpoint: true}}}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainedContractExpiryUsage(data); err != nil {
		t.Fatal(err)
	}
	for _, proof := range []*contractUsageExpiry{
		{Capacity: -1, Reports: snapshot.Expiry.Reports},
		{Capacity: 200, Reports: nil},
		{Capacity: 200, Reports: map[ContractParty]contractUsageClose{ContractPartySource: {ByteCount: 120}}},
		{Capacity: 200, Reports: map[ContractParty]contractUsageClose{ContractPartySource: {ByteCount: 120}, ContractPartyDestination: {ByteCount: 119}}},
		{Capacity: 200, Reports: map[ContractParty]contractUsageClose{"foreign": {ByteCount: 120}}},
	} {
		snapshot.Expiry = proof
		data, _ := json.Marshal(snapshot)
		if _, err := decodeContractUsageSnapshot(data); err == nil {
			t.Fatalf("invalid original proof accepted: %s", data)
		}
	}
	for _, data := range []string{
		`{"version":1,"byte_count":0,"providers":[],"expiry":{}}`,
		`{"version":1,"byte_count":0,"providers":[],"expiry":{"capacity":0,"reports":{"source":{"checkpoint":true}}}}`,
		`{"version":1,"byte_count":0,"providers":[],"expiry":{"capacity":0,"reports":{"source":{"byte_count":0}}}}`,
	} {
		if _, err := decodeContractUsageSnapshot([]byte(data)); err == nil {
			t.Fatalf("incomplete proof accepted: %s", data)
		}
	}
	snapshot.Expiry = nil
	data, _ = json.Marshal(snapshot)
	if _, err := retainedContractExpiryUsage(data); err == nil {
		t.Fatal("positive ordinary snapshot accepted as an original expiry proof")
	}
	legacy, err := retainedContractExpiryUsage(nil)
	if err != nil || legacy.ByteCount != 0 || legacy.ExcludedReason != "expired_unconfirmed" {
		t.Fatalf("legacy interrupted expiry was reconstructed: %+v, %v", legacy, err)
	}
}
