package model

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/geo/solve"
)

// Pings as their pinger reports them (connect/GEOMAP.md §2.5, §2.6): one row
// per attested probe of a target extender, by a provider or by another
// extender, whatever became of it at the target.
//
// A row is written only after the operator verified the pinger's signature
// under the key it holds for the pinger, checked that the pinger is the
// reporting client, and recomputed the target's verdict under the target's
// stored key. So `Cosign` is the operator's word, not the pinger's: a
// co-signed row carries a second check by the party that saw the round trip,
// and it is the only kind of row that is a measurement (D8). A refused or
// unknown row is the pinger's claim that the target refused or never
// answered; it is evidence only in aggregate (§5.5).
//
// A replayed report is a no-op rather than a second sample. The unique key is
// the target, the pinger, the nonce and the create time -- a partitioned
// table's key must carry its partition column -- so the key alone stops only
// a copy stored at the same instant, and the ingest looks the target, pinger
// and nonce up across the replay window before it stores
// (AddReportedNetworkPings). Rows live a day (§5.7), in day partitions the
// sweep drops whole (the partitions, below), and every store adds to the
// tallies the dashboard and the monitor count from (the tallies, below).
//
// A ping relayed through an NLayer chain (§2.9) is stored with its depth. It
// counts everywhere a ping counts -- the dashboard, the refusal evidence --
// except as a solver term: its round trip includes the detour through the
// front, so it overstates the distance to the chain end it names.

// The kind of node that pinged. Stored values.
const (
	NetworkPingPingerKindProvider = 1
	NetworkPingPingerKindExtender = 2
)

// The operator's verdict on a ping. Stored values.
const (
	// no verdict arrived at the pinger: a close, a timeout, or a target that
	// predates the verdict
	NetworkPingCosignUnknown = 0
	// the target accepted, and its co-signature verifies under its stored key
	NetworkPingCosignCosigned = 1
	// the target refused, by the pinger's account, or accepted with a
	// co-signature that does not verify
	NetworkPingCosignRejected = 2
)

// One stored ping: what the ingest verified, and what the reads return.
type NetworkPing struct {
	PingId     server.Id
	PingerKind int
	// the provider's client id, or the pinging extender's extender id
	PingerId         server.Id
	TargetExtenderId server.Id
	ProbeNonce       []byte
	RttMs            int
	// the pinger's own timestamp on the claim
	ProbeTime time.Time
	Cosign    int
	// the target's refusal reason (connect ExtenderProbeVerdictReason*), 0
	// unless refused
	CosignReason    int
	PingerSignature []byte
	// the target's co-signature, nil unless co-signed
	Cosignature []byte
	// the NLayer relays the probe crossed to reach its target, 0 for a direct
	// ping; the pinger's word, outside its signature
	HopCount   int
	CreateTime time.Time
}

// Stores verified pings and reports how many were new. A ping whose target,
// pinger, nonce and create time are already stored -- the unique key -- is
// skipped, which catches a copy stored at the same instant only; the ingest
// stores through AddReportedNetworkPings, which also catches a copy stored at
// another time.
func AddNetworkPings(ctx context.Context, pings []*NetworkPing) (inserted int) {
	return addNetworkPings(ctx, pings, nil)
}

// The ingest's store (GEOMAP §2.5): stores verified pings less every replay
// -- a ping whose target, pinger and nonce are already stored at or after
// `replayMinCreateTime`, or that repeats an earlier ping of the same batch --
// and reports how many were new, so the caller counts the rest as rejected.
//
// The unique key carries the create time, so without this lookup a nonce
// stored again at another time -- a retried post, or a replay on a later day
// into a later partition -- would be a second row. The lookup reads the key's
// prefix in every partition from `replayMinCreateTime` on. It runs in the
// insert's transaction, at read committed and under a lock per pinger, so two
// posts of one report at once cannot both miss each other: the second waits
// for the first to commit, and its lookup then sees the first's rows.
func AddReportedNetworkPings(
	ctx context.Context,
	pings []*NetworkPing,
	replayMinCreateTime time.Time,
) (inserted int) {
	return addNetworkPings(ctx, pings, &replayMinCreateTime)
}

// The advisory lock a replay lookup holds on one pinger until its transaction
// ends. A pinger's pings are only ever reported by the one client that owns
// it, so the lock serializes nothing but that client's own posts.
func networkPingReplayLockKey(pingerKind int, pingerId server.Id) string {
	return fmt.Sprintf("network-ping-replay-v1:%d:%s", pingerKind, pingerId)
}

// An insert found no partition for a stored row's day in `table`: network_ping,
// or one of the tallies kept by day. It leaves the store as this error rather
// than the database's check violation, which the transaction counts as
// transient and would retry for a minute before the store could create the
// day.
type networkPingPartitionMissingError struct {
	table string
}

// Implements error.
func (self *networkPingPartitionMissingError) Error() string {
	return fmt.Sprintf("no %s partition holds a stored row's day", self.table)
}

