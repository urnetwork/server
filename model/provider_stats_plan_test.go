package model

// provider_stats_plan_test.go — performance regression guard for the /stats
// provider query plans.
//
// It seeds realistic table volume, ANALYZEs, then EXPLAINs every query the
// stats APIs run and asserts none of them fall back to a sequential scan on a
// large table. This catches a dropped/renamed index or a query change that
// regresses a per-provider aggregate into a full-table scan.
//
// Why the seeding volume matters: on a near-empty table Postgres correctly
// prefers a seq scan (it is cheaper than an index for a handful of rows), so a
// plan assertion is only meaningful once the tables are large enough that the
// index plan wins — hence the background rows below.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

const (
	statsPlanProviders  = 200
	statsPlanProviderN  = 10000  // provider contracts / connections
	statsPlanBackground = 150000 // background rows per large table (other networks)
	statsPlanSearchBg   = 100000
)

// statsPlanGuardedTables must be reached by an index plan (index range scan,
// index seek, or bitmap index scan) by every stats query — never a Seq Scan.
// contract_close is intentionally excluded: it is reached by primary key in the
// per-provider queries, and via an acceptable hash join in the network-wide
// overview aggregate (a hash-join seq-scan of contract_close at test scale flips
// to a nested-loop + contract_close_pkey plan as the table grows).
var statsPlanGuardedTables = []string{
	"transfer_contract",
	"network_client_connection",
	"transfer_escrow_sweep",
	"search_provider_stats",
	"network_client",
	"provide_key",
}

