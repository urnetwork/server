package model

// The SN Top board's storage (LEADERTOP.md §8, owner decision 2026-10-29):
// the indexed trail hops, one snapshot per tempo with its network and fleet
// rows, each unit's EMA state, and each network's daily history. Schema in
// server/sn_top_schema.go.

import (
	"context"
	"slices"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

const (
	// per-tempo snapshots, and the hops their replica reads (Q11)
	SnTopSnapshotRetention = 7 * 24 * time.Hour
	SnTrailHopRetention    = 7 * 24 * time.Hour
	// daily history days (Q11)
	SnTopHistoryDays = 90
	// snapshots that keep every row; older ones keep the paged top 400
	snTopFullSnapshots = 2
	// trail hop rows a retention pass deletes at most
	snTrailHopRemoveLimit = 50000
)

const (
	snTopUnitNetwork = "network"
	snTopUnitFleet   = "fleet"
)

/**
 * Trail hops
 */

// SnTrailHop is one hop of a complete verify trail (sn_trail_hop).
type SnTrailHop struct {
	TrailId    server.Id
	HopIndex   int
	TempoIndex int64
	HeadWindow string
	ClientId   server.Id
	// the network snapshotted when the hop was assigned; nil when unknown
	NetworkId   *server.Id
	PrefixHash  [32]byte
	Seed        bool
	Complete    bool
	ConfirmTime time.Time
}

// snTrailHopsOfCompleteTrail lists a complete trail's hops in order, the seed
// first. The hop's prefix is the egress-IP-hash the FINAL proof carries.
func snTrailHopsOfCompleteTrail(trail *VerifyTrail, headWindow string, tempoIndexAt func(time.Time) int64) []*SnTrailHop {
	hops := make([]*SnTrailHop, 0, len(trail.Hops))
	for index, hop := range trail.Hops {
		confirmTime := time.UnixMilli(int64(hop.ConfirmedMs)).UTC()
		hops = append(hops, &SnTrailHop{
			TrailId:     trail.TrailId,
			HopIndex:    index,
			TempoIndex:  tempoIndexAt(confirmTime),
			HeadWindow:  headWindow,
			ClientId:    hop.ClientId,
			NetworkId:   hop.NetworkId,
			PrefixHash:  hop.EgressIpHash,
			Seed:        hop.Seed || index == 0,
			Complete:    true,
			ConfirmTime: confirmTime,
		})
	}
	return hops
}

// insertSnTrailHopsInTx writes the hops; a replay writes nothing twice.
func insertSnTrailHopsInTx(ctx context.Context, tx server.PgTx, hops []*SnTrailHop) {
	if len(hops) == 0 {
		return
	}
	trailIds := make([]server.Id, len(hops))
	hopIndexes := make([]int32, len(hops))
	tempoIndexes := make([]int64, len(hops))
	headWindows := make([]string, len(hops))
	clientIds := make([]server.Id, len(hops))
	networkIds := make([]*server.Id, len(hops))
	prefixHashes := make([][]byte, len(hops))
	seeds := make([]bool, len(hops))
	completes := make([]bool, len(hops))
	confirmTimes := make([]time.Time, len(hops))
	for i, hop := range hops {
		trailIds[i] = hop.TrailId
		hopIndexes[i] = int32(hop.HopIndex)
		tempoIndexes[i] = hop.TempoIndex
		headWindows[i] = hop.HeadWindow
		clientIds[i] = hop.ClientId
		networkIds[i] = hop.NetworkId
		prefixHashes[i] = slices.Clone(hop.PrefixHash[:])
		seeds[i] = hop.Seed
		completes[i] = hop.Complete
		confirmTimes[i] = hop.ConfirmTime.UTC()
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO sn_trail_hop (
				trail_id, hop_index, tempo_index, head_window, client_id,
				network_id, prefix_hash, seed, complete, confirm_time
			)
			SELECT * FROM unnest(
				$1::uuid[], $2::smallint[], $3::bigint[], $4::varchar[], $5::uuid[],
				$6::uuid[], $7::bytea[], $8::bool[], $9::bool[], $10::timestamp[]
			)
			ON CONFLICT (trail_id, hop_index) DO NOTHING
		`,
		trailIds, hopIndexes, tempoIndexes, headWindows, clientIds,
		networkIds, prefixHashes, seeds, completes, confirmTimes,
	))
}

// SnTopTrailEvidence is what complete trails showed in one tempo: each
// provider's prefixes on non-seed hops, and its network.
type SnTopTrailEvidence struct {
	Prefixes map[server.Id][][32]byte
	Networks map[server.Id]server.Id
	Hops     int
}

// GetSnTopTrailEvidence reads the non-seed hops of complete trails confirmed
// in [startTime, endTime) through the confirm_time index.
func GetSnTopTrailEvidence(ctx context.Context, startTime time.Time, endTime time.Time) *SnTopTrailEvidence {
	evidence := &SnTopTrailEvidence{
		Prefixes: map[server.Id][][32]byte{},
		Networks: map[server.Id]server.Id{},
	}
	seen := map[server.Id]map[[32]byte]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT client_id, network_id, prefix_hash
				FROM sn_trail_hop
				WHERE $1 <= confirm_time AND confirm_time < $2 AND complete AND NOT seed
			`,
			startTime.UTC(),
			endTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var networkId *server.Id
				var prefixHash []byte
				server.Raise(result.Scan(&clientId, &networkId, &prefixHash))
				if len(prefixHash) != 32 {
					continue
				}
				evidence.Hops += 1
				if networkId != nil {
					evidence.Networks[clientId] = *networkId
				}
				h := [32]byte(prefixHash)
				if seen[clientId] == nil {
					seen[clientId] = map[[32]byte]bool{}
				}
				if !seen[clientId][h] {
					seen[clientId][h] = true
					evidence.Prefixes[clientId] = append(evidence.Prefixes[clientId], h)
				}
			}
		})
	})
	return evidence
}

