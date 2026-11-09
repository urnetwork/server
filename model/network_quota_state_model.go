package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// network_quota_state (DESIGN.md 20.6's Empty balance, 20.7's stored, count and limit rows; §2.8
// api-13; QUOTA-M2-04): each network's stored-byte count, whether it is over its storage limit, and
// when it was marked spent. A row lives until its network is deleted. The table belongs to MG2: a
// binary that does not link it reads and writes nothing here (server.SidecarMg2Linked).
//
// The stored count is the nightly pass's absolute count (SetNetworkStoredByteCount, which
// server/taskworker/evs_nightly.go calls with EVSB-M2-12's count), moved between passes by usage
// batches' deltas (ApplyStoredByteDeltaInTx), clamped at zero with the drift counted. Deltas landing
// while a pass runs are not replayed: the next pass corrects them (plan-QUOTA-small-readings reading
// 6). storage_over is recomputed wherever the count moves; limit and tier changes reach it through
// ReconcileSidecarCounters (QUOTA-M2-12) and the downgrade jobs (QUOTA-M6-05). A network is at its
// limit once its count reaches it, so the limit itself is over: zero, "not included", refuses the
// first upload.
//
// A network is spent once a lease, a relayed reservation or an evs debit finds no balance (§20.6).
// The mark keeps its earliest time, and §20.6 gives no rule for leaving it, so a taskworker pass every
// SpentPassInterval clears a network once any in-window balance has room (reading 4): a new grant
// reopens a network within about a minute. A lease refused while Postgres still shows room may mark a
// network again after the pass clears it; the next lease or batch settles it.
//
// GetQuotaOverLimit is Quota.OverLimit's answer, which evs and sockets re-read every 5 s. Storage
// refusals ship dark: while enforce_sidecar_limits is false the storage map stays empty and no
// storage row is read. The spent map never goes dark, since an empty balance refuses as escrow does
// (§20.5). A network over storage but holding balance is in the storage map only, so a Find does not
// answer it quota (SOCK reading 9).

// Refused: this binary does not link MG2, so network_quota_state does not exist for it.
var ErrNetworkQuotaStateUnavailable = errors.New("network quota state is not available")

// The limit_key of a storage refusal: §20.4's key.
const quotaStorageLimitKey = "storage"

// The limit_key of a spent network's refusal: the transfer balance pro.yml grants as `data`, which a
// purchase lifts. It carries no limit or count: no balance is left.
const quotaSpentLimitKey = "data"

type NetworkQuotaStateSettings struct {
	// Networks one statement of GetQuotaOverLimit reads.
	OverLimitPageSize int
	// The most networks one GetQuotaOverLimit answer names. Past it the rest read as not over, as an
	// unread list would, and urnetwork_quota_over_limit_truncated_total counts the answer.
	OverLimitMaxNetworkCount int
	// How often the spent pass runs: reading 4's 60 s.
	SpentPassInterval time.Duration
	// Spent networks one statement of the pass looks at.
	SpentPassPageSize int
	// Pages one pass looks at before it hands the rest to the next run.
	SpentPassMaxPageCount int
}

func DefaultNetworkQuotaStateSettings() *NetworkQuotaStateSettings {
	return &NetworkQuotaStateSettings{
		OverLimitPageSize:        5000,
		OverLimitMaxNetworkCount: 200000,
		SpentPassInterval:        60 * time.Second,
		SpentPassPageSize:        500,
		SpentPassMaxPageCount:    20,
	}
}

var networkQuotaStateSettings = DefaultNetworkQuotaStateSettings()

// Replaces the process's settings, returning a restore function. For tests.
func Testing_SetNetworkQuotaStateSettings(settings *NetworkQuotaStateSettings) func() {
	prev := networkQuotaStateSettings
	networkQuotaStateSettings = settings
	return func() {
		networkQuotaStateSettings = prev
	}
}

var quotaStoredDriftByteCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_quota_stored_drift_bytes_total",
	Help: "Bytes a stored-byte delta would have taken a network's count below zero, clamped there until the nightly count corrects it (§20.7)",
})

var quotaOverLimitTruncatedCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_quota_over_limit_truncated_total",
	Help: "Quota.OverLimit answers cut at OverLimitMaxNetworkCount, the networks past it reading as not over",
})

func init() {
	prometheus.MustRegister(quotaStoredDriftByteCounter, quotaOverLimitTruncatedCounter)
}

// One network's row.
type NetworkQuotaState struct {
	NetworkId       server.Id
	StoredByteCount int64
	StorageOver     bool
	// nil when the network is not spent
	SpentTime *time.Time
}

// The network's row, nil when it has none.
func GetNetworkQuotaState(ctx context.Context, networkId server.Id) (state *NetworkQuotaState, returnErr error) {
	if !server.SidecarMg2Linked() {
		return nil, ErrNetworkQuotaStateUnavailable
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT stored_byte_count, storage_over, spent_time
				FROM network_quota_state
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				state = &NetworkQuotaState{NetworkId: networkId}
				server.Raise(result.Scan(&state.StoredByteCount, &state.StorageOver, &state.SpentTime))
			}
		})
	})
	return
}

// The storage limit in bytes for a network of tier with limits, -1 for none: `storage`, or for Team,
// storage_overage_max while storage_gib_month is priced, since storage past `storage` is then metered
// up to it, and `storage` while it is unset (owner decision 122). A row that raises `storage` past
// storage_overage_max raises the limit with it.
func networkStorageLimitByteCount(tier NetworkTier, limits *ProSidecarLimits, team *ProTeamConfig) int64 {
	storage := limits.Limit(quotaStorageLimitKey)
	if storage == proSidecarLimitNone || tier != NetworkTierTeam {
		return storage
	}
	if _, priced := team.OverageStorageGibMonthUsd(); !priced {
		return storage
	}
	return max(storage, int64(team.StorageOverageMax))
}

// The network's storage limit in bytes, -1 for none, after its network_sidecar_limit rows.
func NetworkStorageLimitByteCount(ctx context.Context, networkId server.Id) int64 {
	tier, limits := networkSidecarLimitsWithTier(ctx, nil, networkId)
	return networkStorageLimitByteCount(tier, limits, &Pro().Team)
}

// NetworkStorageLimitByteCount through the caller's transaction where the caches miss.
func NetworkStorageLimitByteCountInTx(ctx context.Context, tx server.PgTx, networkId server.Id) int64 {
	tier, limits := networkSidecarLimitsWithTier(ctx, tx, networkId)
	return networkStorageLimitByteCount(tier, limits, &Pro().Team)
}

// Whether a count is at or past a limit, -1 being none.
func networkStorageOver(storedByteCount int64, limitByteCount int64) bool {
	return limitByteCount != proSidecarLimitNone && limitByteCount <= storedByteCount
}

// One network's change in a transaction that may touch several.
type networkQuotaChange struct {
	storedByteDelta int64
	// the network's storage limit, read before the transaction; only read with a delta
	limitByteCount int64
	hasStoredDelta bool
	spentTime      time.Time
	hasSpentTime   bool
}