// Stores `pings`, less the replays when `replayMinCreateTime` is set
// (AddReportedNetworkPings), and adds what it stored to the tallies in the
// same transaction, so that they are exact (the tallies, below). An insert
// that finds no partition for its day -- the sweep further behind than its
// days ahead, or a row older than the days the sweep keeps -- creates the
// day's partition of every table kept by day and stores once more, so an
// insert never fails for want of a partition (GEOMAP §5.7).
func addNetworkPings(
	ctx context.Context,
	pings []*NetworkPing,
	replayMinCreateTime *time.Time,
) (inserted int) {
	if len(pings) == 0 {
		return 0
	}
	tallySettings := DefaultNetworkPingTallySettings()
	// ids and times are fixed once, so a store after a missing partition
	// writes the same rows
	now := server.NowUtc()
	resolvedPings := make([]*NetworkPing, 0, len(pings))
	for _, ping := range pings {
		resolvedPing := *ping
		if resolvedPing.PingId == (server.Id{}) {
			resolvedPing.PingId = server.NewId()
		}
		if resolvedPing.CreateTime.IsZero() {
			resolvedPing.CreateTime = now
		}
		resolvedPing.CreateTime = resolvedPing.CreateTime.UTC()
		if len(resolvedPing.Cosignature) == 0 {
			resolvedPing.Cosignature = nil
		}
		resolvedPings = append(resolvedPings, &resolvedPing)
	}

	// raises a missing day partition as its own error, since the transaction
	// would retry the database's check violation as transient
	raiseStoreErr := func(err error) {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == pgerrcode.CheckViolation &&
			slices.Contains(networkPingPartitionedTables, pgErr.TableName) {
			panic(&networkPingPartitionMissingError{table: pgErr.TableName})
		}
		server.Raise(err)
	}

	// Adds the stored pings to the four tallies: the fleet's hour tally, the
	// per-target hour tally, and the day's distinct pingers and targets. Each
	// upsert takes its rows in key order and the tables in a fixed order, so
	// two stores that touch the same rows lock them in the same order and
	// cannot deadlock.
	tally := func(tx server.PgTx, storedPings []*NetworkPing) {
		// a row of the fleet's hour tally
		type hourTallyKey struct {
			hour         time.Time
			shard        int
			pingerKind   int
			relayed      bool
			cosign       int
			cosignReason int
		}
		// the counts of a row of the fleet's hour tally
		type hourTallyCounts struct {
			pingCount             int64
			zeroRttCount          int64
			beyondHalfPlanetCount int64
		}
		// a row of the per-target hour tally
		type targetHourTallyKey struct {
			hour             time.Time
			targetExtenderId server.Id
			pingerKind       int
		}
		// the counts of a row of the per-target hour tally
		type targetHourTallyCounts struct {
			pingCount      int64
			rejectionCount int64
		}
		// a pinger of a day
		type pingerDayKey struct {
			day        time.Time
			pingerKind int
			pingerId   server.Id
		}
		// a target of a day
		type targetDayKey struct {
			day              time.Time
			targetExtenderId server.Id
		}
		hourTallyKeyCounts := map[hourTallyKey]*hourTallyCounts{}
		targetHourTallyKeyCounts := map[targetHourTallyKey]*targetHourTallyCounts{}
		pingerDayKeys := map[pingerDayKey]bool{}
		targetDayKeys := map[targetDayKey]bool{}
		for _, storedPing := range storedPings {
			hour := storedPing.CreateTime.UTC().Truncate(time.Hour)
			day := networkPingPartitionDay(storedPing.CreateTime)
			hourKey := hourTallyKey{
				hour:         hour,
				shard:        int(storedPing.PingerId[len(storedPing.PingerId)-1]) % tallySettings.HourShardCount,
				pingerKind:   storedPing.PingerKind,
				relayed:      0 < storedPing.HopCount,
				cosign:       storedPing.Cosign,
				cosignReason: storedPing.CosignReason,
			}
			hourCounts := hourTallyKeyCounts[hourKey]
			if hourCounts == nil {
				hourCounts = &hourTallyCounts{}
				hourTallyKeyCounts[hourKey] = hourCounts
			}
			hourCounts.pingCount += 1
			if storedPing.RttMs == 0 {
				hourCounts.zeroRttCount += 1
			}
			if tallySettings.HalfPlanetRttMs < storedPing.RttMs {
				hourCounts.beyondHalfPlanetCount += 1
			}

			targetHourKey := targetHourTallyKey{
				hour:             hour,
				targetExtenderId: storedPing.TargetExtenderId,
				pingerKind:       storedPing.PingerKind,
			}
			targetHourCounts := targetHourTallyKeyCounts[targetHourKey]
			if targetHourCounts == nil {
				targetHourCounts = &targetHourTallyCounts{}
				targetHourTallyKeyCounts[targetHourKey] = targetHourCounts
			}
			targetHourCounts.pingCount += 1
			if storedPing.Cosign == NetworkPingCosignRejected {
				targetHourCounts.rejectionCount += 1
			}

			pingerDayKeys[pingerDayKey{
				day:        day,
				pingerKind: storedPing.PingerKind,
				pingerId:   storedPing.PingerId,
			}] = true
			targetDayKeys[targetDayKey{
				day:              day,
				targetExtenderId: storedPing.TargetExtenderId,
			}] = true
		}
		// orders the relayed flag within a key
		boolOrder := func(value bool) int {
			if value {
				return 1
			}
			return 0
		}

		hourKeys := slices.SortedFunc(maps.Keys(hourTallyKeyCounts), func(a hourTallyKey, b hourTallyKey) int {
			return cmp.Or(
				a.hour.Compare(b.hour),
				cmp.Compare(a.shard, b.shard),
				cmp.Compare(a.pingerKind, b.pingerKind),
				cmp.Compare(boolOrder(a.relayed), boolOrder(b.relayed)),
				cmp.Compare(a.cosign, b.cosign),
				cmp.Compare(a.cosignReason, b.cosignReason),
			)
		})
		hours := make([]time.Time, 0, len(hourKeys))
		shards := make([]int16, 0, len(hourKeys))
		pingerKinds := make([]int16, 0, len(hourKeys))
		relayedFlags := make([]bool, 0, len(hourKeys))
		cosigns := make([]int16, 0, len(hourKeys))
		cosignReasons := make([]int16, 0, len(hourKeys))
		pingCounts := make([]int64, 0, len(hourKeys))
		zeroRttCounts := make([]int64, 0, len(hourKeys))
		beyondHalfPlanetCounts := make([]int64, 0, len(hourKeys))
		for _, hourKey := range hourKeys {
			hourCounts := hourTallyKeyCounts[hourKey]
			hours = append(hours, hourKey.hour)
			shards = append(shards, int16(hourKey.shard))
			pingerKinds = append(pingerKinds, int16(hourKey.pingerKind))
			relayedFlags = append(relayedFlags, hourKey.relayed)
			cosigns = append(cosigns, int16(hourKey.cosign))
			cosignReasons = append(cosignReasons, int16(hourKey.cosignReason))
			pingCounts = append(pingCounts, hourCounts.pingCount)
			zeroRttCounts = append(zeroRttCounts, hourCounts.zeroRttCount)
			beyondHalfPlanetCounts = append(beyondHalfPlanetCounts, hourCounts.beyondHalfPlanetCount)
		}
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO network_ping_hour_tally (
				hour,
				shard,
				pinger_kind,
				relayed,
				cosign,
				cosign_reason,
				ping_count,
				zero_rtt_count,
				beyond_half_planet_count
			)
			SELECT * FROM unnest(
				$1::timestamp[],
				$2::smallint[],
				$3::smallint[],
				$4::boolean[],
				$5::smallint[],
				$6::smallint[],
				$7::bigint[],
				$8::bigint[],
				$9::bigint[]
			)
			ON CONFLICT (hour, shard, pinger_kind, relayed, cosign, cosign_reason) DO UPDATE SET
				ping_count = network_ping_hour_tally.ping_count + excluded.ping_count,
				zero_rtt_count = network_ping_hour_tally.zero_rtt_count + excluded.zero_rtt_count,
				beyond_half_planet_count = network_ping_hour_tally.beyond_half_planet_count + excluded.beyond_half_planet_count
			`,
			hours,
			shards,
			pingerKinds,
			relayedFlags,
			cosigns,
			cosignReasons,
			pingCounts,
			zeroRttCounts,
			beyondHalfPlanetCounts,
		)
		raiseStoreErr(err)

		targetHourKeys := slices.SortedFunc(maps.Keys(targetHourTallyKeyCounts), func(a targetHourTallyKey, b targetHourTallyKey) int {
			return cmp.Or(
				a.hour.Compare(b.hour),
				bytes.Compare(a.targetExtenderId[:], b.targetExtenderId[:]),
				cmp.Compare(a.pingerKind, b.pingerKind),
			)
		})
		targetHours := make([]time.Time, 0, len(targetHourKeys))
		targetExtenderIds := make([]server.Id, 0, len(targetHourKeys))
		targetPingerKinds := make([]int16, 0, len(targetHourKeys))
		targetPingCounts := make([]int64, 0, len(targetHourKeys))
		targetRejectionCounts := make([]int64, 0, len(targetHourKeys))
		for _, targetHourKey := range targetHourKeys {
			targetHourCounts := targetHourTallyKeyCounts[targetHourKey]
			targetHours = append(targetHours, targetHourKey.hour)
			targetExtenderIds = append(targetExtenderIds, targetHourKey.targetExtenderId)
			targetPingerKinds = append(targetPingerKinds, int16(targetHourKey.pingerKind))
			targetPingCounts = append(targetPingCounts, targetHourCounts.pingCount)
			targetRejectionCounts = append(targetRejectionCounts, targetHourCounts.rejectionCount)
		}
		_, err = tx.Exec(
			ctx,
			`
			INSERT INTO network_ping_target_hour_tally (
				hour,
				target_extender_id,
				pinger_kind,
				ping_count,
				rejection_count
			)
			SELECT * FROM unnest(
				$1::timestamp[],
				$2::uuid[],
				$3::smallint[],
				$4::bigint[],
				$5::bigint[]
			)
			ON CONFLICT (hour, target_extender_id, pinger_kind) DO UPDATE SET
				ping_count = network_ping_target_hour_tally.ping_count + excluded.ping_count,
				rejection_count = network_ping_target_hour_tally.rejection_count + excluded.rejection_count
			`,
			targetHours,
			targetExtenderIds,
			targetPingerKinds,
			targetPingCounts,
			targetRejectionCounts,
		)
		raiseStoreErr(err)

		pingerDays := slices.SortedFunc(maps.Keys(pingerDayKeys), func(a pingerDayKey, b pingerDayKey) int {
			return cmp.Or(
				a.day.Compare(b.day),
				cmp.Compare(a.pingerKind, b.pingerKind),
				bytes.Compare(a.pingerId[:], b.pingerId[:]),
			)
		})
		pingerDayDays := make([]time.Time, 0, len(pingerDays))
		pingerDayKinds := make([]int16, 0, len(pingerDays))
		pingerDayIds := make([]server.Id, 0, len(pingerDays))
		for _, pingerDay := range pingerDays {
			pingerDayDays = append(pingerDayDays, pingerDay.day)
			pingerDayKinds = append(pingerDayKinds, int16(pingerDay.pingerKind))
			pingerDayIds = append(pingerDayIds, pingerDay.pingerId)
		}
		_, err = tx.Exec(
			ctx,
			`
			INSERT INTO network_ping_pinger_day (day, pinger_kind, pinger_id)
			SELECT * FROM unnest($1::date[], $2::smallint[], $3::uuid[])
			ON CONFLICT DO NOTHING
			`,
			pingerDayDays,
			pingerDayKinds,
			pingerDayIds,
		)
		raiseStoreErr(err)

		targetDays := slices.SortedFunc(maps.Keys(targetDayKeys), func(a targetDayKey, b targetDayKey) int {
			return cmp.Or(
				a.day.Compare(b.day),
				bytes.Compare(a.targetExtenderId[:], b.targetExtenderId[:]),
			)
		})
		targetDayDays := make([]time.Time, 0, len(targetDays))
		targetDayIds := make([]server.Id, 0, len(targetDays))
		for _, targetDay := range targetDays {
			targetDayDays = append(targetDayDays, targetDay.day)
			targetDayIds = append(targetDayIds, targetDay.targetExtenderId)
		}
		_, err = tx.Exec(
			ctx,
			`
			INSERT INTO network_ping_target_day (day, target_extender_id)
			SELECT * FROM unnest($1::date[], $2::uuid[])
			ON CONFLICT DO NOTHING
			`,
			targetDayDays,
			targetDayIds,
		)
		raiseStoreErr(err)
	}

	store := func() (stored int) {
		server.Tx(ctx, func(tx server.PgTx) {
			stored = 0
			// indexes into resolvedPings of the pings that are replays
			replayIndexes := map[int]bool{}
			if replayMinCreateTime != nil {
				// one lock per pinger, taken in a fixed order so that two
				// posts naming the same pingers cannot deadlock
				pingerLockKeys := []string{}
				for _, resolvedPing := range resolvedPings {
					pingerLockKey := networkPingReplayLockKey(resolvedPing.PingerKind, resolvedPing.PingerId)
					if !slices.Contains(pingerLockKeys, pingerLockKey) {
						pingerLockKeys = append(pingerLockKeys, pingerLockKey)
					}
				}
				slices.Sort(pingerLockKeys)
				for _, pingerLockKey := range pingerLockKeys {
					server.RaisePgResult(tx.Exec(
						ctx,
						`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
						pingerLockKey,
					))
				}

				targetExtenderIds := make([]server.Id, 0, len(resolvedPings))
				pingerKinds := make([]int16, 0, len(resolvedPings))
				pingerIds := make([]server.Id, 0, len(resolvedPings))
				probeNonces := make([][]byte, 0, len(resolvedPings))
				for _, resolvedPing := range resolvedPings {
					targetExtenderIds = append(targetExtenderIds, resolvedPing.TargetExtenderId)
					pingerKinds = append(pingerKinds, int16(resolvedPing.PingerKind))
					pingerIds = append(pingerIds, resolvedPing.PingerId)
					probeNonces = append(probeNonces, resolvedPing.ProbeNonce)
				}
				result, err := tx.Query(
					ctx,
					`
					SELECT reported.ordinal
					FROM unnest($1::uuid[], $2::smallint[], $3::uuid[], $4::bytea[])
						WITH ORDINALITY AS reported(target_extender_id, pinger_kind, pinger_id, probe_nonce, ordinal)
					WHERE EXISTS (
						SELECT 1
						FROM network_ping
						WHERE
							network_ping.target_extender_id = reported.target_extender_id AND
							network_ping.pinger_kind = reported.pinger_kind AND
							network_ping.pinger_id = reported.pinger_id AND
							network_ping.probe_nonce = reported.probe_nonce AND
							$5 <= network_ping.create_time
					)
					`,
					targetExtenderIds,
					pingerKinds,
					pingerIds,
					probeNonces,
					replayMinCreateTime.UTC(),
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var ordinal int64
						server.Raise(result.Scan(&ordinal))
						replayIndexes[int(ordinal)-1] = true
					}
				})

				// a ping the batch already carried is a replay of the first
				batchKeys := map[string]bool{}
				for i, resolvedPing := range resolvedPings {
					batchKey := fmt.Sprintf(
						"%s:%d:%s:%x",
						resolvedPing.TargetExtenderId,
						resolvedPing.PingerKind,
						resolvedPing.PingerId,
						resolvedPing.ProbeNonce,
					)
					if batchKeys[batchKey] {
						replayIndexes[i] = true
					}
					batchKeys[batchKey] = true
				}
			}

			storePings := []*NetworkPing{}
			for i, resolvedPing := range resolvedPings {
				if !replayIndexes[i] {
					storePings = append(storePings, resolvedPing)
				}
			}
			if len(storePings) == 0 {
				return
			}
			pingIds := make([]server.Id, 0, len(storePings))
			pingerKinds := make([]int16, 0, len(storePings))
			pingerIds := make([]server.Id, 0, len(storePings))
			targetExtenderIds := make([]server.Id, 0, len(storePings))
			probeNonces := make([][]byte, 0, len(storePings))
			rttMss := make([]int32, 0, len(storePings))
			probeTimes := make([]time.Time, 0, len(storePings))
			cosigns := make([]int16, 0, len(storePings))
			cosignReasons := make([]int16, 0, len(storePings))
			pingerSignatures := make([][]byte, 0, len(storePings))
			cosignatures := make([][]byte, 0, len(storePings))
			hopCounts := make([]int16, 0, len(storePings))
			createTimes := make([]time.Time, 0, len(storePings))
			for _, storePing := range storePings {
				pingIds = append(pingIds, storePing.PingId)
				pingerKinds = append(pingerKinds, int16(storePing.PingerKind))
				pingerIds = append(pingerIds, storePing.PingerId)
				targetExtenderIds = append(targetExtenderIds, storePing.TargetExtenderId)
				probeNonces = append(probeNonces, storePing.ProbeNonce)
				rttMss = append(rttMss, int32(storePing.RttMs))
				probeTimes = append(probeTimes, storePing.ProbeTime.UTC())
				cosigns = append(cosigns, int16(storePing.Cosign))
				cosignReasons = append(cosignReasons, int16(storePing.CosignReason))
				pingerSignatures = append(pingerSignatures, storePing.PingerSignature)
				cosignatures = append(cosignatures, storePing.Cosignature)
				hopCounts = append(hopCounts, int16(storePing.HopCount))
				createTimes = append(createTimes, storePing.CreateTime)
			}
			// one statement for the batch; a copy of a stored row at the same
			// instant is skipped, so only what was stored comes back to be
			// tallied
			result, err := tx.Query(
				ctx,
				`
				INSERT INTO network_ping (
					ping_id,
					pinger_kind,
					pinger_id,
					target_extender_id,
					probe_nonce,
					rtt_ms,
					probe_time,
					cosign,
					cosign_reason,
					pinger_signature,
					cosignature,
					hop_count,
					create_time
				)
				SELECT * FROM unnest(
					$1::uuid[],
					$2::smallint[],
					$3::uuid[],
					$4::uuid[],
					$5::bytea[],
					$6::integer[],
					$7::timestamp[],
					$8::smallint[],
					$9::smallint[],
					$10::bytea[],
					$11::bytea[],
					$12::smallint[],
					$13::timestamp[]
				)
				ON CONFLICT DO NOTHING
				RETURNING pinger_kind, pinger_id, target_extender_id, rtt_ms, cosign, cosign_reason, hop_count, create_time
				`,
				pingIds,
				pingerKinds,
				pingerIds,
				targetExtenderIds,
				probeNonces,
				rttMss,
				probeTimes,
				cosigns,
				cosignReasons,
				pingerSignatures,
				cosignatures,
				hopCounts,
				createTimes,
			)
			if err != nil {
				raiseStoreErr(err)
			}
			storedPings := []*NetworkPing{}
			func() {
				defer result.Close()
				for result.Next() {
					storedPing := &NetworkPing{}
					server.Raise(result.Scan(
						&storedPing.PingerKind,
						&storedPing.PingerId,
						&storedPing.TargetExtenderId,
						&storedPing.RttMs,
						&storedPing.Cosign,
						&storedPing.CosignReason,
						&storedPing.HopCount,
						&storedPing.CreateTime,
					))
					storedPings = append(storedPings, storedPing)
				}
				if err := result.Err(); err != nil {
					raiseStoreErr(err)
				}
			}()
			if 0 < len(storedPings) {
				tally(tx, storedPings)
			}
			stored = len(storedPings)
		}, pgx.ReadCommitted)
		return stored
	}

	partitionMissing := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				// the one failure handled here: no partition holds a row's day
				var partitionMissingErr *networkPingPartitionMissingError
				if err, ok := r.(error); ok && errors.As(err, &partitionMissingErr) {
					partitionMissing = true
					return
				}
				panic(r)
			}
		}()
		inserted = store()
	}()
	if !partitionMissing {
		return inserted
	}
	lockTimeout := DefaultNetworkPingPartitionSettings().LockTimeout
	createdDays := map[time.Time]bool{}
	for _, resolvedPing := range resolvedPings {
		day := networkPingPartitionDay(resolvedPing.CreateTime)
		if !createdDays[day] {
			for _, table := range networkPingPartitionedTables {
				createNetworkPingTablePartition(ctx, table, day, lockTimeout)
			}
			createdDays[day] = true
		}
	}
	return store()
}

