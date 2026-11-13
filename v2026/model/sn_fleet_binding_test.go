package model

// The binding census replays the coordinator's bindingAt (sn
// evm/src/STCoordinator.sol): the newest version already valid at an epoch
// decides, so a later version's start hides an earlier one; a revocation ends
// a version before its effective epoch and a cleanup at its cleanup epoch. No
// database; identities are synthetic.

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func testSnFleetBindingVersion(client byte, generation uint64, from uint64, to uint64, block uint64) *SnFleetBindingVersion {
	return &SnFleetBindingVersion{ClientId: server.Id{client}, Generation: generation, FleetId: [32]byte{client, 0xf1}, Hotkey: [32]byte{client, 0xf2},
		Uid: uint16(client), ValidFromEpoch: from, SignedValidToEpoch: to, ValidToEpoch: to, BoundBlock: block}
}

func TestSnFleetBindingsAtReplaysBindingAt(t *testing.T) {
	// client 1: generation 1 for 40..60 revoked from 45, generation 2 for 45..46
	revoked := testSnFleetBindingVersion(1, 1, 40, 60, 10)
	revoked.ValidToEpoch, revoked.Revoked = 44, true
	short := testSnFleetBindingVersion(1, 2, 45, 46, 20)
	// client 2: cleaned at 43
	cleaned := testSnFleetBindingVersion(2, 7, 40, 60, 11)
	cleaned.Cleaned, cleaned.CleanedAtEpoch = true, 43
	// client 3: bound from 50
	future := testSnFleetBindingVersion(3, 1, 50, 61, 12)
	versions := []*SnFleetBindingVersion{short, future, cleaned, revoked}
	clients := func(epoch uint64) []byte {
		out := []byte{}
		for _, version := range SnFleetBindingsAt(versions, epoch) {
			out = append(out, version.ClientId[0])
		}
		return out
	}
	for epoch, want := range map[uint64][]byte{
		39: {},
		40: {1, 2},
		42: {1, 2},
		43: {1},
		44: {1},
		45: {1},
		46: {1},
		// generation 2 is the newest valid version and has ended; generation
		// 1's original validity does not come back
		50: {3},
		61: {3},
		62: {},
	} {
		if got := clients(epoch); !slices.Equal(got, want) {
			t.Fatalf("epoch %d: %v, want %v", epoch, got, want)
		}
	}
	if at := SnFleetBindingsAt(versions, 45); len(at) != 1 || at[0].Generation != 2 {
		t.Fatal("the newest valid version decides", at)
	}
}

func TestParseSnFleetBindingEventRefusesMalformedRows(t *testing.T) {
	bound := func(data StFleetBoundEventData) *StChainEvent {
		encoded, _ := json.Marshal(data)
		return &StChainEvent{Kind: StFleetBoundEventKind, DataJson: string(encoded)}
	}
	valid := StFleetBoundEventData{ClientId: "0x07010000000000000000000000000000", FleetId: "0x" + "21" + "f1" + "00000000000000000000000000000000000000000000000000000000000000"[:60],
		Hotkey: "0x" + "31f2" + "00000000000000000000000000000000000000000000000000000000000000"[:60], Uid: "27", Generation: "9", ValidFromEpoch: "41", ValidToEpoch: "52"}
	parsed, err := parseSnFleetBindingEvent(bound(valid))
	if err != nil || parsed.uid != 27 || parsed.generation != 9 || parsed.validFrom != 41 || parsed.validTo != 52 || parsed.clientId != (server.Id{7, 1}) {
		t.Fatal("valid FleetBound", parsed, err)
	}
	for name, change := range map[string]func(*StFleetBoundEventData){
		"short client":     func(data *StFleetBoundEventData) { data.ClientId = "0x0701" },
		"bare client":      func(data *StFleetBoundEventData) { data.ClientId = "07010000000000000000000000000000" },
		"wide uid":         func(data *StFleetBoundEventData) { data.Uid = "65536" },
		"leading zero":     func(data *StFleetBoundEventData) { data.Generation = "09" },
		"signed epoch":     func(data *StFleetBoundEventData) { data.ValidFromEpoch = "-1" },
		"non-hex hotkey":   func(data *StFleetBoundEventData) { data.Hotkey = "0x" + "zz" + valid.Hotkey[4:] },
		"short fleet":      func(data *StFleetBoundEventData) { data.FleetId = "0x21" },
		"decimal overflow": func(data *StFleetBoundEventData) { data.ValidToEpoch = "18446744073709551616" },
	} {
		data := valid
		change(&data)
		if _, err := parseSnFleetBindingEvent(bound(data)); err == nil {
			t.Fatalf("%s was parsed", name)
		}
	}
	revoked, _ := json.Marshal(SnFleetBindingRevokedEventData{ClientId: valid.ClientId, Generation: "9", EffectiveEpoch: "0"})
	if _, err := parseSnFleetBindingEvent(&StChainEvent{Kind: SnFleetBindingRevokedEventKind, DataJson: string(revoked)}); err == nil {
		t.Fatal("a revocation effective at epoch 0 was parsed")
	}
	if _, err := parseSnFleetBindingEvent(&StChainEvent{Kind: "Deposited", DataJson: "{}"}); err == nil {
		t.Fatal("another kind was parsed as a binding event")
	}
}