// Applies changes in network-id order: rows created where missing, then locked in that order, then
// written, so two transactions touching the same networks cannot deadlock. A delta moves the count,
// clamped at zero with the drift counted, and recomputes storage_over against the change's limit; a
// spent time keeps the earliest. Answers what each network's count became.
func applyNetworkQuotaChangesInTx(ctx context.Context, tx server.PgTx, networkIdChanges map[server.Id]*networkQuotaChange) map[server.Id]int64 {
	if len(networkIdChanges) == 0 {
		return map[server.Id]int64{}
	}
	networkIds := make([]server.Id, 0, len(networkIdChanges))
	for networkId := range networkIdChanges {
		networkIds = append(networkIds, networkId)
	}
	slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})

	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_quota_state (network_id)
			SELECT network_id FROM unnest($1::uuid[]) AS change(network_id)
			ORDER BY network_id
			ON CONFLICT (network_id) DO NOTHING
		`,
		networkIds,
	))

	type held struct {
		storedByteCount int64
		storageOver     bool
	}
	networkIdHelds := map[server.Id]*held{}
	result, err := tx.Query(
		ctx,
		`
			SELECT network_id, stored_byte_count, storage_over
			FROM network_quota_state
			WHERE network_id = ANY($1::uuid[])
			ORDER BY network_id
			FOR UPDATE
		`,
		networkIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			state := &held{}
			server.Raise(result.Scan(&networkId, &state.storedByteCount, &state.storageOver))
			networkIdHelds[networkId] = state
		}
	})

	storedByteCounts := make([]int64, 0, len(networkIds))
	storageOvers := make([]bool, 0, len(networkIds))
	spentSets := make([]bool, 0, len(networkIds))
	spentTimes := make([]time.Time, 0, len(networkIds))
	networkIdStoredByteCounts := map[server.Id]int64{}
	for _, networkId := range networkIds {
		change := networkIdChanges[networkId]
		state, ok := networkIdHelds[networkId]
		if !ok {
			server.Raise(fmt.Errorf("network quota state for %s is missing after its insert", networkId))
		}
		storedByteCount := state.storedByteCount
		storageOver := state.storageOver
		if change.hasStoredDelta {
			moved := storedByteCount + change.storedByteDelta
			if moved < 0 {
				quotaStoredDriftByteCounter.Add(float64(-moved))
				moved = 0
			}
			storedByteCount = moved
			storageOver = networkStorageOver(storedByteCount, change.limitByteCount)
		}
		storedByteCounts = append(storedByteCounts, storedByteCount)
		storageOvers = append(storageOvers, storageOver)
		spentSets = append(spentSets, change.hasSpentTime)
		spentTimes = append(spentTimes, change.spentTime.UTC())
		networkIdStoredByteCounts[networkId] = storedByteCount
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_quota_state AS state
			SET
				stored_byte_count = change.stored_byte_count,
				storage_over = change.storage_over,
				spent_time = CASE
					WHEN change.spent_set THEN LEAST(COALESCE(state.spent_time, change.spent_time), change.spent_time)
					ELSE state.spent_time
				END
			FROM unnest($1::uuid[], $2::bigint[], $3::bool[], $4::bool[], $5::timestamp[])
				AS change(network_id, stored_byte_count, storage_over, spent_set, spent_time)
			WHERE state.network_id = change.network_id
		`,
		networkIds,
		storedByteCounts,
		storageOvers,
		spentSets,
		spentTimes,
	))
	return networkIdStoredByteCounts
}

// Moves the network's stored count by delta between nightly passes (§20.6's usage batches), clamping at
// zero and counting the drift, and recomputes storage_over. Reads the limit through tx where the
// caches miss.
func ApplyStoredByteDeltaInTx(ctx context.Context, tx server.PgTx, networkId server.Id, storedByteDelta int64) error {
	if !server.SidecarMg2Linked() {
		return ErrNetworkQuotaStateUnavailable
	}
	applyNetworkQuotaChangesInTx(ctx, tx, map[server.Id]*networkQuotaChange{
		networkId: {
			storedByteDelta: storedByteDelta,
			limitByteCount:  NetworkStorageLimitByteCountInTx(ctx, tx, networkId),
			hasStoredDelta:  true,
		},
	})
	return nil
}

// The nightly pass's absolute write (§20.7's count row): the network's stored count is byteCount, and
// storage_over follows it. EVSB-M2-12's server/taskworker/evs_nightly.go calls it with the count it
// lists, so Quota keeps §20.7's five methods.
func SetNetworkStoredByteCount(ctx context.Context, networkId server.Id, storedByteCount int64) (returnErr error) {
	if !server.SidecarMg2Linked() {
		return ErrNetworkQuotaStateUnavailable
	}
	if storedByteCount < 0 {
		return fmt.Errorf("a stored count of %d", storedByteCount)
	}
	limitByteCount := NetworkStorageLimitByteCount(ctx, networkId)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_quota_state (network_id, stored_byte_count, storage_over)
				VALUES ($1, $2, $3)
				ON CONFLICT (network_id) DO UPDATE
				SET
					stored_byte_count = EXCLUDED.stored_byte_count,
					storage_over = EXCLUDED.storage_over
			`,
			networkId,
			storedByteCount,
			networkStorageOver(storedByteCount, limitByteCount),
		))
	})
	return
}

// Marks the network spent from spentTime, keeping an earlier mark. Idempotent.
func MarkNetworkSpent(ctx context.Context, networkId server.Id, spentTime time.Time) (returnErr error) {
	if !server.SidecarMg2Linked() {
		return ErrNetworkQuotaStateUnavailable
	}
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = MarkNetworkSpentInTx(ctx, tx, networkId, spentTime)
	})
	return
}

// MarkNetworkSpent in the caller's transaction.
func MarkNetworkSpentInTx(ctx context.Context, tx server.PgTx, networkId server.Id, spentTime time.Time) error {
	if !server.SidecarMg2Linked() {
		return ErrNetworkQuotaStateUnavailable
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_quota_state (network_id, spent_time)
			VALUES ($1, $2)
			ON CONFLICT (network_id) DO UPDATE
			SET spent_time = LEAST(COALESCE(network_quota_state.spent_time, EXCLUDED.spent_time), EXCLUDED.spent_time)
		`,
		networkId,
		spentTime.UTC(),
	))
	return nil
}

