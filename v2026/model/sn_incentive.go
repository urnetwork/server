// The subnet's on-chain incentive census, one snapshot per native tempo
// (LEADERTOP.md §6.2, design question 10). The incentive reader
// (controller/sn_incentive.go) reads it once per tempo at the finalized head
// after SubnetEpochIndex advances. Each snapshot keeps every UID's Incentive,
// LastUpdate, ValidatorPermit and ValidatorTrust, the holder (Keys and
// BlockAtRegistration) of each permitted validator's and each bound fleet's
// UID, the permitted validators' applied rows, and whether the incentive is
// stale. Snapshots are kept for the board's 7 days.
package model

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Why a snapshot's incentive does not reflect the reference validator's head
// ranking. Incentive is Yuma's output; while one validator holds most of the
// stake, the UIDs earning incentive are that validator's row.
const (
	// the reference validator's applied row is the stopgap fallback row
	// (pool 169 and reserves 170 and 250), which ranks no head fleet
	SnIncentiveStaleFallbackRow = "fallback_row"
	// the reference validator last set weights longer ago than the subnet's
	// activity cutoff, so Yuma no longer counts its row
	SnIncentiveStaleReferenceInactive = "reference_inactive"
	// the reference validator's ValidatorTrust is well below 1: consensus no
	// longer follows its row
	SnIncentiveStaleLowValidatorTrust = "low_validator_trust"
	// no reference validator holds a permit on the subnet
	SnIncentiveStaleNoReference = "no_reference_validator"
)

// Every stale reason, for metric label initialization.
var SnIncentiveStaleReasons = []string{SnIncentiveStaleFallbackRow, SnIncentiveStaleReferenceInactive, SnIncentiveStaleLowValidatorTrust, SnIncentiveStaleNoReference}

// One applied weight.
type SnWeightEntry struct {
	Uid   uint16 `json:"uid"`
	Value uint16 `json:"value"`
}

// One permitted validator's applied row.
type SnValidatorWeightRow struct {
	Uid uint16          `json:"uid"`
	Row []SnWeightEntry `json:"row"`
}

// One UID of a snapshot.
type SnIncentiveUid struct {
	Uid uint16
	// SubtensorModule::Incentive of the subnet's mechanism 0 at this UID, as
	// the chain stores it: Yuma's per-UID incentive, normalized to sum to 1
	// across the subnet, then stored as a PerU16 where 65535 is 1. Use
	// IncentiveFraction for the UID's share.
	Incentive       uint16
	LastUpdate      uint64
	ValidatorPermit bool
	ValidatorTrust  uint16
	// the holder was read: every permitted validator's and bound fleet's UID
	HolderRead          bool
	Hotkey              [32]byte
	BlockAtRegistration uint64
}

// IncentiveFraction is the UID's share of the subnet's incentive, a fraction
// in [0, 1]: the stored PerU16 over 65535. This is the unit of the board's
// fleets[].incentive, which the apps render as a percent. The UIDs' shares sum
// to about 1, short only by the chain's per-UID rounding.
func (self *SnIncentiveUid) IncentiveFraction() float64 {
	return float64(self.Incentive) / 65535
}

// One tempo's incentive census.
type SnIncentiveSnapshot struct {
	SubnetEpochIndex uint64
	Block            uint64
	BlockHash        string
	ReadAt           time.Time
	Netuid           uint16
	SubnetworkN      uint16
	Tempo            uint16
	ActivityCutoff   uint64
	// the reference validator's UID, or nil when none holds a permit
	ReferenceUid            *uint16
	ReferenceLastUpdate     uint64
	ReferenceValidatorTrust uint16
	FallbackRow             bool
	Stale                   bool
	StaleReasons            []string
	ValidatorRowsComplete   bool
	ValidatorRows           []SnValidatorWeightRow
	ResponseBytes           int64
	// every UID below SubnetworkN, ascending
	Uids []*SnIncentiveUid
}

// The incentive of uid when its holder at the snapshot is hotkey; its
// IncentiveFraction is the UID's share in [0, 1]. A UID that changed hands
// earns for its new holder, so another hotkey's UID is not found.
func (self *SnIncentiveSnapshot) IncentiveFor(uid uint16, hotkey [32]byte) (*SnIncentiveUid, bool) {
	if int(uid) >= len(self.Uids) {
		return nil, false
	}
	row := self.Uids[uid]
	if row.Uid != uid || !row.HolderRead || row.Hotkey != hotkey {
		return nil, false
	}
	return row, true
}