/**
 * Presence
 */

// SnTopPresence is each provider's presence in a tempo from the per-minute
// reliability rows: its valid blocks over the blocks the rollup has drained.
type SnTopPresence struct {
	Clients           map[server.Id]float64
	ClientNetworks    map[server.Id]server.Id
	ProvidingNetworks map[server.Id]bool
	CoveredBlocks     int64
}

// GetSnTopPresence counts each client's valid reliability blocks in
// [startTime, endTime). It is one index-only range scan of the tempo's blocks
// on client_reliability's covering index (valid, block_number,
// client_address_hash) INCLUDE (network_id, client_id), pruned to the day
// partitions the range touches. Blocks past the rollup's drained mark do not
// count yet, so a lagging rollup does not lower anyone's presence.
func GetSnTopPresence(ctx context.Context, startTime time.Time, endTime time.Time) *SnTopPresence {
	presence := &SnTopPresence{
		Clients:           map[server.Id]float64{},
		ClientNetworks:    map[server.Id]server.Id{},
		ProvidingNetworks: map[server.Id]bool{},
	}
	minBlock := reliabilityBlockNumber(startTime)
	maxBlock := reliabilityBlockNumber(endTime)
	if maxDrained, ok := clientReliabilityMaxDrainedBlock(ctx); ok && maxDrained+1 < maxBlock {
		maxBlock = maxDrained + 1
	}
	presence.CoveredBlocks = max(0, maxBlock-minBlock)
	if presence.CoveredBlocks == 0 {
		return presence
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
					SELECT network_id, client_id, COUNT(DISTINCT block_number)
				FROM client_reliability
				WHERE valid = true AND $1 <= block_number AND block_number < $2
				GROUP BY network_id, client_id
			`,
			minBlock,
			maxBlock,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId, clientId server.Id
				var blocks int64
				server.Raise(result.Scan(&networkId, &clientId, &blocks))
				presence.Clients[clientId] = min(1, float64(blocks)/float64(presence.CoveredBlocks))
				presence.ClientNetworks[clientId] = networkId
				presence.ProvidingNetworks[networkId] = true
			}
		})
	})
	return presence
}

// GetNetworkCreateTimes reads when each network was created.
func GetNetworkCreateTimes(ctx context.Context, networkIds []server.Id) map[server.Id]time.Time {
	createTimes := map[server.Id]time.Time{}
	server.Db(ctx, func(conn server.PgConn) {
		for start := 0; start < len(networkIds); start += 4096 {
			chunk := networkIds[start:min(start+4096, len(networkIds))]
			result, err := conn.Query(
				ctx,
				`SELECT network_id, create_time FROM network WHERE network_id = ANY($1::uuid[])`,
				chunk,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var networkId server.Id
					var createTime time.Time
					server.Raise(result.Scan(&networkId, &createTime))
					createTimes[networkId] = createTime.UTC()
				}
			})
		}
	})
	return createTimes
}

/**
 * EMA state
 */

// GetSnTopEmaState reads every unit's EMA state.
func GetSnTopEmaState(ctx context.Context) (networks map[server.Id]SnTopUnitState, fleets map[SnTopFleetKey]SnTopUnitState) {
	networks = map[server.Id]SnTopUnitState{}
	fleets = map[SnTopFleetKey]SnTopUnitState{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT unit_kind, unit_key, ema, ema_exact, solo_ema, last_tempo_index, last_position, measurable_bits
				FROM sn_top_ema_state
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var kind, key string
				var exact *string
				var measurableBits int32
				state := SnTopUnitState{}
				server.Raise(result.Scan(&kind, &key, &state.Ema, &exact, &state.SoloEma, &state.LastTempoIndex, &state.LastPosition, &measurableBits))
				state.MeasurableBits = uint8(measurableBits)
				switch kind {
				case snTopUnitNetwork:
					networkId, err := server.ParseId(key)
					if err != nil {
						continue
					}
					networks[networkId] = state
				case snTopUnitFleet:
					fleetKey, err := ParseSnTopFleetKey(key)
					if err != nil || exact == nil {
						continue
					}
					value, err := parseSnTopRat(*exact)
					if err != nil || value.Sign() <= 0 {
						glog.Infof("[sntop]dropping fleet %s with a malformed exact EMA\n", key)
						continue
					}
					state.EmaExact = value
					fleets[fleetKey] = state
				}
			}
		})
	})
	return
}

/**
 * Snapshots
 */

// SnTopReplicaCheckpoint identifies the signed measurement whose exact state
// was committed. It is independent of the network estimate's tempo index.
type SnTopReplicaCheckpoint struct {
	ValidatorHotkey [32]byte
	NativeEpoch     uint64
	MeasurementHash string
}

// SnTopSnapshot is one tempo's snapshot header (sn_top_snapshot).
type SnTopSnapshot struct {
	SnapshotId         server.Id
	TempoIndex         int64
	TempoSource        string
	Block              uint64
	TempoStartTime     time.Time
	TempoEndTime       time.Time
	ComputedAt         time.Time
	SettlementEpoch    uint64
	TotalRanked        int
	HeadSeats          int
	ClaimedFleets      int
	OpenSeats          int
	ProjectedCutoff    float64
	ClaimedCutoff      *float64
	BindingOpen        bool
	BindingNote        string
	EligibleProviders  int
	MeasuredProviders  int
	CohortCapacity     int
	PresenceSource     string
	ValidatorSummaryAt *time.Time
	IncentiveAt        *time.Time
	PrecisionAt200     *float64
	ReplicaCheckpoint  *SnTopReplicaCheckpoint
	ReplicaComplete    bool
}

const snTopSnapshotSelect = `
	SELECT
		snapshot_id, tempo_index, tempo_source, block, tempo_start_time, tempo_end_time,
		computed_at, settlement_epoch, total_ranked, head_seats, claimed_fleets, open_seats,
		projected_cutoff, claimed_cutoff, binding_open, binding_note, eligible_providers,
		measured_providers, cohort_capacity, presence_source, validator_summary_at,
		incentive_at, precision_at_200, replica_validator_hotkey, replica_native_epoch, replica_measurement_hash, replica_complete
	FROM sn_top_snapshot
`

func scanSnTopSnapshot(result server.PgResult) *SnTopSnapshot {
	snapshot := &SnTopSnapshot{}
	var block, settlementEpoch int64
	var replicaHotkey []byte
	var replicaEpoch *int64
	var replicaHash *string
	server.Raise(result.Scan(
		&snapshot.SnapshotId, &snapshot.TempoIndex, &snapshot.TempoSource, &block,
		&snapshot.TempoStartTime, &snapshot.TempoEndTime, &snapshot.ComputedAt, &settlementEpoch,
		&snapshot.TotalRanked, &snapshot.HeadSeats, &snapshot.ClaimedFleets, &snapshot.OpenSeats,
		&snapshot.ProjectedCutoff, &snapshot.ClaimedCutoff, &snapshot.BindingOpen, &snapshot.BindingNote,
		&snapshot.EligibleProviders, &snapshot.MeasuredProviders, &snapshot.CohortCapacity,
		&snapshot.PresenceSource, &snapshot.ValidatorSummaryAt, &snapshot.IncentiveAt, &snapshot.PrecisionAt200,
		&replicaHotkey, &replicaEpoch, &replicaHash, &snapshot.ReplicaComplete,
	))
	if len(replicaHotkey) == 32 && replicaEpoch != nil && replicaHash != nil {
		snapshot.ReplicaCheckpoint = &SnTopReplicaCheckpoint{ValidatorHotkey: [32]byte(replicaHotkey), NativeEpoch: uint64(*replicaEpoch), MeasurementHash: *replicaHash}
	}
	snapshot.Block = uint64(block)
	snapshot.SettlementEpoch = uint64(settlementEpoch)
	snapshot.TempoStartTime = snapshot.TempoStartTime.UTC()
	snapshot.TempoEndTime = snapshot.TempoEndTime.UTC()
	snapshot.ComputedAt = snapshot.ComputedAt.UTC()
	return snapshot
}

// GetLatestSnTopSnapshot is nil before the first snapshot.
func GetLatestSnTopSnapshot(ctx context.Context) (snapshot *SnTopSnapshot) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, snTopSnapshotSelect+`ORDER BY computed_at DESC, snapshot_id DESC LIMIT 1`)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				snapshot = scanSnTopSnapshot(result)
			}
		})
	})
	return
}

// GetSnTopSnapshot is nil when the snapshot is unknown or was pruned.
func GetSnTopSnapshot(ctx context.Context, snapshotId server.Id) (snapshot *SnTopSnapshot) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, snTopSnapshotSelect+`WHERE snapshot_id = $1`, snapshotId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				snapshot = scanSnTopSnapshot(result)
			}
		})
	})
	return
}

// WriteSnTopSnapshot stores one tempo's snapshot, its rows and the next EMA
// state in one transaction. It also records the previous UTC day's history
// when this is the day's first snapshot.
func WriteSnTopSnapshot(ctx context.Context, snapshot *SnTopSnapshot, projection *SnTopProjection) {
	server.Tx(ctx, func(tx server.PgTx) {
		var block, settlementEpoch int64 = int64(snapshot.Block), int64(snapshot.SettlementEpoch)
		var replicaHotkey []byte
		var replicaEpoch *int64
		var replicaHash *string
		if checkpoint := snapshot.ReplicaCheckpoint; checkpoint != nil {
			replicaHotkey = checkpoint.ValidatorHotkey[:]
			epoch := int64(checkpoint.NativeEpoch)
			replicaEpoch = &epoch
			replicaHash = &checkpoint.MeasurementHash
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_top_snapshot (
					snapshot_id, tempo_index, tempo_source, block, tempo_start_time, tempo_end_time,
					computed_at, settlement_epoch, total_ranked, head_seats, claimed_fleets, open_seats,
					projected_cutoff, claimed_cutoff, binding_open, binding_note, eligible_providers,
					measured_providers, cohort_capacity, presence_source, validator_summary_at,
					incentive_at, precision_at_200, replica_validator_hotkey, replica_native_epoch, replica_measurement_hash, replica_complete
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)
			`,
			snapshot.SnapshotId, snapshot.TempoIndex, snapshot.TempoSource, block,
			snapshot.TempoStartTime.UTC(), snapshot.TempoEndTime.UTC(), snapshot.ComputedAt.UTC(), settlementEpoch,
			snapshot.TotalRanked, snapshot.HeadSeats, snapshot.ClaimedFleets, snapshot.OpenSeats,
			snapshot.ProjectedCutoff, snapshot.ClaimedCutoff, snapshot.BindingOpen, snapshot.BindingNote,
			snapshot.EligibleProviders, snapshot.MeasuredProviders, snapshot.CohortCapacity,
			snapshot.PresenceSource, snapshot.ValidatorSummaryAt, snapshot.IncentiveAt, snapshot.PrecisionAt200,
			replicaHotkey, replicaEpoch, replicaHash, snapshot.ReplicaComplete,
		))
		insertSnTopNetworksInTx(ctx, tx, snapshot.SnapshotId, projection.Networks)
		insertSnTopFleetsInTx(ctx, tx, snapshot.SnapshotId, projection.Fleets)
		replaceSnTopEmaStateInTx(ctx, tx, snapshot.ComputedAt, projection, false)
		recordSnTopDailyInTx(ctx, tx, snapshot)
	})
}

