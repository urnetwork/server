package model

// st_ledger_v2_model_test.go — pg-backed tests of the settlement v2 records
// and mirror (VERSION2-FINAL.md §6.4, §8.4).

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
)

func testLedgerV2Ref(marker byte) string {
	return "sha256:" + strings.Repeat(string("0123456789abcdef"[marker%16]), 64)
}

// SV-1 (fixed inputs and the explicit null declaration): an epoch's provider
// rows are recorded once, a null declaration names who made it, and neither
// replaces the other.
func TestStLedgerV2EpochRowsAreRecordedOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key, other := testStDeploymentKey, StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		chunks := []string{testLedgerV2Ref(1), testLedgerV2Ref(2)}

		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 5, chunks); err != nil {
			t.Fatal(err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 5, append([]string(nil), chunks...)); err != nil {
			t.Fatalf("repeating the same record: %v", err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 5, chunks[:1]); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("changed rows: %v", err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 6, nil); err != nil {
			t.Fatal(err)
		}
		if err := DeclareStLedgerV2EpochUnrecoverable(ctx, key, 1, 7, "operator@example"); err != nil {
			t.Fatal(err)
		}
		if err := DeclareStLedgerV2EpochUnrecoverable(ctx, key, 1, 7, "operator@example"); err != nil {
			t.Fatalf("repeating a declaration: %v", err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 7, chunks); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("rows over a declaration: %v", err)
		}
		if err := DeclareStLedgerV2EpochUnrecoverable(ctx, key, 1, 5, "operator@example"); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("declaration over recorded rows: %v", err)
		}
		if err := DeclareStLedgerV2EpochUnrecoverable(ctx, key, 1, 8, ""); err == nil || err.Error() != "st ledger v2: an unrecoverable declaration must name who makes it" {
			t.Fatalf("anonymous declaration: %v", err)
		}

		records := GetStLedgerV2EpochRows(ctx, key, 1, 4, 8)
		if len(records) != 3 {
			t.Fatalf("records %+v", records)
		}
		if r := records[5]; r.Null() || !reflect.DeepEqual(r.ProviderChunks, chunks) || r.DeclaredBy != "" || r.CreateTime.IsZero() {
			t.Fatalf("epoch 5 %+v", r)
		}
		if r := records[6]; r.Null() || r.ProviderChunks == nil || len(r.ProviderChunks) != 0 {
			t.Fatalf("epoch 6 (providers: []) %+v", r)
		}
		if r := records[7]; !r.Null() || r.ProviderChunks != nil || r.DeclaredBy != "operator@example" {
			t.Fatalf("epoch 7 (providers: null) %+v", r)
		}
		if records := GetStLedgerV2EpochRows(ctx, other, 1, 4, 8); len(records) != 0 {
			t.Fatalf("another deployment sees %+v", records)
		}
		if records := GetStLedgerV2EpochRows(ctx, key, 2, 4, 8); len(records) != 0 {
			t.Fatalf("another pool sees %+v", records)
		}
	})
}

func testLedgerV2Root(seq uint64, epoch uint64, total uint64, leafCount uint64) *StLedgerV2Root {
	root := &StLedgerV2Root{NoId: 1, Seq: seq, Epoch: epoch, LeafCount: leafCount, Root: testStCkey(byte(seq)), LedgerHash: testStCkey(byte(100 + seq))}
	root.Total.SetUint64(total)
	return root
}

func testLedgerV2Amount(epoch uint64, coldkey byte, amount uint64) *StLedgerV2Amount {
	return &StLedgerV2Amount{Epoch: epoch, Coldkey: testStCkey(coldkey), Amount: *uint256.NewInt(amount)}
}

