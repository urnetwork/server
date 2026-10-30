package controller

// Delayed cut recovery changes exact fleet availability without advancing the
// network estimate or confusing legacy numeric storage with verified zero.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol/headsummary"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestSnTopReplicaLateRecoveryPreservesNetworkStateDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := t.Context()
		fixture := newSnTopDbFixture(tb, ctx)
		signed := newSnTopReplicaTestFixture(t)
		signed.cfg.Enabled = true
		SetStConfig(signed.cfg)
		snTopDbResetChainState()
		keys := verifyServerKeysInstance
		SetVerifyServerKeys([]*VerifyServerKey{})
		tb.Cleanup(func() { SetVerifyServerKeys(keys) })
		server.UseLocalBlobStoreForTest(tb, tb.TempDir(), "replica", 16*1024*1024)
		signed.store, _ = server.LoadBlobStore()
		signed.publish(t)
		networkId, clientSession := fixture.network(tb, "replica")
		defer clientSession.Cancel()
		clientId := fixture.provider(tb, networkId, "198.51.100.9", fixture.end.Add(-time.Minute))
		key := model.SnTopFleetKey{FleetId: [32]byte{0x32}, Hotkey: [32]byte{0x33}, Generation: 2, Uid: 7}
		var summary *SnTopValidatorSummary
		restore := SetSnTopSources(SnTopSources{
			ClaimedFleets: func(context.Context, uint64) []model.SnTopFleetInput {
				return []model.SnTopFleetInput{{Key: key, ClientIds: []server.Id{clientId}, NetworkIds: []server.Id{networkId}}}
			},
			ValidatorSummary: func(context.Context) *SnTopValidatorSummary { return summary },
			Incentive:        func(context.Context) *SnTopIncentive { return nil },
		})
		defer restore()
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()
		result, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(tb, err, nil)
		connect.AssertEqual(tb, result.Skipped, "")
		before := model.GetLatestSnTopSnapshot(ctx)
		beforeStates, _ := model.GetSnTopEmaState(ctx)
		// Old writers omit the new availability column. Their existing numeric
		// value stays in storage but cannot masquerade as a verified replica.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE sn_top_fleet SET replica_score = 9 WHERE snapshot_id = $1`, before.SnapshotId))
		})
		row := model.GetSnTopFleets(ctx, before.SnapshotId, []server.Id{networkId})[networkId][0]
		wire, err := json.Marshal(snTopFleetFromModel(row))
		connect.AssertEqual(tb, err, nil)
		if !strings.Contains(string(wire), `"replica_score":null`) {
			tb.Fatalf("legacy numeric value exposed: %s", wire)
		}

		// Legacy rows and a migrated EMA with no predecessor cannot block or
		// impersonate a fresh authenticated empty-prior namespace.
		legacy := *signed.summary
		legacy.Schema, legacy.NativeEpoch = headsummary.Schema, signed.summary.NativeEpoch-2
		legacy.PreviousMeasurementArtifactHash = ""
		legacy.EvidenceEmptyPrior = false
		legacy.EvidenceRecipient, legacy.EvidenceOperators, legacy.EvidenceCuts, legacy.EvidencePolicy = 0, nil, nil, nil
		legacyBody := signed.key.sign(t, &legacy)
		legacyStored, err := snStoredHeadSummary(&legacy, legacyBody, signed.stored.ReceivedAt)
		connect.AssertEqual(tb, err, nil)
		connect.AssertEqual(tb, model.PutSnHeadSummary(ctx, legacyStored), true)
		migrated := *signed.summary
		migrated.NativeEpoch--
		migrated.EvidenceEmptyPrior = false
		migratedBody := signed.key.sign(t, &migrated)
		migratedStored, err := snStoredHeadSummary(&migrated, migratedBody, signed.stored.ReceivedAt)
		connect.AssertEqual(tb, err, nil)
		connect.AssertEqual(tb, model.PutSnHeadSummary(ctx, migratedStored), true)
		genesis := signed.stored
		connect.AssertEqual(tb, model.PutSnHeadSummary(ctx, genesis), true)
		signed.summary.NativeEpoch++
		signed.summary.PreviousMeasurementArtifactHash = genesis.MeasurementArtifactHash
		signed.summary.EvidenceEmptyPrior = false
		signed.summary.MeasurementArtifactHash = "sha256:" + headsummary.Hex32([32]byte{0x61})[2:]
		signed.sign(t)
		connect.AssertEqual(tb, model.PutSnHeadSummary(ctx, signed.stored), true)
		selected := model.GetNextSnHeadSummary(ctx, signed.stored.Netuid, signed.stored.ValidatorHotkey, nil, signed.stored.NativeEpoch)
		connect.AssertEqual(tb, selected.NativeEpoch, genesis.NativeEpoch)
		summary = snTopValidatorSummaryFromModel(signed.stored)
		again, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(tb, err, nil)
		connect.AssertEqual(tb, again.Skipped, "folded")
		after := model.GetLatestSnTopSnapshot(ctx)
		afterStates, _ := model.GetSnTopEmaState(ctx)
		if after.SnapshotId != before.SnapshotId || !after.ComputedAt.Equal(before.ComputedAt) || !reflect.DeepEqual(beforeStates, afterStates) {
			tb.Fatal("late replica refolded network estimate")
		}
		connect.AssertEqual(tb, after.ReplicaComplete, true)
		connect.AssertEqual(tb, after.ReplicaCheckpoint.NativeEpoch, signed.stored.NativeEpoch)
		row = model.GetSnTopFleets(ctx, after.SnapshotId, []server.Id{networkId})[networkId][0]
		wire, err = json.Marshal(snTopFleetFromModel(row))
		connect.AssertEqual(tb, err, nil)
		if !strings.Contains(string(wire), `"replica_score":0`) {
			tb.Fatalf("verified zero missing: %s", wire)
		}

		// A later estimate retains the checkpoint through a temporary object
		// failure. Recovery must retry even though its target hash already matches.
		server.UseLocalBlobStoreForTest(tb, tb.TempDir(), "missing", 16*1024*1024)
		later := fixture.anchor.Add(model.SnTopTempoDuration)
		next, err := computeSnTopSnapshotAt(&ComputeSnTopSnapshotArgs{}, anonymous, later)
		connect.AssertEqual(tb, err, nil)
		connect.AssertEqual(tb, next.Skipped, "")
		unavailable := model.GetLatestSnTopSnapshot(ctx)
		connect.AssertEqual(tb, unavailable.ReplicaComplete, false)
		connect.AssertEqual(tb, unavailable.ReplicaCheckpoint.MeasurementHash, signed.stored.MeasurementArtifactHash)
		frozen, _ := model.GetSnTopEmaState(ctx)
		signed.store, _ = server.LoadBlobStore()
		signed.publish(t)
		_, err = computeSnTopSnapshotAt(&ComputeSnTopSnapshotArgs{}, anonymous, later.Add(time.Minute))
		connect.AssertEqual(tb, err, nil)
		recovered := model.GetLatestSnTopSnapshot(ctx)
		connect.AssertEqual(tb, recovered.SnapshotId, unavailable.SnapshotId)
		connect.AssertEqual(tb, recovered.ReplicaComplete, true)
		got, _ := model.GetSnTopEmaState(ctx)
		if !reflect.DeepEqual(got, frozen) {
			tb.Fatal("checkpoint retry refolded network estimate")
		}
	})
}
