// The validator head summaries the operator has accepted (LEADERTOP.md §6.1;
// sn docs/spec/head-summary-v1.md), one per (netuid, validator hotkey, native
// epoch), with one row per fleet identity of each eligible head UID: the
// validator's exact score, rank and selection, which the board shows for
// claimed fleets. A later native_snapshot_block of the same tempo replaces a
// stored summary; any other keeps it. Kept for the board's 7 days.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// One fleet identity's result in a summary: its UID entry's rank, exact score
// and selection.
type SnHeadSummaryFleetScore struct {
	Key  SnFleetKey
	Rank int
	// the exact reduced score of the fleet's UID
	ScoreNumerator   string
	ScoreDenominator string
	Score            float64
	Selected         bool
}

// One stored summary.
type SnHeadSummary struct {
	Netuid                  uint16
	ValidatorHotkey         [32]byte
	ValidatorUid            uint16
	NativeEpoch             uint64
	NativeSnapshotBlock     uint64
	NativeSnapshotHash      [32]byte
	EvmSnapshotBlock        uint64
	EvmSnapshotHash         [32]byte
	SettlementEpoch         uint64
	PolicyId                uint64
	PolicyHash              [32]byte
	MeasurementArtifactHash string
	MaximumHeadFleets       uint16
	// the eligible head UIDs: a rank's "of"
	EntryCount    int
	SelectedCount int
	SignedAt      time.Time
	SigningHash   string
	ReceivedAt    time.Time
	// the exact canonical body, kept so the signature can be verified again
	Body []byte
	// by rank, then fleet id and generation
	Fleets []*SnHeadSummaryFleetScore
}

// The score, rank and selection of one fleet identity, if the summary ranks it.
func (self *SnHeadSummary) FleetScore(key SnFleetKey) (*SnHeadSummaryFleetScore, bool) {
	for _, fleet := range self.Fleets {
		if fleet.Key == key {
			return fleet, true
		}
	}
	return nil, false
}

// PutSnHeadSummary stores a summary unless the same tempo's summary of the
// same validator at an equal or later native snapshot block is stored; it
// reports whether it stored.
func PutSnHeadSummary(ctx context.Context, summary *SnHeadSummary) bool {
	stored := false
	server.Tx(ctx, func(tx server.PgTx) {
		stored = false
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_head_summary (
					netuid, validator_hotkey, native_epoch, native_snapshot_block, native_snapshot_hash,
					evm_snapshot_block, evm_snapshot_hash, settlement_epoch, policy_id, policy_hash,
					measurement_artifact_hash, validator_uid, maximum_head_fleets, entry_count, selected_count,
					signed_at, signing_hash, received_at, body
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
				ON CONFLICT (netuid, validator_hotkey, native_epoch) DO UPDATE
				SET
					native_snapshot_block = EXCLUDED.native_snapshot_block,
					native_snapshot_hash = EXCLUDED.native_snapshot_hash,
					evm_snapshot_block = EXCLUDED.evm_snapshot_block,
					evm_snapshot_hash = EXCLUDED.evm_snapshot_hash,
					settlement_epoch = EXCLUDED.settlement_epoch,
					policy_id = EXCLUDED.policy_id,
					policy_hash = EXCLUDED.policy_hash,
					measurement_artifact_hash = EXCLUDED.measurement_artifact_hash,
					validator_uid = EXCLUDED.validator_uid,
					maximum_head_fleets = EXCLUDED.maximum_head_fleets,
					entry_count = EXCLUDED.entry_count,
					selected_count = EXCLUDED.selected_count,
					signed_at = EXCLUDED.signed_at,
					signing_hash = EXCLUDED.signing_hash,
					received_at = EXCLUDED.received_at,
					body = EXCLUDED.body
				WHERE sn_head_summary.native_snapshot_block < EXCLUDED.native_snapshot_block
			`,
			int(summary.Netuid), summary.ValidatorHotkey[:], int64(summary.NativeEpoch), int64(summary.NativeSnapshotBlock), summary.NativeSnapshotHash[:],
			int64(summary.EvmSnapshotBlock), summary.EvmSnapshotHash[:], int64(summary.SettlementEpoch), int64(summary.PolicyId), summary.PolicyHash[:],
			summary.MeasurementArtifactHash, int(summary.ValidatorUid), int(summary.MaximumHeadFleets), summary.EntryCount, summary.SelectedCount,
			summary.SignedAt.UTC(), summary.SigningHash, summary.ReceivedAt.UTC(), summary.Body,
		))
		if tag.RowsAffected() != 1 {
			return
		}
		stored = true
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM sn_head_summary_fleet WHERE netuid = $1 AND validator_hotkey = $2 AND native_epoch = $3`,
			int(summary.Netuid), summary.ValidatorHotkey[:], int64(summary.NativeEpoch),
		))
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, fleet := range summary.Fleets {
				batch.Queue(
					`
						INSERT INTO sn_head_summary_fleet (
							netuid, validator_hotkey, native_epoch, uid, fleet_id, generation, hotkey,
							rank, score_numerator, score_denominator, score, selected
						)
						VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
					`,
					int(summary.Netuid), summary.ValidatorHotkey[:], int64(summary.NativeEpoch), int(fleet.Key.Uid), fleet.Key.FleetId[:], int64(fleet.Key.Generation), fleet.Key.Hotkey[:],
					fleet.Rank, fleet.ScoreNumerator, fleet.ScoreDenominator, fleet.Score, fleet.Selected,
				)
			}
		})
	})
	return stored
}

