package model

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// ReconcileSidecarCounters (QUOTA-M2-12): §20.7's "exact, recounted nightly" by plan-QUOTA-small-readings
// reading 7 (QD-356), one bounded page of network_quota_state's networks a call, resumable by network cursor:
//
//	storage_over  recomputed from the stored count and the network's limit now, so a limit an override or a
//	              tier change moved since the count's last write is followed
//	connected     {sq_<network_id>}connected_sidecar's members past their expiry dropped; the resident poll
//	              writes and refreshes the live ones (plan-QUOTA-sidecar-clients-store #1)
//	spent         the 60 s spent pass run once a night from the start (ClearSpentNetworks), so a mark no
//	              balance backs is lifted even while that chain is behind
//	over limits   a network whose connected sidecars pass sidecar_clients, or whose stored count passes its
//	              storage limit, counted in urnetwork_quota_reconcile_over_limit_total by limit key; nothing is
//	              refused here
//
// Every correction is counted in urnetwork_quota_reconcile_corrected_total. The other row-counted capacities
// (dns_entries, custom domains, members) count in their owners' transactions (DNS, TEAM), and their nightly
// recount is not here yet.

var quotaReconcileCorrectedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_quota_reconcile_corrected_total",
	Help: "Counters the nightly reconcile found wrong and corrected, by field: storage_over, connected (expired members dropped), spent (marks lifted).",
}, []string{"field"})

var quotaReconcileOverLimitCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_quota_reconcile_over_limit_total",
	Help: "Networks the nightly reconcile found over a row-counted capacity, by limit key; nothing is refused.",
}, []string{"limit_key"})

func init() {
	prometheus.MustRegister(quotaReconcileCorrectedCounter, quotaReconcileOverLimitCounter)
}

// The sidecar_clients key (§20.4).
const sidecarClientsLimitKey = "sidecar_clients"

// One page of the reconcile.
type ReconcileSidecarCountersResult struct {
	LookedCount             int
	StorageOverCorrected    int
	ConnectedExpiredRemoved int64
	SpentCleared            int
	// the page's last network, where the next page starts; nil when the pass is done
	LastNetworkId *server.Id
}

// One page of at most pageCount networks after after (nil: the start, which also runs the spent pass), at now.
func ReconcileSidecarCounters(ctx context.Context, now time.Time, after *server.Id, pageCount int) (result ReconcileSidecarCountersResult, returnErr error) {
	if !server.SidecarMg2Linked() {
		return result, ErrNetworkQuotaStateUnavailable
	}
	if pageCount <= 0 {
		return result, errors.New("a reconcile page of no networks")
	}
	type stateRow struct {
		networkId       server.Id
		storedByteCount int64
		storageOver     bool
	}
	rows := []*stateRow{}
	server.Db(ctx, func(conn server.PgConn) {
		stateRows, err := conn.Query(
			ctx,
			`
				SELECT network_id, stored_byte_count, storage_over
				FROM network_quota_state
				WHERE $1::uuid IS NULL OR $1 < network_id
				ORDER BY network_id
				LIMIT $2
			`,
			after,
			pageCount,
		)
		server.WithPgResult(stateRows, err, func() {
			for stateRows.Next() {
				row := &stateRow{}
				server.Raise(stateRows.Scan(&row.networkId, &row.storedByteCount, &row.storageOver))
				rows = append(rows, row)
			}
		})
	})
	result.LookedCount = len(rows)
	if len(rows) == pageCount {
		lastNetworkId := rows[len(rows)-1].networkId
		result.LastNetworkId = &lastNetworkId
	}

	// storage_over from the count and the limit now, read with no connection held
	correctNetworkIds := []server.Id{}
	correctOvers := []bool{}
	for _, row := range rows {
		limitByteCount := NetworkStorageLimitByteCount(ctx, row.networkId)
		over := networkStorageOver(row.storedByteCount, limitByteCount)
		if over {
			quotaReconcileOverLimitCounter.WithLabelValues(quotaStorageLimitKey).Inc()
		}
		if over != row.storageOver {
			correctNetworkIds = append(correctNetworkIds, row.networkId)
			correctOvers = append(correctOvers, over)
		}
	}
	if 0 < len(correctNetworkIds) {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_quota_state
					SET storage_over = correct.storage_over
					FROM unnest($1::uuid[], $2::bool[]) AS correct(network_id, storage_over)
					WHERE network_quota_state.network_id = correct.network_id
				`,
				correctNetworkIds,
				correctOvers,
			))
			result.StorageOverCorrected = int(tag.RowsAffected())
		})
		quotaReconcileCorrectedCounter.WithLabelValues("storage_over").Add(float64(result.StorageOverCorrected))
	}

	// the connected set: expired members out, and the live ones counted against sidecar_clients
	nowMs := strconv.FormatInt(now.UnixMilli(), 10)
	server.Redis(ctx, func(client server.RedisClient) {
		for _, row := range rows {
			key := networkSidecarConnectedKey(row.networkId)
			removed, err := client.ZRemRangeByScore(ctx, key, "-inf", nowMs).Result()
			if err != nil {
				returnErr = err
				return
			}
			result.ConnectedExpiredRemoved += removed
			connected, err := client.ZCount(ctx, key, "("+nowMs, "+inf").Result()
			if err != nil {
				returnErr = err
				return
			}
			if limit := NetworkSidecarLimits(ctx, row.networkId).Limit(sidecarClientsLimitKey); limit != proSidecarLimitNone && limit < connected {
				quotaReconcileOverLimitCounter.WithLabelValues(sidecarClientsLimitKey).Inc()
			}
		}
	})
	if returnErr != nil {
		return
	}
	quotaReconcileCorrectedCounter.WithLabelValues("connected").Add(float64(result.ConnectedExpiredRemoved))

	if after == nil {
		var spentAfter *server.Id
		for {
			spent, err := ClearSpentNetworks(ctx, now, spentAfter)
			if err != nil {
				returnErr = err
				return
			}
			result.SpentCleared += spent.ClearedCount
			if spent.LastNetworkId == nil {
				break
			}
			spentAfter = spent.LastNetworkId
		}
		quotaReconcileCorrectedCounter.WithLabelValues("spent").Add(float64(result.SpentCleared))
	}
	return
}