// The mirror records each root's outcome only from pending, keeps funding
// snapshots immutable, and tracks demands until answered.
// SV-5 (mirror)
func TestStLedgerV2MirrorRecordsChainState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey

		funding := &StLedgerV2Funding{NoId: 1, Epoch: 5, Observed: *uint256.NewInt(11), Amount: *uint256.NewInt(10), FundedThrough: *uint256.NewInt(10), BlockNumber: 100}
		SetStLedgerV2Funding(ctx, key, funding)
		SetStLedgerV2Funding(ctx, key, &StLedgerV2Funding{NoId: 1, Epoch: 5, FundedThrough: *uint256.NewInt(99), Deferred: true, BlockNumber: 200})
		if got := GetStLedgerV2Funding(ctx, key, 1, 5); got == nil || !reflect.DeepEqual(got, funding) {
			t.Fatalf("funding %+v, want the first snapshot %+v", got, funding)
		}
		if GetStLedgerV2Funding(ctx, key, 1, 6) != nil || GetStLedgerV2Funding(ctx, key, 2, 5) != nil {
			t.Fatal("unmirrored funding returned")
		}

		AddStLedgerV2Commit(ctx, key, testLedgerV2Root(1, 5, 10, 2), 110, 0)
		AddStLedgerV2Commit(ctx, key, testLedgerV2Root(1, 5, 10, 2), 999, 0)
		pending := GetStLedgerV2PendingRoot(ctx, key, 1)
		if pending == nil || pending.Seq != 1 || pending.Status != StLedgerV2RootPending || *pending.CommitBlock != 110 || pending.StatusBlock != nil || pending.RootIndex != nil || pending.Total.Uint64() != 10 || pending.LeafCount != 2 || pending.Root != testStCkey(1) || pending.LedgerHash != testStCkey(101) {
			t.Fatalf("pending %+v", pending)
		}
		// a commit replaces the pending root
		AddStLedgerV2Commit(ctx, key, testLedgerV2Root(2, 6, 20, 3), 120, 1)
		if replaced := GetStLedgerV2Root(ctx, key, 1, 1); replaced.Status != StLedgerV2RootReplaced || *replaced.StatusBlock != 120 {
			t.Fatalf("replaced %+v", replaced)
		}
		// outcomes apply only to a pending root
		SetStLedgerV2RootDeleted(ctx, key, 1, 1, 121)
		finalized := testLedgerV2Root(1, 5, 10, 2)
		index := uint64(0)
		finalized.RootIndex = &index
		SetStLedgerV2RootFinalized(ctx, key, finalized, 122)
		if replaced := GetStLedgerV2Root(ctx, key, 1, 1); replaced.Status != StLedgerV2RootReplaced || *replaced.StatusBlock != 120 || replaced.RootIndex != nil {
			t.Fatalf("replaced root changed: %+v", replaced)
		}
		SetStLedgerV2RootDeleted(ctx, key, 1, 2, 130)
		if deleted := GetStLedgerV2Root(ctx, key, 1, 2); deleted.Status != StLedgerV2RootDeleted || *deleted.StatusBlock != 130 {
			t.Fatalf("deleted %+v", deleted)
		}
		if GetStLedgerV2PendingRoot(ctx, key, 1) != nil {
			t.Fatal("a deleted root is pending")
		}

		AddStLedgerV2Commit(ctx, key, testLedgerV2Root(3, 7, 30, 3), 140, 0)
		third := testLedgerV2Root(3, 7, 30, 3)
		third.RootIndex = &index
		SetStLedgerV2RootFinalized(ctx, key, third, 141)
		// a finalization whose commit was never mirrored is inserted
		fourth := testLedgerV2Root(4, 8, 40, 4)
		one := uint64(1)
		fourth.RootIndex = &one
		SetStLedgerV2RootFinalized(ctx, key, fourth, 150)
		if got := GetStLedgerV2FinalizedRoot(ctx, key, 1, 0); got == nil || got.Seq != 3 || *got.CommitBlock != 140 || *got.StatusBlock != 141 {
			t.Fatalf("root 0 %+v", got)
		}
		if got := GetLatestStLedgerV2FinalizedRoot(ctx, key, 1); got == nil || got.Seq != 4 || got.CommitBlock != nil || *got.RootIndex != 1 || got.Status != StLedgerV2RootFinalized {
			t.Fatalf("latest finalized %+v", got)
		}
		if got := GetNextUnrecordedStLedgerV2Root(ctx, key, 1); got == nil || got.Seq != 3 {
			t.Fatalf("next unrecorded %+v", got)
		}
		roots := GetStLedgerV2RootsFromEpoch(ctx, key, 1, 6)
		if len(roots) != 3 || roots[0].Seq != 2 || roots[1].Seq != 3 || roots[2].Seq != 4 {
			t.Fatalf("roots from epoch 6: %+v", roots)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("a finalization without its root index was accepted")
				}
			}()
			SetStLedgerV2RootFinalized(ctx, key, testLedgerV2Root(5, 9, 50, 4), 160)
		}()

		// a demand is keyed by seq, finalized root and account index
		AddStLedgerV2Demand(ctx, key, &StLedgerV2Demand{NoId: 1, Seq: 3, RootIndex: 1, Index: 1, Demander: "0xd1", DemandBlock: 142})
		AddStLedgerV2Demand(ctx, key, &StLedgerV2Demand{NoId: 1, Seq: 3, RootIndex: 1, Index: 0, Demander: "0xd2", DemandBlock: 143})
		AddStLedgerV2Demand(ctx, key, &StLedgerV2Demand{NoId: 1, Seq: 3, RootIndex: 1, Index: 1, Demander: "0xd3", DemandBlock: 144})
		AddStLedgerV2Demand(ctx, key, &StLedgerV2Demand{NoId: 1, Seq: 3, RootIndex: 0, Index: 1, Demander: "0xd4", DemandBlock: 145})
		SetStLedgerV2DemandAnswered(ctx, key, 1, 3, 1, 0, "0xa1", 146)
		SetStLedgerV2DemandAnswered(ctx, key, 1, 3, 1, 0, "0xa2", 147)
		open := GetOpenStLedgerV2Demands(ctx, key, 1, 3)
		if want := []*StLedgerV2Demand{{NoId: 1, Seq: 3, RootIndex: 0, Index: 1, Demander: "0xd4", DemandBlock: 145}, {NoId: 1, Seq: 3, RootIndex: 1, Index: 1, Demander: "0xd1", DemandBlock: 142}}; !reflect.DeepEqual(open, want) {
			t.Fatalf("open demands %+v", open)
		}
		SetStLedgerV2DemandAnswered(ctx, key, 1, 3, 0, 1, "0xa3", 148)
		if open := GetOpenStLedgerV2Demands(ctx, key, 1, 3); len(open) != 1 || open[0].RootIndex != 1 || open[0].Index != 1 {
			t.Fatalf("open demands %+v after answering root 0's", open)
		}
	})
}

