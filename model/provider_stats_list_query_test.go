package model

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The provider list's statements before 2026-11-18, kept as the result
// controls of the statements it runs now.
const (
	// One contract_close primary-key descent per eligible contract.
	providerStatsTransferBytesLateralBeforeSQL = `
	WITH eligible AS MATERIALIZED (
		SELECT contract_id, destination_id
		FROM transfer_contract
		WHERE destination_id = ANY($1::uuid[]) AND close_time >= $2
	)
	SELECT tc.destination_id, COALESCE(SUM(cc.used_transfer_byte_count), 0)
	FROM eligible tc
	CROSS JOIN LATERAL (
		SELECT used_transfer_byte_count
		FROM contract_close cc
		WHERE cc.contract_id = tc.contract_id AND cc.party = 'destination'
		OFFSET 0
	) cc
	GROUP BY tc.destination_id
`
	providerStatsContractsBeforeSQL = `
	SELECT destination_id, COUNT(*), COUNT(DISTINCT source_id)
	FROM transfer_contract
	WHERE destination_id = ANY($1::uuid[]) AND create_time >= $2
	GROUP BY destination_id
`
)

type providerStatsListFixture struct {
	network     server.Id
	providers   []server.Id // every provider of the network, the last one idle
	foreign     server.Id   // a provider of another network
	windowStart time.Time
}

// Many contracts per provider around the window start, in both directions of
// every boundary: closed before, at and after it, still open, canceled; with
// destination reports, source reports only, both, none, zero bytes and
// checkpoints; repeated and distinct sources; a foreign provider's contracts
// and reports without a contract. Most contract ids lead with their creation
// millisecond, so many share an id range; some are random.
func seedProviderStatsListFixture(t testing.TB, ctx context.Context, conn server.PgConn, contractsPerProvider int) *providerStatsListFixture {
	t.Helper()
	fixture := &providerStatsListFixture{
		network:     server.NewId(),
		foreign:     server.NewId(),
		windowStart: time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC),
	}
	for range 6 {
		fixture.providers = append(fixture.providers, server.NewId())
	}
	active := fixture.providers[:5]
	exec := func(sql string, args ...any) {
		server.RaisePgResult(conn.Exec(ctx, sql, args...))
	}
	exec(`
		INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id,
			transfer_byte_count, create_time, close_time, outcome)
		SELECT
			-- most ids lead with their creation millisecond, as connect.NewId's
			-- do, and group several to a key range; one in nine is random
			CASE WHEN g % 9 = 0 THEN md5('provider-stats-list-' || g)::uuid
				ELSE (lpad(to_hex((extract(epoch FROM created) * 1000)::bigint), 12, '0')
					|| substr(md5('provider-stats-list-' || g), 1, 20))::uuid END,
			md5('source-network-' || g % 3)::uuid,
			md5('source-' || (g * 7) % 41)::uuid,
			$2::uuid,
			CASE WHEN g % 23 = 0 THEN $3::uuid ELSE ($1::uuid[])[1 + g % array_length($1::uuid[], 1)] END,
			1000000,
			created,
			CASE WHEN g % 11 = 0 THEN NULL
				WHEN g % 29 = 0 THEN $4::timestamp
				WHEN g % 31 = 0 THEN $4::timestamp - interval '1 microsecond'
				ELSE created + (g % 13) * interval '7 minutes' END,
			CASE WHEN g % 11 = 0 THEN NULL WHEN g % 17 = 0 THEN 'canceled' ELSE 'success' END
		FROM (
			SELECT g,
				CASE WHEN g % 37 = 0 THEN $4::timestamp
					WHEN g % 41 = 0 THEN $4::timestamp - interval '1 microsecond'
					ELSE $4::timestamp + ((g % 193) - 96) * interval '15 minutes' END AS created
			FROM generate_series(1, $5::integer) g
		) contract`,
		idStrings(active), fixture.network, fixture.foreign, fixture.windowStart, contractsPerProvider*len(active))
	exec(`
		INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count, checkpoint)
		SELECT contract_id, COALESCE(close_time, create_time), party,
			CASE WHEN party = 'source' THEN 1000003 ELSE (hashint8(n)::bigint & 2147483647) % 5 * 1009 END,
			n % 19 = 0
		FROM (
			SELECT contract_id, close_time, create_time, p.party, row_number() OVER (ORDER BY contract_id, p.party) AS n
			FROM transfer_contract CROSS JOIN (VALUES ('source'), ('destination')) p(party)
			WHERE destination_network_id = $1
		) report
		WHERE (party = 'destination' AND n % 7 <> 0) OR (party = 'source' AND n % 5 <> 0)`,
		fixture.network)
	exec(`
		INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
		SELECT md5('provider-stats-orphan-' || g)::uuid, $1::timestamp, 'destination', 999983
		FROM generate_series(1, 50) g`, fixture.windowStart)
	exec(`ANALYZE transfer_contract, contract_close`)
	return fixture
}