// WriteSnTopReplica finalizes delayed signed evidence on an existing estimate.
// Network rows, estimate timestamps and network EMA values remain untouched.
func WriteSnTopReplica(ctx context.Context, snapshotId server.Id, checkpoint *SnTopReplicaCheckpoint, summaryAt time.Time, projection *SnTopProjection) {
	server.Tx(ctx, func(tx server.PgTx) {
		var hotkey []byte
		var epoch *int64
		var hash *string
		if checkpoint != nil {
			hotkey = checkpoint.ValidatorHotkey[:]
			value := int64(checkpoint.NativeEpoch)
			epoch, hash = &value, &checkpoint.MeasurementHash
		}
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE sn_top_snapshot SET replica_validator_hotkey = $2,
				replica_native_epoch = $3, replica_measurement_hash = $4,
				validator_summary_at = $5, claimed_cutoff = $6, replica_complete = $7
			WHERE snapshot_id = $1
		`, snapshotId, hotkey, epoch, hash, summaryAt.UTC(), projection.ClaimedCutoff, projection.ReplicaUidScores != nil))
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM sn_top_fleet WHERE snapshot_id = $1`, snapshotId))
		insertSnTopFleetsInTx(ctx, tx, snapshotId, projection.Fleets)
		replaceSnTopEmaStateInTx(ctx, tx, server.NowUtc(), projection, true)
	})
}