// A finalized root's new accounts and amounts are recorded once, in root
// order, atomically; every refusal leaves nothing written.
// SV-5 (per-epoch amounts for readers)
func TestStLedgerV2RecordFinalizedRoots(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		for seq, epoch := range map[uint64]uint64{1: 5, 2: 6} {
			root := testLedgerV2Root(seq, epoch, 10*seq, seq+1)
			AddStLedgerV2Commit(ctx, key, root, 100*seq, 0)
			index := seq - 1
			root.RootIndex = &index
			SetStLedgerV2RootFinalized(ctx, key, root, 100*seq+1)
		}
		AddStLedgerV2Commit(ctx, key, testLedgerV2Root(3, 7, 30, 3), 300, 0)

		refused := func(name string, err error) {
			t.Helper()
			if !errors.Is(err, ErrStLedgerV2Record) {
				t.Fatalf("%s: %v", name, err)
			}
			if count := GetStLedgerV2AccountCount(ctx, key, 1); count != 0 {
				t.Fatalf("%s wrote %d accounts", name, count)
			}
		}
		first := [][32]byte{testStCkey(1), testStCkey(2)}
		firstAmounts := []*StLedgerV2Amount{testLedgerV2Amount(5, 1, 4), testLedgerV2Amount(5, 2, 6), testLedgerV2Amount(5, 3, 0)}
		refused("unknown seq", RecordStLedgerV2Root(ctx, key, 1, 9, 0, first, firstAmounts))
		refused("pending root", RecordStLedgerV2Root(ctx, key, 1, 3, 0, first, firstAmounts))
		refused("out of root order", RecordStLedgerV2Root(ctx, key, 1, 2, 2, [][32]byte{testStCkey(3)}, nil))
		refused("wrong first index", RecordStLedgerV2Root(ctx, key, 1, 1, 1, first, firstAmounts))
		refused("repeated new coldkey", RecordStLedgerV2Root(ctx, key, 1, 1, 0, [][32]byte{testStCkey(1), testStCkey(1)}, firstAmounts))
		refused("repeated amount", RecordStLedgerV2Root(ctx, key, 1, 1, 0, first, append(firstAmounts, testLedgerV2Amount(5, 1, 4))))

		if err := RecordStLedgerV2Root(ctx, key, 1, 1, 0, first, firstAmounts); err != nil {
			t.Fatal(err)
		}
		if err := RecordStLedgerV2Root(ctx, key, 1, 1, 0, nil, nil); err != nil {
			t.Fatalf("repeating a record: %v", err)
		}
		if recorded := GetStLedgerV2Root(ctx, key, 1, 1); recorded.RecordedTime == nil {
			t.Fatalf("root 0 not marked recorded: %+v", recorded)
		}
		if next := GetNextUnrecordedStLedgerV2Root(ctx, key, 1); next == nil || next.Seq != 2 {
			t.Fatalf("next unrecorded %+v", next)
		}
		if err := RecordStLedgerV2Root(ctx, key, 1, 2, 2, [][32]byte{testStCkey(2)}, nil); !errors.Is(err, ErrStLedgerV2Record) {
			t.Fatalf("an existing coldkey as a new account: %v", err)
		}
		if err := RecordStLedgerV2Root(ctx, key, 1, 2, 2, [][32]byte{testStCkey(3)}, []*StLedgerV2Amount{testLedgerV2Amount(6, 1, 7), testLedgerV2Amount(6, 3, 3)}); err != nil {
			t.Fatal(err)
		}
		if next := GetNextUnrecordedStLedgerV2Root(ctx, key, 1); next != nil {
			t.Fatalf("unrecorded root left: %+v", next)
		}

		if count := GetStLedgerV2AccountCount(ctx, key, 1); count != 3 {
			t.Fatalf("account count %d", count)
		}
		for coldkey, want := range map[byte]StLedgerV2Account{
			1: {NoId: 1, Index: 0, Coldkey: testStCkey(1), RootIndex: 0},
			2: {NoId: 1, Index: 1, Coldkey: testStCkey(2), RootIndex: 0},
			3: {NoId: 1, Index: 2, Coldkey: testStCkey(3), RootIndex: 1},
		} {
			if got := GetStLedgerV2AccountsForColdkey(ctx, key, testStCkey(coldkey)); len(got) != 1 || *got[0] != want {
				t.Fatalf("coldkey %d accounts %+v", coldkey, got)
			}
		}
		if got := GetStLedgerV2AccountsForColdkey(ctx, key, testStCkey(9)); len(got) != 0 {
			t.Fatalf("unknown coldkey accounts %+v", got)
		}
		epoch5 := GetStLedgerV2Amounts(ctx, key, 1, 5)
		if len(epoch5) != 2 || epoch5[0].Coldkey != testStCkey(1) || epoch5[0].Amount.Uint64() != 4 || epoch5[0].RootIndex != 0 || epoch5[0].NoId != 1 || epoch5[0].Epoch != 5 || epoch5[1].Amount.Uint64() != 6 {
			t.Fatalf("epoch 5 amounts %+v", epoch5)
		}
		amounts := GetStLedgerV2AmountsForColdkey(ctx, key, testStCkey(1))
		if len(amounts) != 2 || amounts[0].Epoch != 5 || amounts[1].Epoch != 6 || amounts[1].Amount.Uint64() != 7 || amounts[1].RootIndex != 1 {
			t.Fatalf("coldkey 1 amounts %+v", amounts)
		}
	})
}