// A pooled connection or a transaction.
type providerStatsListQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func providerStatsListTransferBytes(t testing.TB, ctx context.Context, q providerStatsListQuerier, sql string, ids []string, start time.Time, args ...any) map[server.Id]int64 {
	t.Helper()
	amounts := map[server.Id]int64{}
	rows, err := q.Query(ctx, sql, append(args, ids, start)...)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var id server.Id
			var amount int64
			server.Raise(rows.Scan(&id, &amount))
			if _, exists := amounts[id]; exists {
				t.Fatal("provider returned twice")
			}
			amounts[id] = amount
		}
	})
	return amounts
}

type providerStatsListCounts struct {
	contracts int
	clients   int
}

func providerStatsListContracts(t testing.TB, ctx context.Context, q providerStatsListQuerier, sql string, ids []string, start time.Time, args ...any) map[server.Id]providerStatsListCounts {
	t.Helper()
	counts := map[server.Id]providerStatsListCounts{}
	rows, err := q.Query(ctx, sql, append(args, ids, start)...)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var id server.Id
			var count providerStatsListCounts
			server.Raise(rows.Scan(&id, &count.contracts, &count.clients))
			if _, exists := counts[id]; exists {
				t.Fatal("provider returned twice")
			}
			counts[id] = count
		}
	})
	return counts
}

// The provider list's transfer bytes, read by key range since 2026-11-18,
// answer exactly what the per-contract lookups answered, and its contract
// counts what they answered, for every provider selection, under custom and
// generic plans, on a fixture with many contracts and reports per provider
// across every window boundary.
func TestProviderStatsListQueriesMatchBeforeQueries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			fixture := seedProviderStatsListFixture(t, ctx, conn, 400)
			p := fixture.providers
			missing := server.NewId()
			selections := []struct {
				name string
				ids  []server.Id
			}{
				{"empty", []server.Id{}},
				{"one", p[:1]},
				{"duplicate", []server.Id{p[0], p[0], p[1]}},
				{"network", p},
				{"network-and-foreign", append(append([]server.Id{}, p...), fixture.foreign)},
				{"idle", p[5:]},
				{"missing", []server.Id{missing}},
			}
			starts := []time.Time{
				fixture.windowStart,
				fixture.windowStart.Add(-time.Microsecond),
				fixture.windowStart.Add(time.Microsecond),
				fixture.windowStart.Add(-36 * time.Hour),
				fixture.windowStart.Add(36 * time.Hour),
			}
			checked := 0
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				for _, selection := range selections {
					for _, start := range starts {
						func() {
							tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
							server.Raise(err)
							defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
							server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
							ids := idStrings(selection.ids)
							name := fmt.Sprintf("%s/%s/%s", mode, selection.name, start.Format(time.RFC3339Nano))
							wantBytes := providerStatsListTransferBytes(t, ctx, tx, providerStatsTransferBytesLateralBeforeSQL, ids, start)
							if got := providerStatsListTransferBytes(t, ctx, tx, providerStatsTransferBytesSQL, ids, start); !reflect.DeepEqual(got, wantBytes) {
								t.Fatalf("%s: transfer bytes changed: got=%v want=%v", name, got, wantBytes)
							}
							wantCounts := providerStatsListContracts(t, ctx, tx, providerStatsContractsBeforeSQL, ids, start)
							if got := providerStatsListContracts(t, ctx, tx, providerStatsContractsSQL, ids, start); !reflect.DeepEqual(got, wantCounts) {
								t.Fatalf("%s: contracts and clients changed: got=%v want=%v", name, got, wantCounts)
							}
							if selection.name == "network" && start.Equal(fixture.windowStart) {
								// the fixture exercises what it claims
								nonzero, multiple := 0, 0
								for _, provider := range p[:5] {
									if 0 < wantBytes[provider] {
										nonzero++
									}
									if 1 < wantCounts[provider].clients && wantCounts[provider].clients < wantCounts[provider].contracts {
										multiple++
									}
								}
								if nonzero != 5 || multiple != 5 || 0 < wantBytes[p[5]] {
									t.Fatalf("fixture lost its shape: bytes=%v counts=%v", wantBytes, wantCounts)
								}
							}
							checked++
						}()
					}
				}
			}
			if checked != 2*len(selections)*len(starts) {
				t.Fatalf("checked %d selections", checked)
			}
		})
	})
}

