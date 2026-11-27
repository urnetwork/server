package model

// The epoch 8 late-binding window (sn protocol/fleet_late_binding_epoch8.go)
// on the mirrored bindings. Identities are synthetic; the deployment is the
// SN25 mainnet coordinator's, whose epoch 8 has the window.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

const testLateBindingDeployment = StDeploymentKey("964:0xc18925925e2b7bb9059b7d696b8c92762ae86406")

func TestStDeploymentIdentity(t *testing.T) {
	chainId, coordinator, ok := stDeploymentIdentity(testLateBindingDeployment)
	if !ok || chainId != 964 || fmt.Sprintf("0x%x", coordinator) != "0xc18925925e2b7bb9059b7d696b8c92762ae86406" {
		t.Fatalf("mainnet: %d 0x%x %t", chainId, coordinator, ok)
	}
	for _, key := range []StDeploymentKey{"945:0xa", "964", "x:0xc18925925e2b7bb9059b7d696b8c92762ae86406", "964:c18925925e2b7bb9059b7d696b8c92762ae86406"} {
		if _, _, ok := stDeploymentIdentity(key); ok {
			t.Fatalf("%q parsed", key)
		}
	}
}

// For epoch 8, a client with no version active there counts its newest window
// version, from epoch 8; one in force for epoch 8 from before is never
// displaced; a revocation keeps a window version through epoch 8, a cleanup
// in epoch 8 ends it, and every other epoch reads the signed validities.
func TestSnFleetBindingsAtEpoch8Window(t *testing.T) {
	window := func(client byte, generation uint64, block uint64) *SnFleetBindingVersion {
		version := testSnFleetBindingVersion(client, generation, 9, 20, block)
		version.Epoch8Window = true
		return version
	}
	late := window(1, 1, 9_600_029)
	// client 2: in force for epoch 8 from before, then a window binding
	before := testSnFleetBindingVersion(2, 1, 8, 8, 9_590_000)
	displaced := window(2, 2, 9_600_100)
	// client 3: a window binding revoked to end with epoch 8
	revoked := window(3, 1, 9_600_200)
	revoked.ValidToEpoch, revoked.Revoked = 8, true
	// client 4: a window binding cleaned in epoch 8
	cleaned := window(4, 1, 9_600_300)
	cleaned.Cleaned, cleaned.CleanedAtEpoch = true, 8
	// client 5: signed from epoch 9 outside the window
	outside := testSnFleetBindingVersion(5, 1, 9, 20, 9_615_061)
	versions := []*SnFleetBindingVersion{late, before, displaced, revoked, cleaned, outside}
	at := func(epoch uint64) map[byte]uint64 {
		out := map[byte]uint64{}
		for _, version := range SnFleetBindingsAt(versions, epoch) {
			out[version.ClientId[0]] = version.Generation
		}
		return out
	}
	if got := at(8); len(got) != 3 || got[1] != 1 || got[2] != 1 || got[3] != 1 {
		t.Fatalf("epoch 8: %v", got)
	}
	// the revoked window version ended with epoch 8, the cleaned one in it
	if got := at(9); len(got) != 3 || got[1] != 1 || got[2] != 2 || got[5] != 1 {
		t.Fatalf("epoch 9: %v", got)
	}
	if got := at(7); len(got) != 0 {
		t.Fatalf("epoch 7: %v", got)
	}
	if late.FirstEpoch() != 8 || outside.FirstEpoch() != 9 || before.FirstEpoch() != 8 {
		t.Fatal("first epochs")
	}
}

// The FleetBound mirror's candidates for epoch 8 include the clients whose
// binding signed from epoch 9 landed in the window, and only on the mainnet
// coordinator.
func TestStFleetBoundClientIdsEpoch8Window(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		late, last, before, after, normal := [16]byte{0x01}, [16]byte{0x02}, [16]byte{0x03}, [16]byte{0x04}, [16]byte{0x05}
		events := []*StChainEvent{
			stFleetBoundTestEvent(9_560_000, 0, normal, "8", "19"),
			stFleetBoundTestEvent(9_600_027, 0, before, "9", "20"),
			stFleetBoundTestEvent(9_600_029, 0, late, "9", "20"),
			stFleetBoundTestEvent(9_615_060, 0, last, "9", "20"),
			stFleetBoundTestEvent(9_615_061, 0, after, "9", "20"),
		}
		other := StDeploymentKey("945:0xc18925925e2b7bb9059b7d696b8c92762ae86406")
		UpsertStEvents(ctx, testLateBindingDeployment, events)
		UpsertStEvents(ctx, other, events)
		got, err := GetStFleetBoundClientIds(ctx, testLateBindingDeployment, 8, 9_650_428)
		if err != nil || !slices.Equal(got, [][16]byte{late, last, normal}) {
			t.Fatalf("mainnet epoch 8 candidates: %x %v", got, err)
		}
		if got, err := GetStFleetBoundClientIds(ctx, testLateBindingDeployment, 9, 9_700_828); err != nil || len(got) != 5 {
			t.Fatalf("mainnet epoch 9 candidates: %x %v", got, err)
		}
		if got, err := GetStFleetBoundClientIds(ctx, other, 8, 9_650_428); err != nil || !slices.Equal(got, [][16]byte{normal}) {
			t.Fatalf("another chain's epoch 8 candidates: %x %v", got, err)
		}
	})
}

// The binding census marks the versions in the window as it loads them, so
// the claimed fleets of epoch 8, and the verify cohort's priority, count
// them; a version outside the window waits for epoch 9.
func TestSnFleetBindingVersionsEpoch8Window(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bound := func(block uint64, client byte, from string, to string) *StChainEvent {
			data, err := json.Marshal(StFleetBoundEventData{ClientId: fmt.Sprintf("0x%x", [16]byte{client}), FleetId: fmt.Sprintf("0x%x", [32]byte{0xf1}), Hotkey: fmt.Sprintf("0x%x", [32]byte{0xf2}), Uid: "9", Generation: "1", ValidFromEpoch: from, ValidToEpoch: to})
			if err != nil {
				t.Fatal(err)
			}
			return &StChainEvent{BlockNumber: block, BlockHash: fmt.Sprintf("0x%064x", block), LogIndex: 0, TxHash: fmt.Sprintf("0x%064x", block*100), Kind: StFleetBoundEventKind, DataJson: string(data)}
		}
		events := []*StChainEvent{bound(9_600_027, 0x03, "9", "20"), bound(9_600_029, 0x01, "9", "20"), bound(9_615_061, 0x04, "9", "20")}
		if err := ApplySnFleetBindingRange(ctx, testLateBindingDeployment, events, SnFleetBindingCheckpoint{NextBlock: 9_615_062, BlockHash: fmt.Sprintf("0x%064x", 9_615_061)}); err != nil {
			t.Fatal(err)
		}
		versions := GetSnFleetBindingVersions(ctx, testLateBindingDeployment)
		window := map[byte]bool{}
		for _, version := range versions {
			window[version.ClientId[0]] = version.Epoch8Window
		}
		if len(versions) != 3 || !window[0x01] || window[0x03] || window[0x04] {
			t.Fatalf("window versions: %v", window)
		}
		at8 := SnFleetBindingsAt(versions, 8)
		if len(at8) != 1 || at8[0].ClientId[0] != 0x01 {
			t.Fatalf("epoch 8: %v", at8)
		}
	})
}