func TestStatsQueryPlans(t *testing.T) {
	if testing.Short() {
		t.Skip("stats query-plan test seeds ~1M rows; skipped in -short")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		w24 := now.Add(-24 * time.Hour)
		w72 := now.Add(-72 * time.Hour)
		retentionMin := now.Add(-SearchStatsRetention)

		networkId := server.NewId()
		sourceNetworkId := server.NewId()

		providers := make([]server.Id, statsPlanProviders)
		for i := range providers {
			providers[i] = server.NewId()
		}
		providerId0 := providers[0]
		all := idStrings(providers)
		five := all[:5]

		server.Db(ctx, func(conn server.PgConn) {
			statsPlanSeed(ctx, conn, networkId, sourceNetworkId, all)

			assertStatsIndexPlan(t, ctx, conn, "01_enumerate_providers", `
				SELECT network_client.client_id
				FROM network_client INNER JOIN provide_key ON provide_key.client_id = network_client.client_id
				WHERE network_client.network_id = $1 AND network_client.active = true
				GROUP BY network_client.client_id`, networkId)

			assertStatsIndexPlan(t, ctx, conn, "02_transfer_list", `
				SELECT tc.destination_id, COALESCE(SUM(cc.used_transfer_byte_count), 0)
				FROM transfer_contract tc INNER JOIN contract_close cc ON cc.contract_id = tc.contract_id AND cc.party = 'destination'
				WHERE tc.destination_id = ANY($1::uuid[]) AND tc.close_time >= $2 GROUP BY tc.destination_id`, five, w24)

			assertStatsIndexPlan(t, ctx, conn, "03_contracts_clients_list", `
				SELECT destination_id, COUNT(*), COUNT(DISTINCT source_id)
				FROM transfer_contract WHERE destination_id = ANY($1::uuid[]) AND create_time >= $2 GROUP BY destination_id`, five, w24)

			assertStatsIndexPlan(t, ctx, conn, "04_payout_list", `
				SELECT tc.destination_id, COALESCE(SUM(s.payout_net_revenue_nano_cents), 0)
				FROM transfer_escrow_sweep s INNER JOIN transfer_contract tc ON tc.contract_id = s.contract_id
				WHERE tc.destination_id = ANY($1::uuid[]) AND s.sweep_time >= $2 GROUP BY tc.destination_id`, five, w24)

			assertStatsIndexPlan(t, ctx, conn, "05_search_list", `
				SELECT client_id, COALESCE(SUM(match_count), 0)
				FROM search_provider_stats WHERE client_id = ANY($1::uuid[]) AND period_start >= $2 GROUP BY client_id`, five, w24)

			assertStatsIndexPlan(t, ctx, conn, "06_connections_list", `
				SELECT client_id, connect_time, disconnect_time, connected
				FROM network_client_connection WHERE client_id = ANY($1::uuid[]) AND connect_time <= $3 AND (disconnect_time IS NULL OR disconnect_time >= $2)
				ORDER BY client_id, connect_time`, five, w24, now)

			assertStatsIndexPlan(t, ctx, conn, "07_ownership_exists", `
				SELECT EXISTS(SELECT 1 FROM network_client WHERE network_id = $1 AND client_id = $2)`, networkId, providerId0)

			assertStatsIndexPlan(t, ctx, conn, "08_transfer_day_single", `
				SELECT to_char(tc.close_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(cc.used_transfer_byte_count), 0)
				FROM transfer_contract tc INNER JOIN contract_close cc ON cc.contract_id = tc.contract_id AND cc.party = 'destination'
				WHERE tc.destination_id = $1 AND tc.close_time >= $2 GROUP BY day`, providerId0, w72)

			assertStatsIndexPlan(t, ctx, conn, "09_payout_day_single", `
				SELECT to_char(s.sweep_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(s.payout_net_revenue_nano_cents), 0)
				FROM transfer_escrow_sweep s INNER JOIN transfer_contract tc ON tc.contract_id = s.contract_id
				WHERE tc.destination_id = $1 AND s.sweep_time >= $2 GROUP BY day`, providerId0, w72)

			assertStatsIndexPlan(t, ctx, conn, "10_contracts_day_single", `
				SELECT to_char(create_time, 'YYYY-MM-DD') AS day, COUNT(*), COUNT(DISTINCT source_id)
				FROM transfer_contract WHERE destination_id = $1 AND create_time >= $2 GROUP BY day`, providerId0, w72)

			assertStatsIndexPlan(t, ctx, conn, "11_search_day_single", `
				SELECT to_char(period_start, 'YYYY-MM-DD') AS day, COALESCE(SUM(match_count), 0)
				FROM search_provider_stats WHERE client_id = $1 AND period_start >= $2 GROUP BY day`, providerId0, w72)

			assertStatsIndexPlan(t, ctx, conn, "12_client_details_single", `
				SELECT tc.source_id, to_char(tc.close_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(cc.used_transfer_byte_count), 0)
				FROM transfer_contract tc INNER JOIN contract_close cc ON cc.contract_id = tc.contract_id AND cc.party = 'destination'
				WHERE tc.destination_id = $1 AND tc.close_time >= $2 GROUP BY tc.source_id, day`, providerId0, w72)

			assertStatsIndexPlan(t, ctx, conn, "13_overview_transfer_day", `
				SELECT to_char(tc.close_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(cc.used_transfer_byte_count), 0)
				FROM transfer_contract tc INNER JOIN contract_close cc ON cc.contract_id = tc.contract_id AND cc.party = 'destination'
				WHERE tc.destination_id = ANY($1::uuid[]) AND tc.close_time >= $2 GROUP BY day`, all, w72)

			assertStatsIndexPlan(t, ctx, conn, "14_overview_connections", `
				SELECT client_id, connect_time, disconnect_time, connected
				FROM network_client_connection WHERE client_id = ANY($1::uuid[]) AND connect_time <= $3 AND (disconnect_time IS NULL OR disconnect_time >= $2)
				ORDER BY client_id, connect_time`, all, w72, now)

			assertStatsIndexPlan(t, ctx, conn, "15_retention_delete", `
				DELETE FROM search_provider_stats USING (
				    SELECT period_start, client_id FROM search_provider_stats WHERE period_start < $1 ORDER BY period_start LIMIT $2
				) t WHERE search_provider_stats.period_start = t.period_start AND search_provider_stats.client_id = t.client_id`, retentionMin, 50000)
		})
	})
}

// assertStatsIndexPlan EXPLAINs the query and fails if any guarded large table
// is reached by a Seq Scan. The full plan is logged only on regression.
func assertStatsIndexPlan(t testing.TB, ctx context.Context, conn server.PgConn, name, sql string, args ...any) {
	rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
	lines := []string{}
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var line string
			server.Raise(rows.Scan(&line))
			lines = append(lines, line)
		}
	})
	plan := strings.Join(lines, "\n")
	for _, table := range statsPlanGuardedTables {
		if strings.Contains(plan, "Seq Scan on "+table) {
			t.Errorf("stats query %q uses a Seq Scan on %q (expected an index plan):\n%s", name, table, plan)
			return
		}
	}
	t.Logf("[plan ok] %s", name)
}

