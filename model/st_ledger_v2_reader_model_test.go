package model

// st_ledger_v2_reader_model_test.go — settlement v2's per-epoch amounts for
// the earnings readers (VERSION2-FINAL.md §8.4): SV-5.

import (
	"context"
	"reflect"
	"testing"

	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
)

func testStLedgerV2Resolution(clientId server.Id, networkId server.Id, coldkey [32]byte) *StPayoutWalletResolution {
	return &StPayoutWalletResolution{ClientId: clientId, NetworkId: networkId, Mode: protocol.EarningWalletModeProvider, Coldkey: coldkey,
		ConsentHash: [32]byte{1}, ConsentGeneration: 1, HeadHash: [32]byte{1}, HeadGeneration: 1}
}

func testStLedgerV2Amount(epoch uint64, coldkey [32]byte, amount uint64) *StLedgerV2Amount {
	return &StLedgerV2Amount{Epoch: epoch, Coldkey: coldkey, Amount: *uint256.NewInt(amount)}
}

// SV-5: a finalized root's recorded amounts are read per covered epoch: the
// epochs that recorded finalized roots cover, newest first; each network's
// amount of an epoch, each coldkey's amount going to the network of the
// smallest client that selected it in that epoch and an amount no resolution
// selected to no network; and each epoch's total. A root covers three epochs.
func TestStLedgerV2ReaderAmountsPerEpoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key, other := testStDeploymentKey, StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		a, b, c, d := testStCkey(1), testStCkey(2), testStCkey(3), testStCkey(4)
		n1, n2, n3 := server.Id{0x01}, server.Id{0x02}, server.Id{0x03}
		c0, c1, c2, c3 := server.Id{0x10}, server.Id{0x11}, server.Id{0x12}, server.Id{0x13}

		// Epochs 5 to 7 are covered by finalized root 0; epoch 8 is built for
		// a root that is not finalized.
		for epoch := uint64(5); epoch <= 8; epoch++ {
			if err := RecordStLedgerV2EpochRows(ctx, key, 1, epoch, []string{}); err != nil {
				t.Fatal(err)
			}
		}
		for epoch, resolutions := range map[uint64][]*StPayoutWalletResolution{
			5: {testStLedgerV2Resolution(c1, n1, a), testStLedgerV2Resolution(c2, n2, b)},
			// c0 is smaller than c3, so C belongs to N2 in epoch 6
			6: {testStLedgerV2Resolution(c0, n2, c), testStLedgerV2Resolution(c2, n2, b), testStLedgerV2Resolution(c3, n1, c)},
			7: {testStLedgerV2Resolution(c1, n1, a)},
		} {
			if err := AddStPayoutWalletResolutions(ctx, key, epoch, 1, resolutions); err != nil {
				t.Fatal(err)
			}
		}
		// Another pool's and another deployment's smaller clients change nothing.
		for _, scope := range []struct {
			key  StDeploymentKey
			noId uint64
		}{{key, 2}, {other, 1}} {
			if err := AddStPayoutWalletResolutions(ctx, scope.key, 5, scope.noId, []*StPayoutWalletResolution{testStLedgerV2Resolution(server.Id{0x01}, n3, a)}); err != nil {
				t.Fatal(err)
			}
		}
		UpsertStEpoch(ctx, key, &StEpoch{Epoch: 6, StartBlock: 302_400, CommitDeadlineBlock: 353_000, TrailsDeadlineBlock: 353_100, FinalizeBlock: 353_200, Status: StEpochStatusOpen})

		root := &StLedgerV2Root{NoId: 1, Seq: 1, Epoch: 7, Root: [32]byte{7}, Total: *uint256.NewInt(5100), LeafCount: 4, LedgerHash: [32]byte{8}}
		AddStLedgerV2Commit(ctx, key, root, 352_810, 0)
		zero := uint64(0)
		root.RootIndex = &zero
		SetStLedgerV2RootFinalized(ctx, key, root, 367_210)
		if epochs := GetStLedgerV2FinalizedEpochs(ctx, key, 1, 10); len(epochs) != 0 {
			t.Fatalf("epochs of a root not recorded: %+v", epochs)
		}
		amounts := []*StLedgerV2Amount{
			testStLedgerV2Amount(5, a, 500), testStLedgerV2Amount(5, b, 500),
			testStLedgerV2Amount(6, b, 1500), testStLedgerV2Amount(6, c, 1500),
			testStLedgerV2Amount(7, a, 500), testStLedgerV2Amount(7, d, 100),
		}
		if err := RecordStLedgerV2Root(ctx, key, 1, 1, 0, [][32]byte{a, b, c, d}, amounts); err != nil {
			t.Fatal(err)
		}

		epochs := GetStLedgerV2FinalizedEpochs(ctx, key, 1, 10)
		want := []*StEpoch{
			{Epoch: 7, Status: StEpochStatusOpen},
			{Epoch: 6, StartBlock: 302_400, CommitDeadlineBlock: 353_000, TrailsDeadlineBlock: 353_100, FinalizeBlock: 353_200, Status: StEpochStatusOpen},
			{Epoch: 5, Status: StEpochStatusOpen},
		}
		if !reflect.DeepEqual(epochs, want) {
			t.Fatalf("finalized epochs %+v", epochs)
		}
		if epochs := GetStLedgerV2FinalizedEpochs(ctx, key, 1, 2); len(epochs) != 2 || epochs[0].Epoch != 7 || epochs[1].Epoch != 6 {
			t.Fatalf("two newest epochs %+v", epochs)
		}
		for _, scope := range []struct {
			key  StDeploymentKey
			noId uint64
		}{{key, 2}, {other, 1}} {
			if epochs := GetStLedgerV2FinalizedEpochs(ctx, scope.key, scope.noId, 10); len(epochs) != 0 {
				t.Fatalf("%s pool %d epochs %+v", scope.key, scope.noId, epochs)
			}
		}

		amount := func(amount uint64, total uint64) *StLedgerV2EpochAmount {
			return &StLedgerV2EpochAmount{Amount: *uint256.NewInt(amount), Total: *uint256.NewInt(total)}
		}
		for networkId, want := range map[server.Id]map[uint64]*StLedgerV2EpochAmount{
			n1: {5: amount(500, 1000), 6: amount(0, 3000), 7: amount(500, 600)},
			n2: {5: amount(500, 1000), 6: amount(3000, 3000), 7: amount(0, 600)},
			n3: {5: amount(0, 1000), 6: amount(0, 3000), 7: amount(0, 600)},
		} {
			if got := GetStLedgerV2AmountsForNetwork(ctx, key, networkId, 1, []uint64{4, 5, 6, 7, 8}); !reflect.DeepEqual(got, want) {
				t.Fatalf("network %s amounts %+v", networkId, got)
			}
		}
		if got := GetStLedgerV2AmountsForNetwork(ctx, key, n1, 1, []uint64{}); len(got) != 0 {
			t.Fatalf("no epochs: %+v", got)
		}
		if got := GetStLedgerV2AmountsForNetwork(ctx, other, n1, 1, []uint64{5}); len(got) != 0 {
			t.Fatalf("another deployment's amounts %+v", got)
		}

		for _, test := range []struct {
			epoch   uint64
			amounts map[server.Id]uint256.Int
			total   uint64
		}{
			{5, map[server.Id]uint256.Int{n1: *uint256.NewInt(500), n2: *uint256.NewInt(500)}, 1000},
			{6, map[server.Id]uint256.Int{n2: *uint256.NewInt(3000)}, 3000},
			{7, map[server.Id]uint256.Int{n1: *uint256.NewInt(500)}, 600},
			{8, map[server.Id]uint256.Int{}, 0},
		} {
			amounts, total := GetStLedgerV2NetworkAmounts(ctx, key, 1, test.epoch)
			if !reflect.DeepEqual(amounts, test.amounts) || total.Uint64() != test.total {
				t.Fatalf("epoch %d network amounts %+v total %s", test.epoch, amounts, total.Dec())
			}
		}
	})
}