func insertSnTopNetworksInTx(ctx context.Context, tx server.PgTx, snapshotId server.Id, rows []*SnTopNetworkProjection) {
	const chunkSize = 5000
	for start := 0; start < len(rows); start += chunkSize {
		chunk := rows[start:min(start+chunkSize, len(rows))]
		n := len(chunk)
		networkIds := make([]server.Id, n)
		positions := make([]int32, n)
		scores := make([]float64, n)
		raws := make([]float64, n)
		solos := make([]float64, n)
		margins := make([]float64, n)
		trends := make([]int32, n)
		prefixes := make([]int32, n)
		sharedPrefixes := make([]int32, n)
		sharedWithUnits := make([]int32, n)
		presences := make([]float64, n)
		labels := make([]string, n)
		measurables := make([]bool, n)
		eligibles := make([]bool, n)
		prompts := make([]bool, n)
		claimeds := make([]bool, n)
		reasons := make([]string, n)
		for i, row := range chunk {
			networkIds[i] = row.NetworkId
			positions[i] = int32(row.Position)
			scores[i] = SnTopFinite(row.ProjectedScore)
			raws[i] = SnTopFinite(row.RawLastTempo)
			solos[i] = SnTopFinite(row.SoloScore)
			margins[i] = SnTopFinite(row.Margin)
			trends[i] = int32(row.Trend)
			prefixes[i] = int32(row.Prefixes)
			sharedPrefixes[i] = int32(row.SharedPrefixes)
			sharedWithUnits[i] = int32(row.SharedWithUnits)
			presences[i] = SnTopFinite(row.Presence)
			labels[i] = row.Label
			measurables[i] = row.Measurable
			eligibles[i] = row.VerifyEligible
			prompts[i] = row.ClaimPrompt
			claimeds[i] = row.Claimed
			reasons[i] = row.NotMeasurableReason
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_top_network (
					snapshot_id, network_id, position, projected_score, raw_last_tempo, solo_score,
					margin, trend, prefixes, shared_prefixes, shared_with_units, presence, label,
					measurable, verify_eligible, claim_prompt, claimed, not_measurable_reason
				)
				SELECT $1, t.* FROM unnest(
					$2::uuid[], $3::int[], $4::float8[], $5::float8[], $6::float8[],
					$7::float8[], $8::int[], $9::int[], $10::int[], $11::int[], $12::float8[], $13::varchar[],
					$14::bool[], $15::bool[], $16::bool[], $17::bool[], $18::varchar[]
				) AS t
			`,
			snapshotId, networkIds, positions, scores, raws, solos,
			margins, trends, prefixes, sharedPrefixes, sharedWithUnits, presences, labels,
			measurables, eligibles, prompts, claimeds, reasons,
		))
	}
}

func insertSnTopFleetsInTx(ctx context.Context, tx server.PgTx, snapshotId server.Id, rows []*SnTopFleetProjection) {
	for _, row := range rows {
		fleet := row.Fleet
		var validatorRank, validatorRankOf *int32
		if fleet.ValidatorRank != nil {
			value := int32(*fleet.ValidatorRank)
			validatorRank = &value
		}
		if fleet.ValidatorRankOf != nil {
			value := int32(*fleet.ValidatorRankOf)
			validatorRankOf = &value
		}
		networkIds := slices.Clone(fleet.NetworkIds)
		if networkIds == nil {
			networkIds = []server.Id{}
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_top_fleet (
					snapshot_id, fleet_key, fleet_id, hotkey, generation, uid, valid_to_epoch,
					network_ids, replica_score, replica_raw, replica_selected, validator_score,
					validator_rank, validator_rank_of, selected, incentive, incentive_stale, replica_available
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
			`,
			snapshotId, fleet.Key.String(), fleet.Key.FleetId[:], fleet.Key.Hotkey[:],
			int64(fleet.Key.Generation), int32(fleet.Key.Uid), int64(fleet.ValidToEpoch),
			networkIds, SnTopFinite(row.ReplicaScore), SnTopFinite(row.ReplicaRaw), row.ReplicaSelected,
			fleet.ValidatorScore, validatorRank, validatorRankOf, fleet.Selected,
			fleet.Incentive, fleet.IncentiveStale, row.ReplicaAvailable,
		))
	}
}