// Reads the pings of one target created at or after `minCreateTime`, oldest
// first, every kind and verdict.
func GetNetworkPings(
	ctx context.Context,
	targetExtenderId server.Id,
	minCreateTime time.Time,
) []*NetworkPing {
	pings := []*NetworkPing{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				ping_id,
				pinger_kind,
				pinger_id,
				probe_nonce,
				rtt_ms,
				probe_time,
				cosign,
				cosign_reason,
				pinger_signature,
				cosignature,
				hop_count,
				create_time
			FROM network_ping
			WHERE target_extender_id = $1 AND $2 <= create_time
			ORDER BY create_time ASC, ping_id ASC
			`,
			targetExtenderId,
			minCreateTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				ping := &NetworkPing{TargetExtenderId: targetExtenderId}
				server.Raise(result.Scan(
					&ping.PingId,
					&ping.PingerKind,
					&ping.PingerId,
					&ping.ProbeNonce,
					&ping.RttMs,
					&ping.ProbeTime,
					&ping.Cosign,
					&ping.CosignReason,
					&ping.PingerSignature,
					&ping.Cosignature,
					&ping.HopCount,
					&ping.CreateTime,
				))
				pings = append(pings, ping)
			}
		})
	})
	return pings
}

// One co-signed ping as the solver reads it (GEOMAP §5.1): who measured whom,
// and the round trip.
type NetworkPingTerm struct {
	PingerKind int
	// the provider's client id, or the pinging extender's extender id
	PingerId         server.Id
	TargetExtenderId server.Id
	RttMs            int
	ProbeTime        time.Time
}

// Streams every direct co-signed ping created at or after `minCreateTime` to
// `callback`, one fresh value per row, in no particular order. Only a co-signed ping is a measurement (D8), and only a direct one
// measures the path to the extender it names: a relayed round trip (§2.9)
// adds the detour through the front, so it is never a term. The refused and
// unknown ones are read by GetNetworkPingRefusals.
//
// The rows are streamed rather than returned because the window is the whole
// ping graph of a day. The callback runs while the query is open, so it must
// not use the database itself.
func GetNetworkPingTerms(
	ctx context.Context,
	minCreateTime time.Time,
	callback func(*NetworkPingTerm),
) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				pinger_kind,
				pinger_id,
				target_extender_id,
				rtt_ms,
				probe_time
			FROM network_ping
			WHERE $1 <= create_time AND cosign = $2 AND hop_count = 0
			`,
			minCreateTime.UTC(),
			NetworkPingCosignCosigned,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				term := &NetworkPingTerm{}
				server.Raise(result.Scan(
					&term.PingerKind,
					&term.PingerId,
					&term.TargetExtenderId,
					&term.RttMs,
					&term.ProbeTime,
				))
				callback(term)
			}
		})
	})
}

