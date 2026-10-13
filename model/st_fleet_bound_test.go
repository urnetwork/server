// The FleetBound mirror bounds settlement v2 head exclusion
// (VERSION2-FINAL.md §6.2, §8.2). Identities are synthetic.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A mirrored FleetBound row for clientId with the given validity.
func stFleetBoundTestEvent(block uint64, index int, clientId [16]byte, from string, through string) *StChainEvent {
	data, err := json.Marshal(StFleetBoundEventData{ClientId: fmt.Sprintf("0x%x", clientId), FleetId: "0x01", Hotkey: "0x02", Uid: "3", Generation: "4", ValidFromEpoch: from, ValidToEpoch: through})
	if err != nil {
		panic(err)
	}
	return &StChainEvent{BlockNumber: block, BlockHash: fmt.Sprintf("0x%064x", block), LogIndex: index, TxHash: fmt.Sprintf("0x%064x", block*100+uint64(index)), Kind: StFleetBoundEventKind, DataJson: string(data)}
}

// GL-7, SV-6: the checkpoint is per deployment and starts at zero.
func TestStFleetBoundNextBlock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		keyA, keyB := StDeploymentKey("945:0xa"), StDeploymentKey("945:0xb")
		if GetStFleetBoundNextBlock(ctx, keyA) != 0 {
			t.Fatal("a fresh deployment has a FleetBound checkpoint")
		}
		SetStFleetBoundNextBlock(ctx, keyA, 1_000)
		SetStFleetBoundNextBlock(ctx, keyA, 2_000)
		SetStFleetBoundNextBlock(ctx, keyB, 3_000)
		if GetStFleetBoundNextBlock(ctx, keyA) != 2_000 || GetStFleetBoundNextBlock(ctx, keyB) != 3_000 {
			t.Fatal("the FleetBound checkpoint did not advance per deployment")
		}
	})
}

// GL-7, SV-6: the candidates are the clients of FleetBound rows at or before
// the close block whose validity covers the epoch, ascending and unique; other
// kinds, later rows and other validities are not candidates.
func TestStFleetBoundClientIds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := StDeploymentKey("945:0xc")
		covering, renewed, edgeFrom, edgeTo, before, after, late := [16]byte{9}, [16]byte{3}, [16]byte{5}, [16]byte{7}, [16]byte{1}, [16]byte{2}, [16]byte{4}
		other := stFleetBoundTestEvent(100, 9, [16]byte{8}, "1", "100")
		other.Kind = "Deposited"
		UpsertStEvents(ctx, key, []*StChainEvent{
			stFleetBoundTestEvent(100, 0, covering, "40", "60"),
			stFleetBoundTestEvent(101, 0, renewed, "30", "45"),
			stFleetBoundTestEvent(101, 1, renewed, "46", "70"),
			stFleetBoundTestEvent(102, 0, edgeFrom, "50", "80"),
			stFleetBoundTestEvent(103, 0, edgeTo, "20", "50"),
			stFleetBoundTestEvent(104, 0, before, "10", "49"),
			stFleetBoundTestEvent(105, 0, after, "51", "90"),
			stFleetBoundTestEvent(201, 0, late, "40", "60"),
			other,
		})
		clientIds, err := GetStFleetBoundClientIds(ctx, key, 50, 200)
		if err != nil {
			t.Fatal(err)
		}
		if want := [][16]byte{renewed, edgeFrom, edgeTo, covering}; !slices.Equal(clientIds, want) {
			t.Fatalf("candidates %x, want %x", clientIds, want)
		}
		if clientIds, err := GetStFleetBoundClientIds(ctx, key, 50, 201); err != nil || len(clientIds) != 5 {
			t.Fatal("a FleetBound at the close block was not a candidate", clientIds, err)
		}
		if clientIds, err := GetStFleetBoundClientIds(ctx, StDeploymentKey("945:0xd"), 50, 200); err != nil || clientIds == nil || len(clientIds) != 0 {
			t.Fatal("another deployment's FleetBound was a candidate", clientIds, err)
		}
	})
}

// GL-7, SV-6: a malformed row fails the read, naming its log, rather than
// being skipped.
func TestStFleetBoundClientIdsRefuseMalformedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for index, malformed := range []struct {
			label    string
			dataJson string
			want     string
		}{
			{"not an object", `["array"]`, "cannot unmarshal"},
			{"short client", `{"client_id":"0x0102","valid_from_epoch":"1","valid_to_epoch":"2"}`, "is not 16 bytes"},
			{"not hex", `{"client_id":"0xzz","valid_from_epoch":"1","valid_to_epoch":"2"}`, "is not 16 bytes"},
			{"no from", fmt.Sprintf(`{"client_id":"0x%032x","valid_to_epoch":"2"}`, 1), "is not two epochs"},
			{"signed through", fmt.Sprintf(`{"client_id":"0x%032x","valid_from_epoch":"1","valid_to_epoch":"-2"}`, 1), "is not two epochs"},
		} {
			key := StDeploymentKey(fmt.Sprintf("945:0xe%d", index))
			event := stFleetBoundTestEvent(77, 3, [16]byte{1}, "1", "2")
			event.DataJson = malformed.dataJson
			UpsertStEvents(ctx, key, []*StChainEvent{event})
			clientIds, err := GetStFleetBoundClientIds(ctx, key, 1, 100)
			if clientIds != nil || err == nil || !strings.Contains(err.Error(), "FleetBound event 77:3") || !strings.Contains(err.Error(), malformed.want) {
				t.Fatalf("%s: candidates %x, error %v", malformed.label, clientIds, err)
			}
		}
	})
}
