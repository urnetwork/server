package controller

// st_ledger_v2_extender_relay_test.go — the SN25 extender pool's relay half
// (sn WHITEPAPER.md §8.6.2, owner 2026-11-16) as pure arithmetic: the relay
// rows a component takes from the epoch's relay bytes and census, and the
// shadow of §8.6.5's worked example split half by unit and half by relay
// bytes.

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A component's relay rows are the census clients' relay bytes, once per
// client however many census rows it has, in the census's client order: a
// relaying client the census lacks has no row, nor has a census client that
// relayed nothing. Whether a row counts is the split's to decide, so an
// ineligible census client keeps its row. No relay read is no rows.
func TestStLedgerV2RelayRowsAreTheCensusClientsRelayBytes(t *testing.T) {
	_, _, _, census := stExtenderExample()
	// x21 holds two census rows; x24 has no wallet, so it is not eligible
	second := census[0]
	second.ExtenderID = [16]byte{9}
	second.Flags |= ledger.CensusDuplicate
	census = slices.Insert(census, 1, second)
	if err := ledger.CheckCensusRows(census); err != nil {
		t.Fatal(err)
	}
	relay := &model.StEpochExtenderRelay{ClientIdRelayBytes: map[server.Id]model.ByteCount{
		server.Id(stExtenderClient(0x21)): 30,
		server.Id(stExtenderClient(0x23)): 10,
		server.Id(stExtenderClient(0x24)): 5,
		server.Id(stExtenderClient(0x99)): 1_000,
	}}
	want := []ledger.RelayRow{
		{ClientID: stExtenderClient(0x21), RelayBytes: 30},
		{ClientID: stExtenderClient(0x23), RelayBytes: 10},
		{ClientID: stExtenderClient(0x24), RelayBytes: 5},
	}
	rows := stLedgerV2RelayRows(relay, census)
	if !slices.Equal(rows, want) {
		t.Fatalf("relay rows %+v, want %+v", rows, want)
	}
	if err := ledger.CheckRelayRows(rows, census); err != nil {
		t.Fatalf("the relay rows break the relay rules: %v", err)
	}
	if rows := stLedgerV2RelayRows(nil, census); rows == nil || len(rows) != 0 {
		t.Fatalf("no relay read: %+v", rows)
	}
}

// The worked example's shadow with relay rows, x21 relaying 3 bytes and x23 1:
// of Z' = 1,000,000,000,001, the relay half floor(Z'/2) = 500e9 goes 375e9 to
// x21 and 125e9 to x23, and the unit half 500,000,000,001 by unit as before,
// u1 taking the odd byte and x21 its first: x21 500,000,000,001, x22 125e9 and
// x23 375e9, where by unit alone they took 250,000,000,001, 250e9 and 500e9.
// The header lists the relay rows, and the validator's recompute approves the
// shadow root.
func TestStExtenderShadowSplitsTheWorkedExampleHalfByRelayBytes(t *testing.T) {
	ctx := context.Background()
	input := stExtenderExampleInput(t)
	input.Relay = []ledger.RelayRow{{ClientID: stExtenderClient(0x21), RelayBytes: 3}, {ClientID: stExtenderClient(0x23), RelayBytes: 1}}
	report, shadow, err := stExtenderShadowBuild(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	component := shadow.Epochs[0].Extenders
	if split := component.ExtenderSplit; split.Weight != 1_000_000_000_001 || split.RelayWeight != 500_000_000_000 || split.UnitWeight != 500_000_000_001 ||
		split.RelayBytes != 4 || split.Relaying != 2 || split.Eligible != 3 || split.Units != 2 {
		t.Fatalf("split %+v", split)
	}
	share := func(client byte, coldkey byte, bytes uint64) protocol.LedgerUsageRow {
		return protocol.LedgerUsageRow{ClientID: stExtenderClient(client), Coldkey: stExtenderColdkey(coldkey), UsageBytes: bytes}
	}
	want := []protocol.LedgerUsageRow{share(0x21, 'd', 500_000_000_001), share(0x22, 'b', 125_000_000_000), share(0x23, 'e', 375_000_000_000)}
	if !reflect.DeepEqual(component.Rows, want) {
		t.Fatalf("share rows %+v, want %+v", component.Rows, want)
	}
	if len(component.Relay) != 1 {
		t.Fatalf("relay chunks %v", component.Relay)
	}
	relay, err := ledger.DecodeRelayChunks([][]byte{shadow.Objects[component.Relay[0]]})
	if err != nil || !slices.Equal(relay, input.Relay) {
		t.Fatalf("the published relay rows %+v %v", relay, err)
	}
	in, err := stExtenderShadowVerifyInput(report, shadow.Objects)
	if err != nil {
		t.Fatal(err)
	}
	in.MaxFetchBytes = ledger.DefaultMaxFetchBytes
	if result, err := ledger.Verify(ctx, *in); err != nil || result.Check != ledger.CheckNone {
		t.Fatalf("the recompute refused the shadow: %+v, %v", result, err)
	}

	// without relay rows, the split of the worked example by unit alone
	byUnit, err := ledger.SplitExtenderPool(1_000_000_000_001, input.Rows, input.Census, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []protocol.LedgerUsageRow{share(0x21, 'd', 250_000_000_001), share(0x22, 'b', 250_000_000_000), share(0x23, 'e', 500_000_000_000)}; !reflect.DeepEqual(byUnit.Rows, want) || byUnit.RelayWeight != 0 {
		t.Fatalf("by unit %+v, want %+v", byUnit, want)
	}
}