// The UID holders the snapshot read.
func (self *SnIncentiveSnapshot) Holders() *SnUidHolders {
	holders := &SnUidHolders{SubnetEpochIndex: self.SubnetEpochIndex, Block: self.Block, ReadAt: self.ReadAt, Holders: map[uint16]SnUidHolder{}}
	for _, row := range self.Uids {
		if row.HolderRead {
			holders.Holders[row.Uid] = SnUidHolder{Hotkey: row.Hotkey, BlockAtRegistration: row.BlockAtRegistration}
		}
	}
	return holders
}

// InsertSnIncentiveSnapshot stores a snapshot. A tempo is stored once: a
// second read of the same index keeps the first.
func InsertSnIncentiveSnapshot(ctx context.Context, deploymentKey StDeploymentKey, snapshot *SnIncentiveSnapshot) bool {
	key := requireStDeploymentKey(deploymentKey)
	rows, err := json.Marshal(snapshot.ValidatorRows)
	server.Raise(err)
	var referenceUid *int
	if snapshot.ReferenceUid != nil {
		uid := int(*snapshot.ReferenceUid)
		referenceUid = &uid
	}
	inserted := false
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_incentive_snapshot (
					deployment_key, subnet_epoch_index, block_number, block_hash, read_at,
					netuid, subnetwork_n, tempo, activity_cutoff,
					reference_uid, reference_last_update, reference_validator_trust,
					fallback_row, stale, stale_reasons,
					validator_rows_complete, validator_rows_json, response_bytes
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
				ON CONFLICT (deployment_key, subnet_epoch_index) DO NOTHING
			`,
			key, int64(snapshot.SubnetEpochIndex), int64(snapshot.Block), snapshot.BlockHash, snapshot.ReadAt.UTC(),
			int(snapshot.Netuid), int(snapshot.SubnetworkN), int(snapshot.Tempo), int64(snapshot.ActivityCutoff),
			referenceUid, int64(snapshot.ReferenceLastUpdate), int(snapshot.ReferenceValidatorTrust),
			snapshot.FallbackRow, snapshot.Stale, strings.Join(snapshot.StaleReasons, ","),
			snapshot.ValidatorRowsComplete, string(rows), snapshot.ResponseBytes,
		))
		if tag.RowsAffected() != 1 {
			return
		}
		inserted = true
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, row := range snapshot.Uids {
				var hotkey []byte
				var registration *int64
				if row.HolderRead {
					hotkey = row.Hotkey[:]
					block := int64(row.BlockAtRegistration)
					registration = &block
				}
				batch.Queue(
					`
						INSERT INTO sn_incentive_uid (
							deployment_key, subnet_epoch_index, uid, incentive, last_update,
							validator_permit, validator_trust, hotkey, block_at_registration
						)
						VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
					`,
					key, int64(snapshot.SubnetEpochIndex), int(row.Uid), int(row.Incentive), int64(row.LastUpdate),
					row.ValidatorPermit, int(row.ValidatorTrust), hotkey, registration,
				)
			}
		})
	})
	return inserted
}

// The newest stored tempo's index, read time and the subnet's tempo then.
func GetLatestSnIncentiveRead(ctx context.Context, deploymentKey StDeploymentKey) (index uint64, readAt time.Time, tempo uint16, ok bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT subnet_epoch_index, read_at, tempo FROM sn_incentive_snapshot WHERE deployment_key = $1 ORDER BY subnet_epoch_index DESC LIMIT 1`, key)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var value int64
				var tempoValue int
				server.Raise(rows.Scan(&value, &readAt, &tempoValue))
				index, tempo, ok = uint64(value), uint16(tempoValue), true
			}
		})
	})
	return
}