// What one run of the spent pass did.
type ClearSpentNetworksResult struct {
	// Networks whose mark the run cleared.
	ClearedCount int `json:"cleared_count"`
	// Spent networks the run looked at.
	LookedCount int `json:"looked_count"`
	// Where the next run starts: nil once the run reached the end.
	LastNetworkId *server.Id `json:"last_network_id,omitempty"`
}

// One run of the spent pass (reading 4): from after, in network order, clears each spent network
// that holds an in-window balance with room at now, room being its balance less the debits journaled
// against it and not yet applied. Bounded by SpentPassMaxPageCount pages; a run that stops short
// names where the next starts.
func ClearSpentNetworks(ctx context.Context, now time.Time, after *server.Id) (result ClearSpentNetworksResult, returnErr error) {
	if !server.SidecarMg2Linked() {
		return result, ErrNetworkQuotaStateUnavailable
	}
	settings := networkQuotaStateSettings
	now = now.UTC()
	for range max(settings.SpentPassMaxPageCount, 1) {
		var pageNetworkIds []server.Id
		server.Db(ctx, func(conn server.PgConn) {
			args := []any{settings.SpentPassPageSize}
			query := `
				SELECT network_id
				FROM network_quota_state
				WHERE spent_time IS NOT NULL
				ORDER BY network_id
				LIMIT $1
			`
			if after != nil {
				args = append(args, *after)
				query = `
					SELECT network_id
					FROM network_quota_state
					WHERE spent_time IS NOT NULL AND network_id > $2
					ORDER BY network_id
					LIMIT $1
				`
			}
			spentRows, err := conn.Query(ctx, query, args...)
			server.WithPgResult(spentRows, err, func() {
				for spentRows.Next() {
					var networkId server.Id
					server.Raise(spentRows.Scan(&networkId))
					pageNetworkIds = append(pageNetworkIds, networkId)
				}
			})
		})
		if len(pageNetworkIds) == 0 {
			result.LastNetworkId = nil
			return
		}
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_quota_state AS state
					SET spent_time = NULL
					WHERE
						state.network_id = ANY($1::uuid[]) AND
						state.spent_time IS NOT NULL AND
						EXISTS (
							SELECT 1
							FROM transfer_balance balance
							WHERE
								balance.network_id = state.network_id AND
								balance.active AND
								balance.start_time <= $2 AND
								$2 < balance.end_time AND
								COALESCE((
									SELECT SUM(service.debit_byte_count)
									FROM service_debit_journal service
									WHERE service.balance_id = balance.balance_id AND NOT service.applied
								), 0) + COALESCE((
									SELECT SUM(transfer.debit_byte_count)
									FROM transfer_debit_journal transfer
									WHERE transfer.balance_id = balance.balance_id AND NOT transfer.applied
								), 0) < balance.balance_byte_count
						)
				`,
				pageNetworkIds,
				now,
			))
			result.ClearedCount += int(tag.RowsAffected())
		})
		result.LookedCount += len(pageNetworkIds)
		last := pageNetworkIds[len(pageNetworkIds)-1]
		after = &last
		result.LastNetworkId = after
		if len(pageNetworkIds) < settings.SpentPassPageSize {
			result.LastNetworkId = nil
			return
		}
	}
	return
}

// The refusal for a network over its storage limit with storedByteCount: its limit, and whether a
// higher tier lifts it (a row holds whatever the tier).
func networkStorageRefusal(ctx context.Context, networkId server.Id, storedByteCount int64) *sidecar.QuotaRefusal {
	tier, limits := networkSidecarLimitsWithTier(ctx, nil, networkId)
	limitByteCount := networkStorageLimitByteCount(tier, limits, &Pro().Team)
	hasRow := networkSidecarLimitHasRow(ctx, nil, networkId, quotaStorageLimitKey)
	return &sidecar.QuotaRefusal{
		LimitKey: quotaStorageLimitKey,
		Limit:    limitByteCount,
		Used:     storedByteCount,
		Upgrade:  quotaRefusalUpgrade(tier, quotaStorageLimitKey, storedByteCount, hasRow),
	}
}

// A spent network's refusal: a purchase lifts it.
func networkSpentRefusal() *sidecar.QuotaRefusal {
	return &sidecar.QuotaRefusal{
		LimitKey: quotaSpentLimitKey,
		Upgrade:  true,
	}
}

// Quota.OverLimit's answer (see the file header): the networks over their storage limit while limits
// are enforced, and the spent networks always, each with the refusal to give. Reads the over-limit
// partial index in pages, then each storage refusal's limit from the caches once no connection is held.
func GetQuotaOverLimit(ctx context.Context) (*sidecar.QuotaOverLimit, error) {
	if !server.SidecarMg2Linked() {
		return nil, ErrNetworkQuotaStateUnavailable
	}
	settings := networkQuotaStateSettings
	enforced := Pro().EnforceSidecarLimits
	type overRow struct {
		networkId       server.Id
		storedByteCount int64
		storageOver     bool
		spent           bool
	}
	rows := []*overRow{}
	var after *server.Id
	truncated := false
	for {
		page := []*overRow{}
		server.Db(ctx, func(conn server.PgConn) {
			// while dark only the spent marks are read, through the same index
			predicate := `(storage_over OR spent_time IS NOT NULL)`
			if !enforced {
				predicate = `spent_time IS NOT NULL`
			}
			args := []any{settings.OverLimitPageSize}
			cursor := ``
			if after != nil {
				args = append(args, *after)
				cursor = ` AND network_id > $2`
			}
			result, err := conn.Query(
				ctx,
				`
					SELECT network_id, stored_byte_count, storage_over, spent_time IS NOT NULL
					FROM network_quota_state
					WHERE `+predicate+cursor+`
					ORDER BY network_id
					LIMIT $1
				`,
				args...,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					row := &overRow{}
					server.Raise(result.Scan(&row.networkId, &row.storedByteCount, &row.storageOver, &row.spent))
					page = append(page, row)
				}
			})
		})
		rows = append(rows, page...)
		if len(page) < settings.OverLimitPageSize {
			break
		}
		if settings.OverLimitMaxNetworkCount <= len(rows) {
			truncated = true
			break
		}
		last := page[len(page)-1].networkId
		after = &last
	}
	if truncated {
		quotaOverLimitTruncatedCounter.Inc()
		glog.Warningf("[quota]over limit answer cut at %d networks\n", len(rows))
	}

	overLimit := &sidecar.QuotaOverLimit{
		StorageNetworkIdRefusals: map[connect.Id]*sidecar.QuotaRefusal{},
		SpentNetworkIdRefusals:   map[connect.Id]*sidecar.QuotaRefusal{},
	}
	for _, row := range rows {
		if enforced && row.storageOver {
			overLimit.StorageNetworkIdRefusals[connect.Id(row.networkId)] = networkStorageRefusal(ctx, row.networkId, row.storedByteCount)
		}
		if row.spent {
			overLimit.SpentNetworkIdRefusals[connect.Id(row.networkId)] = networkSpentRefusal()
		}
	}
	return overLimit, nil
}

// Removes the network's row in its deletion transaction, the one way a row goes.
func removeNetworkQuotaStateInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_quota_state
			WHERE network_id = $1
		`,
		networkId,
	))
}