// A contiguous range of the pinger-id hash space, both ends included. The
// derive phase reads the day's co-signed pings over several concurrent
// cursors, one range each (GEOMAP §5.3, "Scale"): the hash of the pinger id
// spreads the ids evenly, where the ids themselves lead with their creation
// time, and it keeps every ordered pair inside one range, so each cursor can
// aggregate its pairs alone and the ranges merge by union.
type NetworkPingHashRange struct {
	Lo int64
	Hi int64
}

// Splits the whole int64 hash space into `count` contiguous ranges of equal
// width, the last taking the remainder, so every hash falls in exactly one.
func NetworkPingHashRanges(count int) []NetworkPingHashRange {
	count = max(1, count)
	width := ^uint64(0) / uint64(count)
	ranges := make([]NetworkPingHashRange, 0, count)
	// ranges are cut on the unsigned space and mapped onto int64 in order:
	// flipping the top bit sends 0 to the least int64 and the greatest
	// uint64 to the greatest
	toInt64 := func(value uint64) int64 {
		return int64(value ^ (1 << 63))
	}
	for i := 0; i < count; i += 1 {
		lo := uint64(i) * width
		hi := lo + width - 1
		if i == count-1 {
			hi = ^uint64(0)
		}
		ranges = append(ranges, NetworkPingHashRange{Lo: toInt64(lo), Hi: toInt64(hi)})
	}
	return ranges
}

