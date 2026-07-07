package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

const gib = int64(1024 * 1024 * 1024)

func TestStatsProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "provider-net", userId)
		clientSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkId, userId, "provider-net", false, false),
		)

		// two provider clients in the caller network
		providerA := server.NewId()
		providerB := server.NewId()
		statsInsertNetworkClient(ctx, networkId, providerA)
		statsInsertNetworkClient(ctx, networkId, providerB)
		statsInsertProvideKey(ctx, providerA, ProvideModePublic)
		statsInsertProvideKey(ctx, providerB, ProvideModePublic)

		// a customer (source) network + client
		customerNetworkId := server.NewId()
		customerA := server.NewId()

		// providerA, within 24h: one contract, 2 GiB settled, $3 payout
		recentContract := server.NewId()
		statsInsertContract(ctx, recentContract, customerNetworkId, customerA, networkId, providerA, 2*gib, now.Add(-2*time.Hour), now.Add(-1*time.Hour))
		statsInsertContractClose(ctx, recentContract, 2*gib, now.Add(-1*time.Hour))
		statsInsertSweep(ctx, recentContract, networkId, 2*gib, UsdToNanoCents(3.0), now.Add(-1*time.Hour))

		// providerA, ~48h ago: one contract, 5 GiB settled (outside the 24h window)
		oldContract := server.NewId()
		statsInsertContract(ctx, oldContract, customerNetworkId, customerA, networkId, providerA, 5*gib, now.Add(-48*time.Hour), now.Add(-47*time.Hour))
		statsInsertContractClose(ctx, oldContract, 5*gib, now.Add(-47*time.Hour))

		// providerA connection: connected 3h ago, still active
		statsInsertConnection(ctx, providerA, now.Add(-3*time.Hour), nil, true)

		// providerA search interest within 24h: 7 matches this hour
		statsInsertSearchStat(ctx, providerA, now.Truncate(time.Hour), 7)

		// providerB: no activity

		// ---- StatsProviders (last 24h) ----
		result, err := StatsProviders(clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(result.Providers), 2)

		byId := map[server.Id]*ProviderStats{}
		for _, p := range result.Providers {
			byId[p.ClientId] = p
		}

		a := byId[providerA]
		assert.NotEqual(t, a, nil)
		assert.Equal(t, a.TransferDataLast24h, float64(2)) // 2 GiB (old 5 GiB excluded)
		assert.Equal(t, a.ContractsLast24h, 1)
		assert.Equal(t, a.ClientsLast24h, 1)
		assert.Equal(t, a.PayoutLast24h, 3.0)
		assert.Equal(t, a.SearchInterestLast24h, 7)
		assert.Equal(t, a.Connected, true)
		assert.Equal(t, a.ProvideMode, ProvideModePublic)
		if a.UptimeLast24h < 2.9 || 3.2 < a.UptimeLast24h {
			t.Fatalf("providerA uptime expected ~3h, got %f", a.UptimeLast24h)
		}

		b := byId[providerB]
		assert.NotEqual(t, b, nil)
		assert.Equal(t, b.TransferDataLast24h, float64(0))
		assert.Equal(t, b.ContractsLast24h, 0)
		assert.Equal(t, b.Connected, false)
		// arrays must serialize as [], never null
		assert.NotEqual(t, b.ConnectedEventsLast24h, nil)

		// ---- StatsProvidersLastN (last 72h) includes the old contract ----
		wide, err := StatsProvidersLastN(&StatsProvidersArgs{LastN: 72}, clientSession)
		assert.Equal(t, err, nil)
		wideById := map[server.Id]*ProviderStats{}
		for _, p := range wide.Providers {
			wideById[p.ClientId] = p
		}
		assert.Equal(t, wideById[providerA].TransferDataLast24h, float64(7)) // 2 + 5 GiB
		assert.Equal(t, wideById[providerA].ContractsLast24h, 2)

		// ---- StatsProvider (single provider, last 72h) ----
		single, err := StatsProvider(&StatsProviderArgs{ClientId: providerA, LastN: 72}, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, statsSumFloat(single.TransferData), float64(7))
		assert.Equal(t, statsSumInt(single.Contracts), 2)
		assert.Equal(t, statsSumInt(single.Clients), 2) // one contract per day, distinct-per-day
		assert.Equal(t, statsSumFloat(single.Payout), 3.0)
		assert.Equal(t, statsSumInt(single.SearchInterest), 7)
		// per-served-client breakdown: the single customer
		assert.Equal(t, len(single.ClientDetails), 1)
		assert.Equal(t, single.ClientDetails[0].ClientId, customerA)
		assert.Equal(t, statsSumFloat(single.ClientDetails[0].TransferData), float64(7))
		// uptime is bucketed hourly in minutes; ~3h total
		uptimeMinutes := statsSumFloat(single.Uptime)
		if uptimeMinutes < 175 || 190 < uptimeMinutes {
			t.Fatalf("providerA uptime expected ~180min, got %f", uptimeMinutes)
		}

		// ---- ownership: a client outside the caller network is rejected with 403 ----
		foreign := server.NewId()
		_, err = StatsProvider(&StatsProviderArgs{ClientId: foreign, LastN: 72}, clientSession)
		assert.NotEqual(t, err, nil)
		assert.Equal(t, strings.HasPrefix(err.Error(), "403 "), true)

		// ---- overview: network aggregate ----
		overview, err := StatsProvidersOverview(&StatsProvidersOverviewArgs{LastN: 72}, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, statsSumFloat(overview.TransferData), float64(7))
		assert.Equal(t, statsSumInt(overview.Contracts), 2)
		assert.Equal(t, statsSumFloat(overview.Payout), 3.0)

		// ---- legacy default-90 entry points still work ----
		_, err = StatsProviderLast90(&StatsProviderArgs{ClientId: providerA}, clientSession)
		assert.Equal(t, err, nil)
		_, err = StatsProvidersOverviewLast90(clientSession)
		assert.Equal(t, err, nil)
	})
}