// GetLatestSnIncentiveSnapshot returns the newest stored tempo with every
// UID, or nil before the first read.
func GetLatestSnIncentiveSnapshot(ctx context.Context, deploymentKey StDeploymentKey) *SnIncentiveSnapshot {
	key := requireStDeploymentKey(deploymentKey)
	var snapshot *SnIncentiveSnapshot
	server.Db(ctx, func(conn server.PgConn) {
		snapshot = nil
		rows, err := conn.Query(
			ctx,
			`
				SELECT
					subnet_epoch_index, block_number, block_hash, read_at,
					netuid, subnetwork_n, tempo, activity_cutoff,
					reference_uid, reference_last_update, reference_validator_trust,
					fallback_row, stale, stale_reasons,
					validator_rows_complete, validator_rows_json, response_bytes
				FROM sn_incentive_snapshot
				WHERE deployment_key = $1
				ORDER BY subnet_epoch_index DESC
				LIMIT 1
			`,
			key,
		)
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				return
			}
			snapshot = &SnIncentiveSnapshot{}
			var index, block, cutoff, referenceLastUpdate int64
			var netuid, subnetworkN, tempo, referenceTrust int
			var referenceUid *int
			var staleReasons, validatorRows string
			server.Raise(rows.Scan(
				&index, &block, &snapshot.BlockHash, &snapshot.ReadAt,
				&netuid, &subnetworkN, &tempo, &cutoff,
				&referenceUid, &referenceLastUpdate, &referenceTrust,
				&snapshot.FallbackRow, &snapshot.Stale, &staleReasons,
				&snapshot.ValidatorRowsComplete, &validatorRows, &snapshot.ResponseBytes,
			))
			snapshot.SubnetEpochIndex, snapshot.Block, snapshot.ActivityCutoff = uint64(index), uint64(block), uint64(cutoff)
			snapshot.Netuid, snapshot.SubnetworkN, snapshot.Tempo = uint16(netuid), uint16(subnetworkN), uint16(tempo)
			snapshot.ReferenceLastUpdate, snapshot.ReferenceValidatorTrust = uint64(referenceLastUpdate), uint16(referenceTrust)
			if referenceUid != nil {
				uid := uint16(*referenceUid)
				snapshot.ReferenceUid = &uid
			}
			snapshot.StaleReasons = []string{}
			if staleReasons != "" {
				snapshot.StaleReasons = strings.Split(staleReasons, ",")
			}
			server.Raise(json.Unmarshal([]byte(validatorRows), &snapshot.ValidatorRows))
		})
		if snapshot == nil {
			return
		}
		snapshot.Uids = []*SnIncentiveUid{}
		rows, err = conn.Query(
			ctx,
			`
				SELECT uid, incentive, last_update, validator_permit, validator_trust, hotkey, block_at_registration
				FROM sn_incentive_uid
				WHERE deployment_key = $1 AND subnet_epoch_index = $2
				ORDER BY uid
			`,
			key, int64(snapshot.SubnetEpochIndex),
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				row := &SnIncentiveUid{}
				var uid, incentive, trust int
				var lastUpdate int64
				var hotkey []byte
				var registration *int64
				server.Raise(rows.Scan(&uid, &incentive, &lastUpdate, &row.ValidatorPermit, &trust, &hotkey, &registration))
				row.Uid, row.Incentive, row.LastUpdate, row.ValidatorTrust = uint16(uid), uint16(incentive), uint64(lastUpdate), uint16(trust)
				if len(hotkey) == 32 && registration != nil {
					row.HolderRead, row.Hotkey, row.BlockAtRegistration = true, [32]byte(hotkey), uint64(*registration)
				}
				snapshot.Uids = append(snapshot.Uids, row)
			}
		})
	})
	return snapshot
}

// Removes the snapshots read before minReadAt and their UIDs; returns how
// many snapshots it removed.
func RemoveSnIncentiveSnapshotsBefore(ctx context.Context, deploymentKey StDeploymentKey, minReadAt time.Time) int64 {
	key := requireStDeploymentKey(deploymentKey)
	var removed int64
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM sn_incentive_uid
				USING sn_incentive_snapshot
				WHERE
					sn_incentive_uid.deployment_key = $1 AND
					sn_incentive_snapshot.deployment_key = $1 AND
					sn_incentive_uid.subnet_epoch_index = sn_incentive_snapshot.subnet_epoch_index AND
					sn_incentive_snapshot.read_at < $2
			`,
			key, minReadAt.UTC(),
		))
		tag := server.RaisePgResult(tx.Exec(ctx, `DELETE FROM sn_incentive_snapshot WHERE deployment_key = $1 AND read_at < $2`, key, minReadAt.UTC()))
		removed = tag.RowsAffected()
	})
	return removed
}