// The pings of GetNetworkPingTerms over one pinger-id hash range, in
// (create_time, ping_id) order: every pair's samples then arrive in
// the same order whichever range split read them, which is what keeps an
// order-dependent aggregate identical for any number of cursors. The callback
// runs while the query is open, so it must not use the database itself.
func GetNetworkPingTermRange(
	ctx context.Context,
	minCreateTime time.Time,
	hashRange NetworkPingHashRange,
	callback func(*NetworkPingTerm),
) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				pinger_kind,
				pinger_id,
				target_extender_id,
				rtt_ms,
				probe_time
			FROM network_ping
			WHERE
				$1 <= create_time AND
				cosign = $2 AND
				hop_count = 0 AND
				uuid_hash_extended(pinger_id, 0) BETWEEN $3 AND $4
			ORDER BY create_time, ping_id
			`,
			minCreateTime.UTC(),
			NetworkPingCosignCosigned,
			hashRange.Lo,
			hashRange.Hi,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				term := &NetworkPingTerm{}
				server.Raise(result.Scan(
					&term.PingerKind,
					&term.PingerId,
					&term.TargetExtenderId,
					&term.RttMs,
					&term.ProbeTime,
				))
				callback(term)
			}
		})
	})
}

// Counts, exactly, the ordered pairs and the
// parties of the co-signed direct pings created at or after `minCreateTime`:
// the terms and nodes a derivation would solve. The derive phase's first plan
// has no earlier run to project from and projects from these.
func CountNetworkPingTermsAndNodes(ctx context.Context, minCreateTime time.Time) (terms int64, nodes int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			WITH window_ping AS (
				SELECT pinger_kind, pinger_id, target_extender_id
				FROM network_ping
				WHERE $1 <= create_time AND cosign = $2 AND hop_count = 0
			)
			SELECT
				(SELECT count(*) FROM (SELECT DISTINCT pinger_kind, pinger_id, target_extender_id FROM window_ping) AS pair),
				(SELECT count(*) FROM (
					SELECT pinger_kind AS node_kind, pinger_id AS node_id FROM window_ping
					UNION
					SELECT $3 AS node_kind, target_extender_id AS node_id FROM window_ping
				) AS node)
			`,
			minCreateTime.UTC(),
			NetworkPingCosignCosigned,
			NetworkPingPingerKindExtender,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&terms, &nodes))
			}
		})
	})
	return terms, nodes
}

// One refused ping as the reputation reads it (GEOMAP §5.5): the pinger's
// claim that the target refused it, and the reason it says the target gave.
type NetworkPingRefusal struct {
	PingerKind int
	// the provider's client id, or the pinging extender's extender id
	PingerId         server.Id
	TargetExtenderId server.Id
	Reason           int
}