// replaceSnTopEmaStateInTx upserts every unit folded this tempo and deletes
// the units that were not (dropped below the EMA floor, or unbound).
func replaceSnTopEmaStateInTx(ctx context.Context, tx server.PgTx, updateTime time.Time, projection *SnTopProjection, fleetsOnly bool) {
	kinds := []string{}
	keys := []string{}
	emas := []float64{}
	exacts := []*string{}
	solos := []float64{}
	tempos := []int64{}
	positions := []int32{}
	measurableBits := []int32{}
	add := func(kind string, key string, state SnTopUnitState) {
		kinds = append(kinds, kind)
		keys = append(keys, key)
		emas = append(emas, SnTopFinite(state.Ema))
		var exact *string
		if state.EmaExact != nil {
			value := snTopRatString(state.EmaExact)
			exact = &value
		}
		exacts = append(exacts, exact)
		solos = append(solos, SnTopFinite(state.SoloEma))
		tempos = append(tempos, state.LastTempoIndex)
		positions = append(positions, int32(state.LastPosition))
		measurableBits = append(measurableBits, int32(state.MeasurableBits))
	}
	for networkId, state := range projection.NetworkStates {
		add(snTopUnitNetwork, networkId.String(), state)
	}
	for key, state := range projection.FleetStates {
		add(snTopUnitFleet, key.String(), state)
	}
	const chunkSize = 5000
	for start := 0; start < len(keys); start += chunkSize {
		end := min(start+chunkSize, len(keys))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_top_ema_state (
					unit_kind, unit_key, ema, ema_exact, solo_ema, last_tempo_index,
					last_position, measurable_bits, update_time
				)
				SELECT t.*, $9::timestamp FROM unnest(
					$1::varchar[], $2::varchar[], $3::float8[], $4::text[], $5::float8[],
					$6::bigint[], $7::int[], $8::int[]
				) AS t
				ON CONFLICT (unit_kind, unit_key) DO UPDATE SET
					ema = EXCLUDED.ema,
					ema_exact = EXCLUDED.ema_exact,
					solo_ema = EXCLUDED.solo_ema,
					last_tempo_index = EXCLUDED.last_tempo_index,
					last_position = EXCLUDED.last_position,
					measurable_bits = EXCLUDED.measurable_bits,
					update_time = EXCLUDED.update_time
			`,
			kinds[start:end], keys[start:end], emas[start:end], exacts[start:end], solos[start:end],
			tempos[start:end], positions[start:end], measurableBits[start:end], updateTime.UTC(),
		))
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM sn_top_ema_state WHERE update_time < $1 AND (NOT $2 OR unit_kind = $3)`,
		updateTime.UTC(), fleetsOnly, snTopUnitFleet,
	))
}

