package controller

// st_ledger_v2_rebuild_test.go — the driver's side of the operator's rebuild
// command (VERSION2-FINAL.md §6.4 fixed inputs): a requested epoch's rows are
// built again for the next root unless a pending root that can still finalize
// lists the recorded rows.

import (
	"reflect"
	"slices"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §6.4 (fixed inputs), SV-1: epoch 5's root is pending with its quorum of
// approvals and an open demand when the operator requests epoch 5's rebuild,
// so it may still finalize: the root of epoch 6 reuses epoch 5's recorded
// rows and the request waits. That root is short of its quorum when epoch 7's
// root is built, so epoch 5's rows are built again and replace the record,
// which ends the request; epoch 6, never requested, reuses its record.
func TestStLedgerV2DriverRebuildsRequestedEpochs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		a, b := ledgerV2Coldkey(1), ledgerV2Coldkey(2)
		key := f.cfg.DeploymentKey()
		f.chain.emission[5], f.chain.emission[6], f.chain.emission[7] = 1000, 1000, 1000
		f.builder.earners[5], f.builder.earners[6], f.builder.earners[7] = [][32]byte{a}, [][32]byte{a}, [][32]byte{a}
		record := func(epoch uint64) model.StLedgerV2EpochRows {
			return *model.GetStLedgerV2EpochRows(f.ctx, key, 1, epoch, epoch)[epoch]
		}

		f.chain.head = f.chain.end(5) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusConfirmed, false, "")
		recorded := record(5)
		f.chain.approvals, f.chain.pending.OpenDemands = 1, 1
		if err := StLedgerV2RebuildEpoch(f.ctx, 5, "rebuilder@example"); err != nil {
			t.Fatal(err)
		}
		// rows built again would differ
		f.builder.earners[5] = [][32]byte{b}

		f.chain.head = f.chain.end(6) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		if input := f.builder.inputs[1]; input.Epochs[0].Rows == nil || !reflect.DeepEqual(input.Epochs[0].Rows.ProviderChunks, recorded.ProviderChunks) {
			t.Fatalf("epoch 5 was built again under a pending root that may finalize: %+v", input.Epochs[0])
		}
		requested := recorded
		requested.RebuildRequestedBy = "rebuilder@example"
		if got := record(5); !reflect.DeepEqual(got, requested) || f.chain.pending.Epoch != 6 {
			t.Fatalf("epoch 5 record %+v, pending %+v", got, f.chain.pending)
		}

		f.chain.head = f.chain.end(7) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 7, 0, 0), model.StPublishStatusConfirmed, false, "")
		input, build := f.builder.inputs[2], f.builder.builds[2]
		if epochs := input.Epochs; epochs[0].Rows != nil || epochs[1].Rows == nil || epochs[2].Rows != nil {
			t.Fatalf("covered epochs %+v %+v %+v", epochs[0], epochs[1], epochs[2])
		}
		got := record(5)
		if !reflect.DeepEqual(got.ProviderChunks, build.Epochs[0].ProviderChunks) || slices.Equal(got.ProviderChunks, recorded.ProviderChunks) || got.RebuildRequestedBy != "" {
			t.Fatalf("rebuilt epoch 5 record %+v", got)
		}
		if !reflect.DeepEqual(build.Accounts, ledgerV2Accounts(a, 2000, b, 1000)) || f.chain.pending.Epoch != 7 {
			t.Fatalf("accounts %v pending %+v", build.Accounts, f.chain.pending)
		}
	})
}
