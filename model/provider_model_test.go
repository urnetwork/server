package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

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
		statsInsertSweep(ctx, recentContract, networkId, providerA, 2*gib, UsdToNanoCents(3.0), now.Add(-1*time.Hour))

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Providers), 2)

		byId := map[server.Id]*ProviderStats{}
		for _, p := range result.Providers {
			byId[p.ClientId] = p
		}

		a := byId[providerA]
		connect.AssertNotEqual(t, a, nil)
		connect.AssertEqual(t, a.TransferDataLast24h, float64(2)) // 2 GiB (old 5 GiB excluded)
		connect.AssertEqual(t, a.ContractsLast24h, 1)
		connect.AssertEqual(t, a.ClientsLast24h, 1)
		connect.AssertEqual(t, a.PayoutLast24h, 3.0)
		connect.AssertEqual(t, a.SearchInterestLast24h, 7)
		connect.AssertEqual(t, a.Connected, true)
		if a.UptimeLast24h < 2.9 || 3.2 < a.UptimeLast24h {
			t.Fatalf("providerA uptime expected ~3h, got %f", a.UptimeLast24h)
		}

		b := byId[providerB]
		connect.AssertNotEqual(t, b, nil)
		connect.AssertEqual(t, b.TransferDataLast24h, float64(0))
		connect.AssertEqual(t, b.ContractsLast24h, 0)
		connect.AssertEqual(t, b.Connected, false)
		// arrays must serialize as [], never null
		connect.AssertNotEqual(t, b.ConnectedEventsLast24h, nil)

		// ---- StatsProvidersLastN (last 72h) includes the old contract ----
		wide, err := StatsProvidersLastN(&StatsProvidersArgs{LastN: 72}, clientSession)
		connect.AssertEqual(t, err, nil)
		wideById := map[server.Id]*ProviderStats{}
		for _, p := range wide.Providers {
			wideById[p.ClientId] = p
		}
		connect.AssertEqual(t, wideById[providerA].TransferDataLast24h, float64(7)) // 2 + 5 GiB
		connect.AssertEqual(t, wideById[providerA].ContractsLast24h, 2)

		// ---- StatsProvider (single provider, last 72h) ----
		single, err := StatsProvider(&StatsProviderArgs{ClientId: providerA, LastN: 72}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, statsSumFloat(single.TransferData), float64(7))
		connect.AssertEqual(t, statsSumInt(single.Contracts), 2)
		connect.AssertEqual(t, statsSumInt(single.Clients), 2) // one contract per day, distinct-per-day
		connect.AssertEqual(t, statsSumFloat(single.Payout), 3.0)
		connect.AssertEqual(t, statsSumInt(single.SearchInterest), 7)
		// per-served-client breakdown: the single customer
		connect.AssertEqual(t, len(single.ClientDetails), 1)
		connect.AssertEqual(t, single.ClientDetails[0].ClientId, customerA)
		connect.AssertEqual(t, statsSumFloat(single.ClientDetails[0].TransferData), float64(7))
		// uptime is bucketed hourly in minutes; ~3h total
		uptimeMinutes := statsSumFloat(single.Uptime)
		if uptimeMinutes < 175 || 190 < uptimeMinutes {
			t.Fatalf("providerA uptime expected ~180min, got %f", uptimeMinutes)
		}

		// ---- ownership: a client outside the caller network is rejected with 403 ----
		foreign := server.NewId()
		_, err = StatsProvider(&StatsProviderArgs{ClientId: foreign, LastN: 72}, clientSession)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, strings.HasPrefix(err.Error(), "403 "), true)

		// ---- overview: network aggregate ----
		overview, err := StatsProvidersOverview(&StatsProvidersOverviewArgs{LastN: 72}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, statsSumFloat(overview.TransferData), float64(7))
		connect.AssertEqual(t, statsSumInt(overview.Contracts), 2)
		connect.AssertEqual(t, statsSumFloat(overview.Payout), 3.0)

		// ---- legacy default-90 entry points still work ----
		_, err = StatsProviderLast90(&StatsProviderArgs{ClientId: providerA}, clientSession)
		connect.AssertEqual(t, err, nil)
		_, err = StatsProvidersOverviewLast90(clientSession)
		connect.AssertEqual(t, err, nil)
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
		connect.AssertEqual(t, statsGetSearchMatchCount(ctx, clientId), int64(0))

		RollupSearchProviderStats(ctx, now)

		// after rollup: 3 matches persisted, and the elapsed redis bucket is drained
		connect.AssertEqual(t, statsGetSearchMatchCount(ctx, clientId), int64(3))

		// idempotent: a second rollup does not double-count
		RollupSearchProviderStats(ctx, now)
		connect.AssertEqual(t, statsGetSearchMatchCount(ctx, clientId), int64(3))
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
		connect.AssertEqual(t, statsGetSearchMatchCount(ctx, clientId), int64(14))

		RemoveOldSearchProviderStats(ctx, now, 50000)

		// only the recent row survives
		connect.AssertEqual(t, statsGetSearchMatchCount(ctx, clientId), int64(5))
	})
}

func TestRemoveOldVerifyProviderStats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		clientId := server.NewId()

		// one recent row (within retention) and one old row (past 30 days)
		statsInsertVerifyStat(ctx, clientId, now.Add(-1*time.Hour).Truncate(time.Hour))
		statsInsertVerifyStat(ctx, clientId, now.Add(-40*24*time.Hour).Truncate(time.Hour))
		connect.AssertEqual(t, statsGetVerifyRowCount(ctx, clientId), 2)

		RemoveOldVerifyProviderStats(ctx, now, 50000)

		// only the recent row survives
		connect.AssertEqual(t, statsGetVerifyRowCount(ctx, clientId), 1)
	})
}