func snTopDay(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// recordSnTopDailyInTx records a UTC day's ranks and scores from its last
// snapshot, once the first snapshot of the next day is written.
func recordSnTopDailyInTx(ctx context.Context, tx server.PgTx, snapshot *SnTopSnapshot) {
	var previousId *server.Id
	var previousComputedAt time.Time
	result, err := tx.Query(
		ctx,
		`
			SELECT snapshot_id, computed_at FROM sn_top_snapshot
			WHERE computed_at < $1
			ORDER BY computed_at DESC, snapshot_id DESC
			LIMIT 1
		`,
		snapshot.ComputedAt.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var snapshotId server.Id
			server.Raise(result.Scan(&snapshotId, &previousComputedAt))
			previousId = &snapshotId
		}
	})
	if previousId == nil {
		return
	}
	previousDay := snTopDay(previousComputedAt)
	if !previousDay.Before(snTopDay(snapshot.ComputedAt)) {
		return
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO sn_top_network_daily (network_id, day, rank, score)
			SELECT network_id, $2::date, position, projected_score
			FROM sn_top_network
			WHERE snapshot_id = $1 AND 0 < position
			ON CONFLICT (network_id, day) DO NOTHING
		`,
		*previousId,
		previousDay,
	))
}

// RemoveOldSnTopData applies the board's retention: snapshots older than
// seven days, the rows past the paged top 400 of all but the newest
// snapshots, daily history older than 90 days, and trail hops older than
// seven days (a bounded batch per pass).
func RemoveOldSnTopData(ctx context.Context, now time.Time) (removedHops int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		cutoff := now.Add(-SnTopSnapshotRetention).UTC()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM sn_top_network
				USING sn_top_snapshot
				WHERE sn_top_network.snapshot_id = sn_top_snapshot.snapshot_id AND sn_top_snapshot.computed_at < $1
			`,
			cutoff,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM sn_top_fleet
				USING sn_top_snapshot
				WHERE sn_top_fleet.snapshot_id = sn_top_snapshot.snapshot_id AND sn_top_snapshot.computed_at < $1
			`,
			cutoff,
		))
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM sn_top_snapshot WHERE computed_at < $1`, cutoff))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM sn_top_network
				USING (
					SELECT snapshot_id FROM sn_top_snapshot
					ORDER BY computed_at DESC, snapshot_id DESC
					OFFSET $1
				) AS old
				WHERE sn_top_network.snapshot_id = old.snapshot_id AND
					(sn_top_network.position = 0 OR $2 < sn_top_network.position)
			`,
			snTopFullSnapshots,
			SnTopPagedPositions,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM sn_top_network_daily WHERE day < $1::date`,
			snTopDay(now).AddDate(0, 0, -SnTopHistoryDays),
		))
	})
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
				DELETE FROM sn_trail_hop
				WHERE ctid IN (
					SELECT ctid FROM sn_trail_hop WHERE confirm_time < $1 LIMIT $2
				)
			`,
			now.Add(-SnTrailHopRetention).UTC(),
			snTrailHopRemoveLimit,
		)
		server.Raise(err)
		removedHops = tag.RowsAffected()
	})
	return
}

/**
 * Reads
 */

// SnTopNetworkRow is a snapshot row with the network's display settings,
// read at request time so a rename or an opt-in applies at once.
type SnTopNetworkRow struct {
	SnTopNetworkProjection
	NetworkName             string
	EmojiTag                string
	PointsLeaderboardPublic bool
	ContainsProfanity       bool
}

const snTopNetworkRowSelect = `
	SELECT
		sn_top_network.network_id, sn_top_network.position, sn_top_network.projected_score,
		sn_top_network.raw_last_tempo, sn_top_network.solo_score, sn_top_network.margin,
		sn_top_network.trend, sn_top_network.prefixes, sn_top_network.shared_prefixes,
		sn_top_network.shared_with_units, sn_top_network.presence, sn_top_network.label,
		sn_top_network.measurable, sn_top_network.verify_eligible, sn_top_network.claim_prompt,
		sn_top_network.claimed, sn_top_network.not_measurable_reason,
		COALESCE(network.network_name, ''), COALESCE(network.emoji_tag, ''),
		COALESCE(network.points_leaderboard_public, false), COALESCE(network.contains_profanity, false)
	FROM sn_top_network
	LEFT JOIN network ON network.network_id = sn_top_network.network_id
