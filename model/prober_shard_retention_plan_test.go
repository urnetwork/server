package model

// The prober shard retention's plan proofs (OPEN-SOURCE-ID-RETIRE.md, option
// A). The drop read each client's closed contracts as a range of
// transfer_contract_open_source_id (open, source_id, destination_id,
// contract_id), the reader that kept the 323 GiB index on Main. It now walks
// the primary key over a generation's contract id range, so with or without
// that index every read of transfer_contract is a range or the batch's ids of
// transfer_contract_pkey. Each test seeds a pass's closed contracts among other
// payers' and plans the real statements with sequential scans disabled, in a
// transaction that rolls back (planIndexDietStatement).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The window read the walk replaced, as the drop ran it until 2026-11-16: the
// origins of the pass's clients from the closed range of each client in
// transfer_contract_open_source_id, and the reply companions to them. $1 the
// clients, $2 the pass's network, $3 the cutoff, $4 the cursor, $5 the batch
// size.
const legacyProberShardRetentionContractWindowSql = `
	WITH client AS MATERIALIZED (
		SELECT DISTINCT client_id FROM unnest($1::uuid[]) AS client(client_id)
	), contract AS MATERIALIZED (
		SELECT origin.contract_id
		FROM client
		CROSS JOIN LATERAL (
			SELECT contract_id, payer_network_id, outcome, close_time
			FROM transfer_contract
			WHERE open = false AND source_id = client.client_id
			OFFSET 0
		) AS origin
		WHERE
			origin.payer_network_id = $2 AND
			origin.outcome IS NOT NULL AND
			origin.close_time < $3 AND
			$4 < origin.contract_id
		UNION
		SELECT companion.contract_id
		FROM client
		CROSS JOIN LATERAL (
			SELECT contract_id, payer_network_id, outcome, close_time
			FROM transfer_contract
			WHERE destination_id = client.client_id
			OFFSET 0
		) AS companion
		WHERE
			companion.payer_network_id = $2 AND
			companion.outcome IS NOT NULL AND
			companion.close_time < $3 AND
			$4 < companion.contract_id
	)
	SELECT ARRAY(SELECT contract_id FROM contract ORDER BY contract_id LIMIT $5)
`

// A pass's closed contracts among other payers', and the planner arguments of
// a walk over the hour before now: the pass's network and clients, a day's
// pass networks with the pass's among them, the walk's range and the cutoff.
type proberRetentionPlanFixture struct {
	networkId  server.Id
	clientIds  []server.Id
	networkIds []server.Id
	lower      server.Id
	upper      server.Id
	cutoff     time.Time
}

// The pass's two clients' origins to many providers and the providers' reply
// companions to them, at the start of the hour so the walk's first window
// holds them, and five times as many contracts of other payers after them.
func newProberRetentionPlanFixture(t testing.TB, ctx context.Context) *proberRetentionPlanFixture {
	t.Helper()
	now := server.NowUtc()
	fixture := &proberRetentionPlanFixture{
		networkId: server.NewId(),
		clientIds: []server.Id{server.NewId(), server.NewId()},
		cutoff:    now.Add(24 * time.Hour),
	}
	passEnd := now.Add(-40 * time.Minute)
	for _, clientId := range fixture.clientIds {
		insertIndexDietClosedContracts(t, ctx, &clientId, nil, fixture.networkId, passEnd, 1000)
		insertIndexDietClosedContracts(t, ctx, nil, &clientId, fixture.networkId, passEnd, 1000)
	}
	insertIndexDietClosedContracts(t, ctx, nil, nil, server.NewId(), now, 20000)
	analyzeIndexDietContracts(t, ctx)

	// a day's pass networks on Main, the pass's among them
	fixture.networkIds = []server.Id{fixture.networkId}
	for range 1599 {
		fixture.networkIds = append(fixture.networkIds, server.NewId())
	}
	fixture.lower, fixture.upper = ContractIdCreateTimeRange(now.Add(-time.Hour), now)
	return fixture
}