// TestSearchProviderStatsRollup exercises the redis-counter -> rollup-task ->
// pg path that backs the search_interest metric.
func TestSearchProviderStatsRollup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		clientId := server.NewId()
		// record matches in a fully-elapsed prior hour so the rollup finalizes it
		priorHour := now.Add(-90 * time.Minute)
		RecordProviderSearchMatches(ctx, []server.Id{clientId}, priorHour)
		RecordProviderSearchMatches(ctx, []server.Id{clientId}, priorHour)
		RecordProviderSearchMatches(ctx, []server.Id{clientId}, priorHour)

		// before rollup: nothing in pg yet
		assert.Equal(t, statsGetSearchMatchCount(ctx, clientId), int64(0))

		RollupSearchProviderStats(ctx, now)

		// after rollup: 3 matches persisted, and the elapsed redis bucket is drained
		assert.Equal(t, statsGetSearchMatchCount(ctx, clientId), int64(3))

		// idempotent: a second rollup does not double-count
		RollupSearchProviderStats(ctx, now)
		assert.Equal(t, statsGetSearchMatchCount(ctx, clientId), int64(3))
	})
}

// TestRemoveOldSearchProviderStats verifies the retention task deletes aged
// rollup rows and keeps rows within the retention window.
func TestRemoveOldSearchProviderStats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		clientId := server.NewId()

		// one recent row (within retention) and one old row (past 30 days)
		statsInsertSearchStat(ctx, clientId, now.Add(-1*time.Hour).Truncate(time.Hour), 5)
		statsInsertSearchStat(ctx, clientId, now.Add(-40*24*time.Hour).Truncate(time.Hour), 9)
		assert.Equal(t, statsGetSearchMatchCount(ctx, clientId), int64(14))

		RemoveOldSearchProviderStats(ctx, now, 50000)

		// only the recent row survives
		assert.Equal(t, statsGetSearchMatchCount(ctx, clientId), int64(5))
	})
}

// ---- test seeding helpers (direct inserts for deterministic timestamps) ----

func statsInsertNetworkClient(ctx context.Context, networkId server.Id, clientId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
			clientId, networkId,
		))
	})
}

func statsInsertProvideKey(ctx context.Context, clientId server.Id, provideMode int) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO provide_key (client_id, provide_mode, secret_key) VALUES ($1, $2, $3)`,
			clientId, provideMode, []byte{0x01},
		))
	})
}

func statsInsertContract(
	ctx context.Context,
	contractId, sourceNetworkId, sourceId, destNetworkId, destId server.Id,
	byteCount int64,
	createTime, closeTime time.Time,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id,
				destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time, outcome
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'success')
			`,
			contractId, sourceNetworkId, sourceId, destNetworkId, destId,
			byteCount, createTime.UTC(), closeTime.UTC(),
		))
	})
}

func statsInsertContractClose(ctx context.Context, contractId server.Id, usedByteCount int64, closeTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
			VALUES ($1, $2, 'destination', $3)
			`,
			contractId, closeTime.UTC(), usedByteCount,
		))
	})
}

func statsInsertSweep(
	ctx context.Context,
	contractId, networkId server.Id,
	payoutByteCount, payoutNanoCents int64,
	sweepTime time.Time,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_escrow_sweep (
				contract_id, balance_id, network_id,
				payout_byte_count, payout_net_revenue_nano_cents, sweep_time
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			`,
			contractId, server.NewId(), networkId, payoutByteCount, payoutNanoCents, sweepTime.UTC(),
		))
	})
}

func statsInsertConnection(ctx context.Context, clientId server.Id, connectTime time.Time, disconnectTime *time.Time, connected bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		var disconnect any
		if disconnectTime != nil {
			disconnect = disconnectTime.UTC()
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_client_connection (
				client_id, connection_id, connected, connect_time, disconnect_time,
				connection_host, connection_service, connection_block
			)
			VALUES ($1, $2, $3, $4, $5, 'h', 's', 'b')
			`,
			clientId, server.NewId(), connected, connectTime.UTC(), disconnect,
		))
	})
}

func statsInsertSearchStat(ctx context.Context, clientId server.Id, periodStart time.Time, matchCount int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO search_provider_stats (period_start, period_end, client_id, match_count)
			VALUES ($1, $2, $3, $4)
			`,
			periodStart.UTC(), periodStart.Add(time.Hour).UTC(), clientId, matchCount,
		))
	})
}

func statsGetSearchMatchCount(ctx context.Context, clientId server.Id) int64 {
	var total int64
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COALESCE(SUM(match_count), 0) FROM search_provider_stats WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&total))
			}
		})
	})
	return total
}

func statsSumFloat(m map[string]float64) float64 {
	var total float64
	for _, v := range m {
		total += v
	}
	return total
}

func statsSumInt(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}