// GetLatestSnHeadSummary returns the stored summary with the newest native
// epoch for netuid, of validatorHotkey when it is given, or nil.
func GetLatestSnHeadSummary(ctx context.Context, netuid uint16, validatorHotkey *[32]byte) *SnHeadSummary {
	var summary *SnHeadSummary
	server.Db(ctx, func(conn server.PgConn) {
		summary = nil
		var hotkeyArg []byte
		if validatorHotkey != nil {
			hotkeyArg = validatorHotkey[:]
		}
		rows, err := conn.Query(
			ctx,
			`
				SELECT
					validator_hotkey, native_epoch, native_snapshot_block, native_snapshot_hash,
					evm_snapshot_block, evm_snapshot_hash, settlement_epoch, policy_id, policy_hash,
					measurement_artifact_hash, validator_uid, maximum_head_fleets, entry_count, selected_count,
					signed_at, signing_hash, received_at, body
				FROM sn_head_summary
				WHERE netuid = $1 AND ($2::bytea IS NULL OR validator_hotkey = $2)
				ORDER BY native_epoch DESC, validator_hotkey
				LIMIT 1
			`,
			int(netuid), hotkeyArg,
		)
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				return
			}
			summary = &SnHeadSummary{Netuid: netuid, Fleets: []*SnHeadSummaryFleetScore{}}
			var hotkey, nativeHash, evmHash, policyHash []byte
			var nativeEpoch, nativeBlock, evmBlock, settlementEpoch, policyId int64
			var validatorUid, maximumHeadFleets int
			server.Raise(rows.Scan(
				&hotkey, &nativeEpoch, &nativeBlock, &nativeHash,
				&evmBlock, &evmHash, &settlementEpoch, &policyId, &policyHash,
				&summary.MeasurementArtifactHash, &validatorUid, &maximumHeadFleets, &summary.EntryCount, &summary.SelectedCount,
				&summary.SignedAt, &summary.SigningHash, &summary.ReceivedAt, &summary.Body,
			))
			summary.ValidatorHotkey, summary.NativeSnapshotHash, summary.EvmSnapshotHash, summary.PolicyHash = [32]byte(hotkey), [32]byte(nativeHash), [32]byte(evmHash), [32]byte(policyHash)
			summary.NativeEpoch, summary.NativeSnapshotBlock, summary.EvmSnapshotBlock = uint64(nativeEpoch), uint64(nativeBlock), uint64(evmBlock)
			summary.SettlementEpoch, summary.PolicyId = uint64(settlementEpoch), uint64(policyId)
			summary.ValidatorUid, summary.MaximumHeadFleets = uint16(validatorUid), uint16(maximumHeadFleets)
		})
		if summary == nil {
			return
		}
		rows, err = conn.Query(
			ctx,
			`
				SELECT uid, fleet_id, generation, hotkey, rank, score_numerator, score_denominator, score, selected
				FROM sn_head_summary_fleet
				WHERE netuid = $1 AND validator_hotkey = $2 AND native_epoch = $3
				ORDER BY rank, fleet_id, generation
			`,
			int(netuid), summary.ValidatorHotkey[:], int64(summary.NativeEpoch),
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				fleet := &SnHeadSummaryFleetScore{}
				var uid int
				var fleetId, hotkey []byte
				var generation int64
				server.Raise(rows.Scan(&uid, &fleetId, &generation, &hotkey, &fleet.Rank, &fleet.ScoreNumerator, &fleet.ScoreDenominator, &fleet.Score, &fleet.Selected))
				fleet.Key = SnFleetKey{FleetId: [32]byte(fleetId), Hotkey: [32]byte(hotkey), Generation: uint64(generation), Uid: uint16(uid)}
				summary.Fleets = append(summary.Fleets, fleet)
			}
		})
	})
	return summary
}

// Removes the summaries received before minReceivedAt; returns how many.
func RemoveSnHeadSummariesBefore(ctx context.Context, minReceivedAt time.Time) int64 {
	var removed int64
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM sn_head_summary_fleet
				USING sn_head_summary
				WHERE
					sn_head_summary_fleet.netuid = sn_head_summary.netuid AND
					sn_head_summary_fleet.validator_hotkey = sn_head_summary.validator_hotkey AND
					sn_head_summary_fleet.native_epoch = sn_head_summary.native_epoch AND
					sn_head_summary.received_at < $1
			`,
			minReceivedAt.UTC(),
		))
		tag := server.RaisePgResult(tx.Exec(ctx, `DELETE FROM sn_head_summary WHERE received_at < $1`, minReceivedAt.UTC()))
		removed = tag.RowsAffected()
	})
	return removed
}