`

func scanSnTopNetworkRow(result server.PgResult) *SnTopNetworkRow {
	row := &SnTopNetworkRow{}
	server.Raise(result.Scan(
		&row.NetworkId, &row.Position, &row.ProjectedScore,
		&row.RawLastTempo, &row.SoloScore, &row.Margin,
		&row.Trend, &row.Prefixes, &row.SharedPrefixes,
		&row.SharedWithUnits, &row.Presence, &row.Label,
		&row.Measurable, &row.VerifyEligible, &row.ClaimPrompt,
		&row.Claimed, &row.NotMeasurableReason,
		&row.NetworkName, &row.EmojiTag, &row.PointsLeaderboardPublic, &row.ContainsProfanity,
	))
	return row
}

const (
	SnTopFilterAll       = "all"
	SnTopFilterUnclaimed = "unclaimed"
	SnTopFilterClaimed   = "claimed"
)

// GetSnTopPage lists a snapshot's rows after `afterPosition` within the paged
// top 400, at most `limit`, through the (snapshot_id, position) index.
func GetSnTopPage(ctx context.Context, snapshotId server.Id, afterPosition int, limit int, filter string) (rows []*SnTopNetworkRow) {
	rows = []*SnTopNetworkRow{}
	if limit <= 0 || SnTopPagedPositions <= afterPosition {
		return
	}
	filterSql := ""
	switch filter {
	case SnTopFilterClaimed:
		filterSql = ` AND sn_top_network.claimed`
	case SnTopFilterUnclaimed:
		filterSql = ` AND NOT sn_top_network.claimed`
	}
	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			snTopNetworkRowSelect+`
				WHERE
					sn_top_network.snapshot_id = $1 AND
					$2 < sn_top_network.position AND
					sn_top_network.position <= $3`+filterSql+`
				ORDER BY sn_top_network.position ASC
				LIMIT $4
			`,
			snapshotId,
			max(0, afterPosition),
			SnTopPagedPositions,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rows = append(rows, scanSnTopNetworkRow(result))
			}
		})
	})
	return
}

// GetSnTopNetworkRow is the network's row in the snapshot, nil when it has
// none (not ranked, or pruned from an old snapshot).
func GetSnTopNetworkRow(ctx context.Context, snapshotId server.Id, networkId server.Id) (row *SnTopNetworkRow) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			snTopNetworkRowSelect+`WHERE sn_top_network.snapshot_id = $1 AND sn_top_network.network_id = $2`,
			snapshotId,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = scanSnTopNetworkRow(result)
			}
		})
	})
	return
}

// SnTopFleetRow is a claimed fleet's row in a snapshot.
type SnTopFleetRow struct {
	Key              SnTopFleetKey
	ValidToEpoch     uint64
	NetworkIds       []server.Id
	ReplicaScore     float64
	ReplicaRaw       float64
	ReplicaSelected  bool
	ReplicaAvailable bool
	ValidatorScore   *float64
	ValidatorRank    *int
	ValidatorRankOf  *int
	Selected         *bool
	Incentive        *float64
	IncentiveStale   bool
}

// GetSnTopSnapshotFleetInputs preserves a snapshot's membership when delayed
// replica data is finalized. It does not substitute today's binding census.
func GetSnTopSnapshotFleetInputs(ctx context.Context, snapshotId server.Id) (fleets []SnTopFleetInput) {
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT fleet_key, valid_to_epoch, network_ids::text[] FROM sn_top_fleet WHERE snapshot_id = $1 ORDER BY fleet_key`, snapshotId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var key string
				var epoch int64
				var networks []string
				server.Raise(rows.Scan(&key, &epoch, &networks))
				parsed, err := ParseSnTopFleetKey(key)
				server.Raise(err)
				fleet := SnTopFleetInput{Key: parsed, ValidToEpoch: uint64(epoch)}
				for _, network := range networks {
					networkId, err := server.ParseId(network)
					server.Raise(err)
					fleet.NetworkIds = append(fleet.NetworkIds, networkId)
				}
				fleets = append(fleets, fleet)
			}
		})
	})
	return
}