// A large network's provider-list fill passed its 2-minute budget
// (2026-11-18) partly because its reads ran in sequence: the payout
// attribution, tens of seconds for the largest networks, waited for the
// contract reads, and the remaining reads waited for it. The attribution
// now runs on its own connection while the provider reads run. Here it waits
// on a lock the test holds on the sweeps, and every provider read, the last
// of them the connection intervals, still finishes while the lock is held;
// the list is answered, payout included, once the lock is released.
func TestStatsProvidersPayoutReadOverlapsProviderReads(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		now := server.NowUtc()
		networkId := server.NewId()
		clientSession := providerPayoutTestSession(ctx, networkId)
		provider := server.NewId()
		statsInsertNetworkClient(ctx, networkId, provider)
		statsInsertProvideKey(ctx, provider, ProvideModePublic)
		sourceNetwork, source := server.NewId(), server.NewId()
		contract := server.NewId()
		statsInsertContract(ctx, contract, sourceNetwork, source, networkId, provider, 2*gib, now.Add(-2*time.Hour), now.Add(-time.Hour))
		statsInsertContractClose(ctx, contract, 2*gib, now.Add(-time.Hour))
		statsInsertSweep(ctx, contract, networkId, provider, 2*gib, UsdToNanoCents(3.0), now.Add(-time.Hour))
		statsInsertConnection(ctx, provider, now.Add(-3*time.Hour), nil, true)

		locked, release, unlocked := make(chan time.Time), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(unlocked)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `LOCK TABLE transfer_escrow_sweep IN ACCESS EXCLUSIVE MODE`))
				var lockTime time.Time
				server.Raise(tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&lockTime))
				locked <- lockTime
				<-release
			}, server.OptReadWrite())
		}()
		lockTime := <-locked
		releaseOnce := sync.OnceFunc(func() { close(release) })

		type listResult struct {
			result *StatsProvidersResult
			err    error
			raised any
		}
		answered := make(chan listResult, 1)
		go func() {
			var answer listResult
			defer func() {
				answer.raised = recover()
				answered <- answer
			}()
			answer.result, answer.err = statsProviders(clientSession, now.Add(-24*time.Hour), now)
		}()
		// The list and the lock are joined on every exit, a failure included.
		var answer listResult
		joined := false
		join := func() {
			releaseOnce()
			if !joined {
				answer = <-answered
				joined = true
			}
			<-unlocked
		}
		defer join()

		// Wait until the attribution waits on the lock and the provider reads
		// have finished behind it, or the list is answered without it. The
		// reads take milliseconds here; 30 s is a wide margin, not a timing
		// assertion.
		overlapDeadline := time.After(30 * time.Second)
		overlapped := false
		for !overlapped {
			select {
			case answer = <-answered:
				joined = true
				t.Fatalf("list answered while the sweeps were locked: %v %v %v", answer.result, answer.err, answer.raised)
			case <-overlapDeadline:
				t.Fatal("provider reads did not finish while the payout attribution waited on its lock: they waited for it")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
			// Poll from the maintenance pool, so the poll never borrows the
			// provider reads' released connection and replaces its last query.
			// The poll's own text names both statements; its sessions are not them.
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `
					SELECT
						EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
							AND state = 'active' AND wait_event_type = 'Lock' AND query LIKE '%payout_sweeps%'
							AND query NOT LIKE '%pg_stat_activity%')
						AND EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
							AND state = 'idle' AND query LIKE '%FROM network_client_connection%connect_time <= $3%'
							AND query NOT LIKE '%pg_stat_activity%' AND $1 <= query_start)`,
					lockTime,
				).Scan(&overlapped))
			})
		}
		join()
		if answer.raised != nil || answer.err != nil || answer.result == nil || len(answer.result.Providers) != 1 {
			t.Fatalf("list = (%v, %v) raised=%v", answer.result, answer.err, answer.raised)
		}
		got := answer.result.Providers[0]
		if got.ClientId != provider || got.TransferDataLast24h != 2 || got.PayoutLast24h != 3.0 || got.ContractsLast24h != 1 || got.ClientsLast24h != 1 || !got.Connected {
			t.Fatalf("provider stats changed: %+v", got)
		}
	})
}