func statsPlanSeed(ctx context.Context, conn server.PgConn, networkId, sourceNetworkId server.Id, providers []string) {
	exec := func(sql string, args ...any) {
		server.RaisePgResult(conn.Exec(ctx, sql, args...))
	}

	// the caller network's providers, plus background clients on other networks
	exec(`INSERT INTO network_client (client_id, network_id, active) SELECT p, $1, true FROM unnest($2::uuid[]) p`, networkId, providers)
	exec(`INSERT INTO provide_key (client_id, provide_mode, secret_key) SELECT p, 3, '\x01'::bytea FROM unnest($1::uuid[]) p`, providers)
	exec(`INSERT INTO network_client (client_id, network_id, active) SELECT gen_random_uuid(), gen_random_uuid(), true FROM generate_series(1, $1) g`, statsPlanBackground)
	exec(`INSERT INTO provide_key (client_id, provide_mode, secret_key) SELECT gen_random_uuid(), 3, '\x01'::bytea FROM generate_series(1, $1) g`, statsPlanBackground)

	// transfer_contract: provider (spread across providers, last 240h) + background
	exec(`
		INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count, create_time, close_time, outcome)
		SELECT gen_random_uuid(), $1, gen_random_uuid(), $2, ($3::uuid[])[1 + (g % array_length($3::uuid[],1))], 1000000000,
		       now() - ((g % 240) || ' hours')::interval, now() - ((g % 240) || ' hours')::interval, 'success'
		FROM generate_series(1, $4) g`, sourceNetworkId, networkId, providers, statsPlanProviderN)
	exec(`
		INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count, create_time, close_time, outcome)
		SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 1000000000,
		       now() - ((g % 500) || ' hours')::interval, now() - ((g % 500) || ' hours')::interval, 'success'
		FROM generate_series(1, $1) g`, statsPlanBackground)

	// contract_close: provider closes (join real contracts) + background
	exec(`
		INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
		SELECT contract_id, close_time, 'destination', transfer_byte_count
		FROM transfer_contract WHERE destination_id = ANY($1::uuid[]) ON CONFLICT DO NOTHING`, providers)
	exec(`
		INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
		SELECT gen_random_uuid(), now() - ((g % 500) || ' hours')::interval, 'destination', 1000000000
		FROM generate_series(1, $1) g ON CONFLICT DO NOTHING`, statsPlanBackground)

	// transfer_escrow_sweep: provider sweeps + background
	exec(`
		INSERT INTO transfer_escrow_sweep (contract_id, balance_id, network_id, payout_byte_count, payout_net_revenue_nano_cents, sweep_time)
		SELECT contract_id, gen_random_uuid(), $2, transfer_byte_count, 1000000000, close_time
		FROM transfer_contract WHERE destination_id = ANY($1::uuid[]) ON CONFLICT DO NOTHING`, providers, networkId)
	exec(`
		INSERT INTO transfer_escrow_sweep (contract_id, balance_id, network_id, payout_byte_count, payout_net_revenue_nano_cents, sweep_time)
		SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 1000000000, 1000000000, now() - ((g % 500) || ' hours')::interval
		FROM generate_series(1, $1) g ON CONFLICT DO NOTHING`, statsPlanBackground)

	// network_client_connection: provider connections + background
	exec(`
		INSERT INTO network_client_connection (client_id, connection_id, connected, connect_time, disconnect_time, connection_host, connection_service, connection_block)
		SELECT ($1::uuid[])[1 + (g % array_length($1::uuid[],1))], gen_random_uuid(), true, now() - ((g % 240) || ' hours')::interval, NULL, 'h','s','b'
		FROM generate_series(1, $2) g`, providers, statsPlanProviderN)
	exec(`
		INSERT INTO network_client_connection (client_id, connection_id, connected, connect_time, disconnect_time, connection_host, connection_service, connection_block)
		SELECT gen_random_uuid(), gen_random_uuid(), true, now() - ((g % 500) || ' hours')::interval, NULL, 'h','s','b'
		FROM generate_series(1, $1) g`, statsPlanBackground)

	// search_provider_stats: provider rows (200 x 48h) + background
	exec(`
		INSERT INTO search_provider_stats (period_start, period_end, client_id, match_count)
		SELECT date_trunc('hour', now()) - (h || ' hours')::interval, date_trunc('hour', now()) - (h || ' hours')::interval + interval '1 hour', p, h
		FROM unnest($1::uuid[]) p, generate_series(1, 48) h ON CONFLICT DO NOTHING`, providers)
	exec(`
		INSERT INTO search_provider_stats (period_start, period_end, client_id, match_count)
		SELECT date_trunc('hour', now()) - ((g % 500) || ' hours')::interval, date_trunc('hour', now()) - ((g % 500) || ' hours')::interval + interval '1 hour', gen_random_uuid(), 1
		FROM generate_series(1, $1) g ON CONFLICT DO NOTHING`, statsPlanSearchBg)

	exec(`ANALYZE network_client, provide_key, transfer_contract, contract_close, transfer_escrow_sweep, network_client_connection, search_provider_stats`)
}