// GetSnTopFleets lists a snapshot's fleets of the given networks, by
// network. The set is small: at most the claimed head.
func GetSnTopFleets(ctx context.Context, snapshotId server.Id, networkIds []server.Id) map[server.Id][]*SnTopFleetRow {
	fleets := map[server.Id][]*SnTopFleetRow{}
	if len(networkIds) == 0 {
		return fleets
	}
	wanted := map[server.Id]bool{}
	for _, networkId := range networkIds {
		wanted[networkId] = true
	}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT fleet_key, valid_to_epoch, network_ids::text[], replica_score, replica_raw,
					replica_selected, validator_score, validator_rank, validator_rank_of, selected,
					incentive, incentive_stale, replica_available
				FROM sn_top_fleet
				WHERE snapshot_id = $1 AND network_ids && $2::uuid[]
				ORDER BY uid, fleet_key
			`,
			snapshotId,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &SnTopFleetRow{}
				var fleetKey string
				var validToEpoch int64
				var validatorRank, validatorRankOf *int32
				var networkIds []string
				server.Raise(result.Scan(
					&fleetKey, &validToEpoch, &networkIds, &row.ReplicaScore, &row.ReplicaRaw,
					&row.ReplicaSelected, &row.ValidatorScore, &validatorRank, &validatorRankOf, &row.Selected,
					&row.Incentive, &row.IncentiveStale, &row.ReplicaAvailable,
				))
				key, err := ParseSnTopFleetKey(fleetKey)
				if err != nil {
					continue
				}
				row.Key = key
				row.ValidToEpoch = uint64(validToEpoch)
				for _, value := range networkIds {
					if networkId, err := server.ParseId(value); err == nil {
						row.NetworkIds = append(row.NetworkIds, networkId)
					}
				}
				if validatorRank != nil {
					value := int(*validatorRank)
					row.ValidatorRank = &value
				}
				if validatorRankOf != nil {
					value := int(*validatorRankOf)
					row.ValidatorRankOf = &value
				}
				for _, networkId := range row.NetworkIds {
					if wanted[networkId] {
						fleets[networkId] = append(fleets[networkId], row)
					}
				}
			}
		})
	})
	return fleets
}

// SnTopHistoryPoint is one day's rank and score.
type SnTopHistoryPoint struct {
	Day   time.Time
	Rank  int
	Score float64
}

// GetSnTopNetworkHistory lists the network's daily history since `since`,
// oldest first.
func GetSnTopNetworkHistory(ctx context.Context, networkId server.Id, since time.Time) (history []SnTopHistoryPoint) {
	history = []SnTopHistoryPoint{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT day, rank, score FROM sn_top_network_daily
				WHERE network_id = $1 AND $2::date <= day
				ORDER BY day ASC
			`,
			networkId,
			snTopDay(since),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				point := SnTopHistoryPoint{}
				server.Raise(result.Scan(&point.Day, &point.Rank, &point.Score))
				point.Day = snTopDay(point.Day)
				history = append(history, point)
			}
		})
	})
	return
}