func statsInsertVerifyStat(ctx context.Context, clientId server.Id, periodStart time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO verify_provider_stats (period_start, period_end, client_id, assignments, confirmations) VALUES ($1, $2, $3, $4, $5)`,
			periodStart, periodStart.Add(time.Hour), clientId, int64(10), int64(8),
		))
	})
}

func statsGetVerifyRowCount(ctx context.Context, clientId server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM verify_provider_stats WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
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
	contractId, networkId, destId server.Id,
	payoutByteCount, payoutNanoCents int64,
	sweepTime time.Time,
) {
	// destination_id is denormalized onto the sweep at settle time (the provider
	// payout stats query filters `s.destination_id = ANY($ids)`), so seed it here
	// to mirror a real sweep — otherwise the payout for this provider is 0.
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_escrow_sweep (
				contract_id, balance_id, network_id, destination_id,
				payout_byte_count, payout_net_revenue_nano_cents, sweep_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
			contractId, server.NewId(), networkId, destId, payoutByteCount, payoutNanoCents, sweepTime.UTC(),
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

// Exercises the provider-enumeration query in StatsProviders (network_client
// semi-join provide_key). Guards the JOIN + GROUP BY -> EXISTS rewrite: a
// client with multiple provide_keys must appear exactly once, an active client
// with no provide_key must be excluded, and an inactive client that has one
// must be excluded.
func TestStatsProvidersProviderEnumeration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "enum-net", userId)
		clientSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkId, userId, "enum-net", false, false),
		)

		// active provider with two provide_keys: must appear exactly once
		p1 := server.NewId()
		statsInsertNetworkClient(ctx, networkId, p1)
		statsInsertProvideKey(ctx, p1, ProvideModePublic)
		statsInsertProvideKey(ctx, p1, ProvideModeNetwork)

		// active provider with one provide_key: appears
		p2 := server.NewId()
		statsInsertNetworkClient(ctx, networkId, p2)
		statsInsertProvideKey(ctx, p2, ProvideModePublic)

		// active client with no provide_key: excluded (not a provider)
		p3 := server.NewId()
		statsInsertNetworkClient(ctx, networkId, p3)

		// inactive client with a provide_key: excluded (active = false)
		p4 := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, false)`,
				p4, networkId,
			))
		})
		statsInsertProvideKey(ctx, p4, ProvideModePublic)

		result, err := StatsProviders(clientSession)
		connect.AssertEqual(t, err, nil)

		seen := map[server.Id]int{}
		for _, p := range result.Providers {
			seen[p.ClientId] += 1
		}

		// exactly the two active providers, each once (p1's second provide_key
		// must not duplicate it)
		connect.AssertEqual(t, len(result.Providers), 2)
		connect.AssertEqual(t, seen[p1], 1)
		connect.AssertEqual(t, seen[p2], 1)
		connect.AssertEqual(t, seen[p3], 0)
		connect.AssertEqual(t, seen[p4], 0)
	})
}

// StatsProvidersOverview aggregates a network's providers into daily buckets. A
// provider holding multiple provide_keys must be counted once, so its transfer /
// payout / contracts are not multiplied. The overview enumeration query is a
// DISTINCT semijoin; this is a regression guard on that single-count behavior.
// (The overview aggregate is already dup-immune -- it filters `= ANY($ids)` and
// GROUPs BY day, so a duplicated id matches each row once -- so this passes even
// without the DISTINCT; it guards against a future refactor that makes the
// overview iterate the id list the way statsProviders does.)
func TestStatsProvidersOverviewDedupsMultiProvideKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		gib := int64(1024 * 1024 * 1024)

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "overview-multikey-net", userId)
		clientSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkId, userId, "overview-multikey-net", false, false),
		)

		// one provider client holding TWO provide_keys
		provider := server.NewId()
		statsInsertNetworkClient(ctx, networkId, provider)
		statsInsertProvideKey(ctx, provider, ProvideModePublic)
		statsInsertProvideKey(ctx, provider, ProvideModeNetwork)

		// a single settled contract in the last 24h: 2 GiB served, $3 payout
		customerNetworkId := server.NewId()
		customerA := server.NewId()
		contractId := server.NewId()
		statsInsertContract(ctx, contractId, customerNetworkId, customerA, networkId, provider, 2*gib, now.Add(-2*time.Hour), now.Add(-1*time.Hour))
		statsInsertContractClose(ctx, contractId, 2*gib, now.Add(-1*time.Hour))
		statsInsertSweep(ctx, contractId, networkId, provider, 2*gib, UsdToNanoCents(3.0), now.Add(-1*time.Hour))

		overview, err := StatsProvidersOverview(&StatsProvidersOverviewArgs{LastN: 72}, clientSession)
		connect.AssertEqual(t, err, nil)
		// exactly one contract's worth -- the two provide_keys must not multiply it
		connect.AssertEqual(t, statsSumInt(overview.Contracts), 1)
		connect.AssertEqual(t, statsSumFloat(overview.TransferData), float64(2))
		connect.AssertEqual(t, statsSumFloat(overview.Payout), 3.0)
	})
}