// Streams every refused ping created at or after `minCreateTime` to
// `callback`, one fresh value per row, in no particular order, relayed or not: the chain end judged a relayed claim exactly as a
// direct one. A refusal is never a measurement and, alone, never evidence
// against the target; it counts only through the two-sided refusal rates
// (§5.5). Unknown pings are neither: a flaky path is not a refusing target.
// The callback runs while the query is open, so it must not use the database
// itself.
func GetNetworkPingRefusals(
	ctx context.Context,
	minCreateTime time.Time,
	callback func(*NetworkPingRefusal),
) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				pinger_kind,
				pinger_id,
				target_extender_id,
				cosign_reason
			FROM network_ping
			WHERE $1 <= create_time AND cosign = $2
			`,
			minCreateTime.UTC(),
			NetworkPingCosignRejected,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				refusal := &NetworkPingRefusal{}
				server.Raise(result.Scan(
					&refusal.PingerKind,
					&refusal.PingerId,
					&refusal.TargetExtenderId,
					&refusal.Reason,
				))
				callback(refusal)
			}
		})
	})
}

// Co-signed pings of one pinger toward one target, relayed through an NLayer
// chain (GEOMAP §2.9), as the reputation counts them.
type NetworkPingAttestationCount struct {
	PingerKind int
	// the provider's client id, or the pinging extender's extender id
	PingerId         server.Id
	TargetExtenderId server.Id
	Count            int
}

// Streams, per pinger and target, the
// co-signed pings created at or after `minCreateTime` that were relayed, one
// fresh value per pair, in no particular order.
//
// A refusal rate is the share of a party's attestations that were refused
// (§5.5), so its denominator is every attestation with a verdict. The direct
// co-signed ones arrive as GetNetworkPingTerms rows and the refused ones,
// relayed or not, as GetNetworkPingRefusals rows; these are the rest. They are
// counts rather than rows because they are never terms: a chain end judges a
// relayed claim exactly as a direct one, so its co-signature counts as
// evidence, while its round trip includes the detour through the front. The
// callback runs while the query is open, so it must not use the database
// itself.
func GetNetworkPingRelayedCosignCounts(
	ctx context.Context,
	minCreateTime time.Time,
	callback func(*NetworkPingAttestationCount),
) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				pinger_kind,
				pinger_id,
				target_extender_id,
				COUNT(*)
			FROM network_ping
			WHERE $1 <= create_time AND cosign = $2 AND 0 < hop_count
			GROUP BY pinger_kind, pinger_id, target_extender_id
			`,
			minCreateTime.UTC(),
			NetworkPingCosignCosigned,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				count := &NetworkPingAttestationCount{}
				server.Raise(result.Scan(
					&count.PingerKind,
					&count.PingerId,
					&count.TargetExtenderId,
					&count.Count,
				))
				callback(count)
			}
		})
	})
}

// The partitions (GEOMAP §5.7, D26). network_ping is range-partitioned by
// create_time into utc days, each named for its day (network_ping_p20260924
// holds 2026-09-24). Retention drops a whole day once every ping in it is past
// its keep timeout -- the span, set by the ping report, over which another
// copy of its claim could still arrive -- and never deletes rows: at fleet scale the table takes
// hundreds of millions of rows a day, and row deletes leave the dead tuples,
// vacuum debt and index bloat that client_reliability ran into, where a
// dropped partition hands its space back at once. The sweep creates the days
// ahead of the inserts and drops the days behind.

// Tunables of the partition maintenance.
type NetworkPingPartitionSettings struct {
	// days after today kept ahead of the inserts, so the ingest runs on
	// through a sweep outage that long
	AheadDays int
	// how long a partition create or drop waits for its lock. Both take an
	// access exclusive lock on network_ping, and one waiting behind a long
	// read holds every insert behind it, so past this it gives up and the
	// next sweep retries it
	LockTimeout time.Duration
}

// Two days ahead, which with today is what the conversion created, and a lock
// wait of seconds.
func DefaultNetworkPingPartitionSettings() *NetworkPingPartitionSettings {
	return &NetworkPingPartitionSettings{
		AheadDays:   2,
		LockTimeout: 5 * time.Second,
	}
}

// Tunables of the ping tallies (the tallies, below).
type NetworkPingTallySettings struct {
	// shards of the fleet's hour tally. Every report of the fleet adds to the
	// same few rows of the hour, so the rows are split by the pinger's id to
	// spread their row locks over this many
	HourShardCount int
	// the round trip past which a ping claims more than half the planet at the
	// solver's km per ms: no geometry holds it, so the §2.19c signal watches
	// the share of them as a clock or wire fault
	HalfPlanetRttMs int
}

// Sixteen shards, and half the earth's circumference at the solver's km per
// ms. The tally migration's backfill used the same two values.
func DefaultNetworkPingTallySettings() *NetworkPingTallySettings {
	return &NetworkPingTallySettings{
		HourShardCount:  16,
		HalfPlanetRttMs: int(math.Round(math.Pi * solve.EarthRadiusKm / solve.DefaultSettings().KmPerMs)),
	}
}

// One partition of network_ping, with its bounds as the catalog has them. A
// bound that is not a finite timestamp (minvalue, a default partition) is the
// zero time.
type NetworkPingPartition struct {
	Name  string
	Lower time.Time
	Upper time.Time
}

// The tables kept in day partitions: network_ping, and the tallies that grow
// with it -- the distinct pingers and targets of each day and the per-target
// hour tally. The sweep keeps every one of them on the same days, and an
// insert that finds a day missing in any of them makes the day in all.
var networkPingPartitionedTables = []string{
	"network_ping",
	"network_ping_pinger_day",
	"network_ping_target_day",
	"network_ping_target_hour_tally",
}

// The utc day holding `t`, which is the day of the partition a row created at
// `t` lands in.
func networkPingPartitionDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// The name of the partition of `table` holding `t`: the table's name, _p, and
// the day.
func networkPingTablePartitionName(table string, t time.Time) string {
	return table + "_p" + networkPingPartitionDay(t).Format("20060102")
}

// The name of the network_ping partition holding `t`.
func NetworkPingPartitionName(t time.Time) string {
	return networkPingTablePartitionName("network_ping", t)
}

// Whether `partitionName` is one this code makes for `table`: the table's
// name, _p, and eight digits of a day. The sweep drops no other.
func networkPingOwnPartition(table string, partitionName string) bool {
	day, ok := strings.CutPrefix(partitionName, table+"_p")
	return ok && len(day) == 8 && strings.Trim(day, "0123456789") == ""
}

// The partitions attached to network_ping, in name order.
func GetNetworkPingPartitions(ctx context.Context) []*NetworkPingPartition {
	return GetNetworkPingTablePartitions(ctx, "network_ping")
}