// The first window of the walk, as the batch reads it.
func (self *proberRetentionPlanFixture) window(t testing.TB, ctx context.Context, batchSize int) []server.Id {
	t.Helper()
	var windowIds []server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, proberShardRetentionWalkWindowSql, self.lower, server.Id{}, self.upper, batchSize).Scan(&windowIds))
	})
	if len(windowIds) != batchSize {
		t.Fatalf("the walk's window holds %d contracts, want %d", len(windowIds), batchSize)
	}
	return windowIds
}

// Every scan of transfer_contract reads transfer_contract_pkey, by a condition
// on contract_id, and never transfer_contract_open_source_id.
func assertProberRetentionPlanReadsOnlyThePrimaryKey(t testing.TB, name string, scans []indexDietContractScan, raw string) {
	t.Helper()
	if len(scans) == 0 {
		t.Fatalf("%s reads no transfer_contract:\n%s", name, raw)
	}
	for _, scan := range scans {
		if len(scan.reads) == 0 {
			t.Fatalf("%s reads transfer_contract by %s, not an index:\n%s", name, scan.nodeType, raw)
		}
		for _, read := range scan.reads {
			if read.index != "transfer_contract_pkey" || !strings.Contains(read.condition, "contract_id") {
				t.Fatalf("%s reads %s (%s), not a primary key range or the batch's ids:\n%s", name, read.index, read.condition, raw)
			}
		}
	}
	if strings.Contains(raw, "transfer_contract_open_source_id") {
		t.Fatalf("%s reads transfer_contract_open_source_id:\n%s", name, raw)
	}
}

// The root cause: the window the walk replaced read the closed range of each
// pass client in transfer_contract_open_source_id. Its origin arm has no other
// index of closed contracts by source.
func TestProberRetentionLegacyWindowReadTheOpenSourceIdIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		if server.IsContractPartitioned(t.Context()) {
			t.Skip("plain contract tables only")
		}
		ctx := t.Context()
		fixture := newProberRetentionPlanFixture(t, ctx)
		scans, raw := planIndexDietStatement(t, ctx, nil, legacyProberShardRetentionContractWindowSql,
			fixture.clientIds, fixture.networkId, fixture.cutoff, server.Id{}, 500)
		openSourceId := false
		for _, scan := range scans {
			for _, read := range scan.reads {
				if read.index == "transfer_contract_open_source_id" && strings.Contains(read.condition, "source_id") {
					openSourceId = true
				}
			}
		}
		if !openSourceId {
			t.Fatalf("the replaced window did not read the origins from transfer_contract_open_source_id:\n%s", raw)
		}
	})
}

// Every statement a real drop sends that names transfer_contract, recorded
// with its arguments (traceContractReads) and planned again with sequential
// scans off and transfer_contract_open_source_id present, where the planner
// could choose it, reads no range of that index. The drop before the walk
// sent the window that read each pass client's closed range from it
// (TestProberRetentionLegacyWindowReadTheOpenSourceIdIndex).
func TestProberRetentionDropReadsNoOpenSourceIdIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		if server.IsContractPartitioned(t.Context()) {
			t.Skip("plain contract tables only")
		}
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		now := server.NowUtc()
		// the pass's origins and reply companions, and other payers' contracts
		// around them, including the pass client's fan-in from other payers
		insertIndexDietClosedContracts(t, ctx, &owner.ClientId, nil, owner.NetworkId, now, 1000)
		insertIndexDietClosedContracts(t, ctx, nil, &owner.ClientId, owner.NetworkId, now, 1000)
		insertIndexDietClosedContracts(t, ctx, nil, &owner.ClientId, server.NewId(), now, 1000)
		insertIndexDietClosedContracts(t, ctx, nil, nil, server.NewId(), now, 20000)
		analyzeIndexDietContracts(t, ctx)

		settings := DefaultProberShardRetentionSettings()
		settings.DefaultMode = ProberShardRetentionModeOn
		settings.DefaultMaxWalkRowsPerSecond = settings.MaxWalkRowsPerSecond
		settings.DefaultMaxContractsPerSecond = settings.MaxContractsPerSecond
		var report *ProberShardRetentionReport
		statements := traceContractReads(t, ctx, func() {
			var err error
			report, _, err = RemoveProberShardGenerations(ctx, server.NowUtc().Add(9*24*time.Hour), ProberShardContractWalk{}, settings)
			if err != nil {
				t.Fatal(err)
			}
		})
		if report.Shards[proberShardRetentionShardDropped] != 1 || report.Contracts[proberShardRetentionContractDeleted] != 2000 {
			t.Fatalf("the drop did not delete the pass's 2000 contracts and drop it: %+v", report)
		}
		walked := false
		for _, statement := range statements {
			walked = walked || statement.sql == proberShardRetentionWalkWindowSql
			conn := acquireContractLifecycleTestConnection(t, ctx)
			func() {
				defer conn.Release()
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer func() {
					_ = tx.Rollback(context.WithoutCancel(ctx))
				}()
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
				var raw []byte
				server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+statement.sql, statement.args...).Scan(&raw))
				if strings.Contains(string(raw), "transfer_contract_open_source_id") {
					t.Fatalf("the drop sends a statement that reads transfer_contract_open_source_id:\n%s\n%s", statement.sql, raw)
				}
			}()
		}
		if !walked {
			t.Fatalf("the drop sent no walk window among its %d contract statements", len(statements))
		}
	})
}

// Every statement of a walk batch that reads transfer_contract, as the batch
// runs it (the window by default, the rest under the bitmap settings with
// custom plans), reads only transfer_contract_pkey: with
// transfer_contract_open_source_id present, where the planner could choose it,
// and dropped.
func TestProberRetentionWalkReadsOnlyThePrimaryKey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		if server.IsContractPartitioned(t.Context()) {
			t.Skip("plain contract tables only")
		}
		ctx := t.Context()
		settings := DefaultProberShardRetentionSettings()
		fixture := newProberRetentionPlanFixture(t, ctx)
		windowIds := fixture.window(t, ctx, settings.WalkBatchSize)
		var matchedIds []server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, proberShardRetentionWalkMatchSql, windowIds, fixture.networkIds, fixture.cutoff, settings.ContractBatchSize).Scan(&matchedIds))
		})
		if len(matchedIds) == 0 {
			t.Fatal("the walk's first window keeps none of the pass's contracts")
		}

		bitmap := append([]string{`SET LOCAL plan_cache_mode = force_custom_plan`}, proberShardRetentionBitmapSettings...)
		for _, indexSetup := range [][]string{
			nil,
			{`DROP INDEX transfer_contract_open_source_id`},
		} {
			for _, statement := range []struct {
				name  string
				setup []string
				sql   string
				args  []any
			}{
				{
					name:  "the walk's window",
					setup: []string{`SET LOCAL plan_cache_mode = force_custom_plan`},
					sql:   proberShardRetentionWalkWindowSql,
					args:  []any{fixture.lower, server.Id{}, fixture.upper, settings.WalkBatchSize},
				},
				{
					name:  "the walk's match",
					setup: bitmap,
					sql:   proberShardRetentionWalkMatchSql,
					args:  []any{windowIds, fixture.networkIds, fixture.cutoff, settings.ContractBatchSize},
				},
				{
					name:  "the classification",
					setup: bitmap,
					sql:   proberShardRetentionClassifySql,
					args:  []any{matchedIds, fixture.networkIds, fixture.cutoff, ContractPartyDestination},
				},
				{
					name:  "the locked classification",
					setup: bitmap,
					sql:   proberShardRetentionClassifyLockSql,
					args:  []any{matchedIds, fixture.networkIds, fixture.cutoff, ContractPartyDestination},
				},
			} {
				setup := append(append([]string{}, indexSetup...), statement.setup...)
				scans, raw := planIndexDietStatement(t, ctx, setup, statement.sql, statement.args...)
				assertProberRetentionPlanReadsOnlyThePrimaryKey(t, statement.name, scans, raw)
			}
		}
	})
}