// The partitions attached to `table`, in name order. Each bound is read back
// from the catalog's own text and cast in the same session, so it parses
// whatever the session's date style, and a date bound reads as its midnight.
func GetNetworkPingTablePartitions(ctx context.Context, table string) []*NetworkPingPartition {
	partitions := []*NetworkPingPartition{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				partition_relation.relname,
				bound.lower_bound,
				bound.upper_bound
			FROM pg_inherits AS inheritance
			JOIN pg_class AS partition_relation ON partition_relation.oid = inheritance.inhrelid
			LEFT JOIN LATERAL (
				SELECT
					CASE WHEN isfinite((matched.value)[1]::timestamp) THEN (matched.value)[1]::timestamp END AS lower_bound,
					CASE WHEN isfinite((matched.value)[2]::timestamp) THEN (matched.value)[2]::timestamp END AS upper_bound
				FROM regexp_match(
					pg_get_expr(partition_relation.relpartbound, partition_relation.oid),
					'^FOR VALUES FROM \(''([^'']*)''\) TO \(''([^'']*)''\)$'
				) AS matched(value)
			) AS bound ON true
			WHERE inheritance.inhparent = to_regclass($1)
			ORDER BY partition_relation.relname
			`,
			"public."+table,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				partition := &NetworkPingPartition{}
				var lower *time.Time
				var upper *time.Time
				server.Raise(result.Scan(&partition.Name, &lower, &upper))
				if lower != nil {
					partition.Lower = lower.UTC()
				}
				if upper != nil {
					partition.Upper = upper.UTC()
				}
				partitions = append(partitions, partition)
			}
		})
	})
	return partitions
}

// The sweep's decision for `table`, apart from the database so the growth
// model runs the same rule (GEOMAP §5.8): the days from today through
// `settings.AheadDays` after it that no partition covers, and the partitions
// this code named for the table whose upper bound is older than
// `now - keepTimeout`, so that every row they hold is past the span it must
// be kept. A partition under another name, or without finite bounds, is never
// dropped here: this code did not make it, and the migration signal names it
// (SIGNALS.md §8.9).
func PlanNetworkPingPartitions(
	table string,
	now time.Time,
	keepTimeout time.Duration,
	partitions []*NetworkPingPartition,
	settings *NetworkPingPartitionSettings,
) (createDays []time.Time, dropPartitions []*NetworkPingPartition) {
	today := networkPingPartitionDay(now)
	for i := 0; i <= settings.AheadDays; i += 1 {
		day := today.AddDate(0, 0, i)
		// a partition overlapping the day covers it, and a second would
		// overlap it
		covered := slices.ContainsFunc(partitions, func(partition *NetworkPingPartition) bool {
			return !partition.Lower.IsZero() && !partition.Upper.IsZero() &&
				partition.Lower.Before(day.AddDate(0, 0, 1)) && day.Before(partition.Upper)
		})
		if !covered {
			createDays = append(createDays, day)
		}
	}
	cut := now.Add(-keepTimeout)
	for _, partition := range partitions {
		if networkPingOwnPartition(table, partition.Name) &&
			!partition.Upper.IsZero() && partition.Upper.Before(cut) {
			dropPartitions = append(dropPartitions, partition)
		}
	}
	return createDays, dropPartitions
}

// Creates the partition of `table` for the day holding `day` unless it
// exists, waiting at most `lockTimeout` for its lock
// (NetworkPingPartitionSettings), and raises what fails. The sweep creates the
// days ahead; an insert creates a day it finds missing.
func createNetworkPingTablePartition(ctx context.Context, table string, day time.Time, lockTimeout time.Duration) {
	day = networkPingPartitionDay(day)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, lockTimeout.Milliseconds()),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				networkPingTablePartitionName(table, day),
				table,
				day.Format("2006-01-02"),
				day.AddDate(0, 0, 1).Format("2006-01-02"),
			),
		))
	}, server.OptNoRetry())
}

// The sweep's partition pass (GEOMAP §5.7): for network_ping and every tally
// kept by day, creates the partitions of today and the `settings.AheadDays`
// days after it that no partition covers, and drops every partition this code
// made whose upper bound is older than `now - keepTimeout`. Then it deletes
// the fleet's hour tally before the oldest day network_ping still keeps,
// whole days of a table too small to partition, so that tally follows the
// ping days. It never deletes a ping row. A statement that fails -- mostly one
// that could not take its lock within `settings.LockTimeout` behind a long
// read -- is logged and left to the next sweep, which the days ahead make
// harmless; the end of the context ends the pass.
func MaintainNetworkPingPartitions(
	ctx context.Context,
	now time.Time,
	keepTimeout time.Duration,
	settings *NetworkPingPartitionSettings,
) (createdPartitionNames []string, droppedPartitionNames []string, removedHourTallyCount int) {
	// runs one statement of the pass and reports whether it took
	run := func(name string, outcome string, statement func()) (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				if server.IsDoneError(r) {
					panic(r)
				}
				glog.Warningf("[ping]%s not %s: %v; the next sweep retries\n", name, outcome, r)
				ok = false
			}
		}()
		statement()
		return true
	}
	for _, table := range networkPingPartitionedTables {
		createDays, dropPartitions := PlanNetworkPingPartitions(
			table,
			now,
			keepTimeout,
			GetNetworkPingTablePartitions(ctx, table),
			settings,
		)
		for _, day := range createDays {
			partitionName := networkPingTablePartitionName(table, day)
			if run(partitionName, "created", func() {
				createNetworkPingTablePartition(ctx, table, day, settings.LockTimeout)
			}) {
				createdPartitionNames = append(createdPartitionNames, partitionName)
			}
		}
		for _, partition := range dropPartitions {
			if run(partition.Name, "dropped", func() {
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(
						ctx,
						fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, settings.LockTimeout.Milliseconds()),
					))
					// the name passed networkPingOwnPartition, so it is a
					// plain identifier
					server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, partition.Name)))
				}, server.OptNoRetry())
			}) {
				droppedPartitionNames = append(droppedPartitionNames, partition.Name)
			}
		}
	}

	oldestDay := time.Time{}
	for _, partition := range GetNetworkPingPartitions(ctx) {
		if networkPingOwnPartition("network_ping", partition.Name) && !partition.Lower.IsZero() &&
			(oldestDay.IsZero() || partition.Lower.Before(oldestDay)) {
			oldestDay = partition.Lower
		}
	}
	if !oldestDay.IsZero() {
		run("network_ping_hour_tally", "trimmed", func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				tag, err := tx.Exec(
					ctx,
					`DELETE FROM network_ping_hour_tally WHERE hour < $1`,
					oldestDay,
				)
				server.Raise(err)
				removedHourTallyCount = int(tag.RowsAffected())
			}, server.OptNoRetry())
		})
	}
	return createdPartitionNames, droppedPartitionNames, removedHourTallyCount
}

// The tallies (GEOMAP §2.7, §5.7). The dashboard's and the monitor's counts
// read tallies the ingest keeps, never the rows: at the target a day's
// partition of network_ping is 275 GiB, and the dashboard counted over it on
// every refresh. Every store adds what it stored to the tallies in its own
// transaction, so they are exact, and a replay, a refused report or a rejected
// claim stores nothing and adds nothing. network_ping_hour_tally counts pings
// per hour, pinger kind, relay, verdict and refusal reason, with the zero and
// beyond-half-planet round trips, sharded by the pinger's id so the fleet's
// reports do not queue on a few rows. network_ping_target_hour_tally counts
// pings and refusals per hour, target and pinger kind, for the per-extender
// counters. network_ping_pinger_day and network_ping_target_day hold the
// distinct pingers and targets of each utc day, so a day's distinct count is
// an index-only count. The sweep keeps them all on the pings' days: the day
// tables are partitioned and dropped with the ping days, and the small hour
// tally is deleted behind the oldest ping day.
//
// A ping is a ping here: every pinger kind and every verdict counts, since the
// question the dashboard answers first is whether pings are being measured and
// reported at all, and the split by kind and verdict is what says which.

// The pinger kinds every window count carries, in publication order, so a
// kind that has gone quiet is a zero rather than an absent series.
var NetworkPingPingerKinds = []int{
	NetworkPingPingerKindProvider,
	NetworkPingPingerKindExtender,
}

// The verdicts every window count carries, in publication order, so a verdict
// that has gone quiet is a zero rather than an absent series.
var NetworkPingCosigns = []int{
	NetworkPingCosignCosigned,
	NetworkPingCosignRejected,
	NetworkPingCosignUnknown,
}

// Pings of one pinger kind with one verdict, direct or relayed.
type ExtenderPingOutcomeCount struct {
	PingerKind int
	Cosign     int
	// through an NLayer chain (hop_count above zero)
	Relayed bool
	Pings   int64
}

// Distinct pingers of one kind.
type ExtenderPingSourceCount struct {
	PingerKind int
	Sources    int64
}

// The trailing 24 hour window of pings.
//
// Pings are rows; sources and targets are the distinct parties behind them.
// The pair is what tells "a few nodes pinging everything" from "everyone
// pinging a few extenders", which the row count alone cannot.
type ExtenderPingCount struct {
	Pings24h int64
	// every pinger kind, verdict and relay, zero when none
	Outcomes24h []ExtenderPingOutcomeCount
	// every pinger kind, zero when none
	Sources24h []ExtenderPingSourceCount
	// distinct extenders pinged
	Targets24h int64
}

// Counts the pings of the trailing day from the tallies. The window is the 24
// clock hours up to and including the current one, the day to within an hour,
// and the counts per kind, verdict and relay are its hours of the fleet's
// tally. The distinct pingers per kind and the distinct targets are exact over
// the utc days that cover the window, from the day tallies, and so an upper
// bound of the window's own that is at most one day wider.
func CountExtenderPings(ctx context.Context, now time.Time) ExtenderPingCount {
	windowStart := now.UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	firstDay := networkPingPartitionDay(windowStart)
	lastDay := networkPingPartitionDay(now)
	// one outcome count's group: the kind, the verdict and the relay
	type outcomeKey struct {
		pingerKind int
		cosign     int
		relayed    bool
	}
	count := ExtenderPingCount{}
	outcomeKeyPings := map[outcomeKey]int64{}
	kindSources := map[int]int64{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT pinger_kind, cosign, relayed, sum(ping_count)::bigint
			FROM network_ping_hour_tally
			WHERE $1 <= hour
			GROUP BY pinger_kind, cosign, relayed
			`,
			windowStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var key outcomeKey
				var pings int64
				server.Raise(result.Scan(&key.pingerKind, &key.cosign, &key.relayed, &pings))
				outcomeKeyPings[key] = pings
				count.Pings24h += pings
			}
		})
		result, err = conn.Query(
			ctx,
			`
			SELECT pinger_kind, count(DISTINCT pinger_id)
			FROM network_ping_pinger_day
			WHERE $1 <= day AND day <= $2
			GROUP BY pinger_kind
			`,
			firstDay,
			lastDay,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var pingerKind int
				var sources int64
				server.Raise(result.Scan(&pingerKind, &sources))
				kindSources[pingerKind] = sources
			}
		})
		result, err = conn.Query(
			ctx,
			`
			SELECT count(DISTINCT target_extender_id)
			FROM network_ping_target_day
			WHERE $1 <= day AND day <= $2
			`,
			firstDay,
			lastDay,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count.Targets24h))
			}
		})
	})
	for _, pingerKind := range NetworkPingPingerKinds {
		for _, cosign := range NetworkPingCosigns {
			for _, relayed := range []bool{false, true} {
				count.Outcomes24h = append(count.Outcomes24h, ExtenderPingOutcomeCount{
					PingerKind: pingerKind,
					Cosign:     cosign,
					Relayed:    relayed,
					Pings: outcomeKeyPings[outcomeKey{
						pingerKind: pingerKind,
						cosign:     cosign,
						relayed:    relayed,
					}],
				})
			}
		}
		count.Sources24h = append(count.Sources24h, ExtenderPingSourceCount{
			PingerKind: pingerKind,
			Sources:    kindSources[pingerKind],
		})
	}
	return count
}

// The pings one target received from one pinger kind
// in one hour bucket of create_time, and how many of them were refused.
type ExtenderHourPingCount struct {
	ExtenderId server.Id
	PingerKind int
	Pings      int64
	Rejections int64
}

// Returns the pings each target received in the hour beginning at hourStart,
// per pinger kind, and how many were refused: that hour's rows of the
// per-target tally, read once per closed hour, exactly as the contract counter
// is fed. The kind is kept so the extender to extender traffic is visible per
// hour; it has two values, so it at most doubles the per extender series.
func CountExtenderPingsByHour(ctx context.Context, hourStart time.Time) []ExtenderHourPingCount {
	counts := []ExtenderHourPingCount{}
	hourStart = hourStart.UTC().Truncate(time.Hour)
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT target_extender_id, pinger_kind, ping_count, rejection_count
			FROM network_ping_target_hour_tally
			WHERE hour = $1
			`,
			hourStart,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var count ExtenderHourPingCount
				server.Raise(result.Scan(
					&count.ExtenderId,
					&count.PingerKind,
					&count.Pings,
					&count.Rejections,
				))
				counts = append(counts, count)
			}
		})
	})
	return counts
}
