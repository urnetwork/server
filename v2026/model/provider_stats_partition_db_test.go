package model

// The provider statistics' per-provider contract reads on the partitioned
// contract tables (since 2026-11-25), against the statements as they ran
// until 2026-11-26:
//   - with a provider array several times a day's distinct destinations, as
//     the largest networks pass, the statements before planned a sequential
//     scan of the filled day, and the reads now read every partition that
//     holds rows through its destination index, in custom and generic plans
//     and under two cost settings (the root cause);
//   - a provider list group's report lookup runs only the partitions of
//     contract_close that hold its id prefix, where every partition ran once
//     per group;
//   - the contract counts plan no partition minted before their window;
//   - every read answers what it answered before, here on the partitioned
//     layout and, in provider_stats_list_query_test.go, on the plain tables.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// The statements as they ran from 2026-11-18 until 2026-11-26, the controls
// of the root-cause tests. The provider list's contract counts as they ran is
// providerStatsContractsBeforeSQL (provider_stats_list_query_test.go).
const (
	providerStatsTransferBytesArrayBeforeSQL = `
	WITH eligible AS MATERIALIZED (
		SELECT contract_id, destination_id
		FROM transfer_contract
		WHERE destination_id = ANY($1::uuid[]) AND close_time >= $2
	), id_range AS MATERIALIZED (
		SELECT array_agg(contract_id) AS contract_ids
		FROM eligible
		GROUP BY substr(uuid_send(contract_id), 1, 4)
	), report AS MATERIALIZED (
		SELECT cc.contract_id, cc.used_transfer_byte_count
		FROM id_range
		CROSS JOIN LATERAL (
			SELECT contract_id, used_transfer_byte_count
			FROM contract_close
			WHERE contract_id = ANY(id_range.contract_ids) AND party = 'destination'
			OFFSET 0
		) cc
	)
	SELECT eligible.destination_id, COALESCE(SUM(report.used_transfer_byte_count), 0)
	FROM report
	JOIN eligible ON eligible.contract_id = report.contract_id
	GROUP BY eligible.destination_id
`
	providerStatsOverviewTransferBytesBeforeSQL = `
	SELECT to_char(tc.close_time, 'YYYY-MM-DD') AS day, COALESCE(SUM(cc.used_transfer_byte_count), 0)
	FROM transfer_contract tc
	INNER JOIN contract_close cc ON
		cc.contract_id = tc.contract_id AND cc.party = 'destination'
	WHERE tc.destination_id = ANY($1::uuid[]) AND tc.close_time >= $2
	GROUP BY day
`
	providerStatsOverviewContractsBeforeSQL = `
	SELECT to_char(create_time, 'YYYY-MM-DD') AS day, COUNT(*), COUNT(DISTINCT source_id)
	FROM transfer_contract
	WHERE destination_id = ANY($1::uuid[]) AND create_time >= $2
	GROUP BY day
`
)

const (
	// the network's providers that hold contracts, and all of its providers
	providerStatsPartitionActiveProviders = 300
	providerStatsPartitionProviders       = 9000
	// the other networks' destinations
	providerStatsPartitionOtherDestinations = 2700
	// contracts minted in the legacy partition's three days, yesterday and today
	providerStatsPartitionLegacyContracts    = 6000
	providerStatsPartitionYesterdayContracts = 24000
	providerStatsPartitionTodayContracts     = 6000
)

// A test database cut over yesterday, as Main stood the day after its
// cutover: the legacy partition, yesterday's and today's partitions and the
// days ahead.
type providerStatsPartitionFixture struct {
	now       time.Time
	yesterday time.Time
	today     time.Time
	network   server.Id
	// the network's providers, those with contracts first
	providers []server.Id
}

// The network's providers hold one contract in twenty of every day, the rest
// go to the other networks' destinations, so a day's partition holds about
// 3,000 distinct destinations against the network's 9,000 providers, as a
// day on Main holds about 130,000 against the largest networks' 100,000 to
// 300,000. Each partition holds at most 30,000 rows, which ANALYZE reads
// whole, so the plans are the same on every run. The contracts close within
// minutes, but one in fifty stays open, and one legacy contract in thirty
// closes in the day before now, as a late close does. Most contracts have a
// destination report, some only a source report; one contract in nine of the
// network has a report of zero bytes. Reports without a contract are minted
// yesterday. Contract ids lead with their minting millisecond, as
// connect.NewId's do.
func seedProviderStatsPartitionFixture(t testing.TB, ctx context.Context) *providerStatsPartitionFixture {
	t.Helper()
	now := server.NowUtc()
	today := server.ContractPartitionDayStart(now)
	fixture := &providerStatsPartitionFixture{
		now:       now,
		yesterday: today.AddDate(0, 0, -1),
		today:     today,
		network:   server.NewId(),
	}
	for range providerStatsPartitionProviders {
		fixture.providers = append(fixture.providers, server.NewId())
	}
	server.Testing_PartitionContractsFrom(ctx, fixture.yesterday)
	todaySpan := max(time.Duration(0), now.Add(-time.Minute).Sub(today))
	server.Tx(ctx, func(tx server.PgTx) {
		exec := func(sql string, args ...any) {
			server.RaisePgResult(tx.Exec(ctx, sql, args...))
		}
		exec(`
			INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time, outcome)
			SELECT
				(lpad(to_hex(floor(extract(epoch FROM minted) * 1000)::bigint), 12, '0')
					|| substr(md5('provider-stats-partition-' || g), 1, 20))::uuid,
				md5('source-network-' || g % 3)::uuid,
				md5('source-' || (g * 7) % 41)::uuid,
				CASE WHEN g % 20 = 0 THEN $1::uuid ELSE md5('other-network-' || g % 5)::uuid END,
				CASE WHEN g % 20 = 0 THEN ($2::uuid[])[1 + (g / 20) % array_length($2::uuid[], 1)]
					ELSE md5('other-destination-' || g % $3::integer)::uuid END,
				1000000,
				minted,
				CASE WHEN g % 50 = 0 THEN NULL
					WHEN g <= $4::integer AND g % 30 = 0 THEN $9::timestamp - (1 + g % 23) * interval '1 hour'
					ELSE minted + (g % 13) * interval '37 seconds' END,
				CASE WHEN g % 50 = 0 THEN NULL WHEN g % 17 = 0 THEN 'canceled' ELSE 'success' END
			FROM (
				SELECT g,
					CASE WHEN g <= $4::integer THEN $6::timestamp - interval '3 days' + (g - 1) * (interval '3 days' / $4::integer)
						WHEN g <= $4::integer + $5::integer THEN $6::timestamp + (g - $4::integer - 1) * (interval '1 day' / $5::integer)
						ELSE $7::timestamp + (g - $4::integer - $5::integer - 1) * ($8::interval / $10::integer) END AS minted
				FROM generate_series(1, $4::integer + $5::integer + $10::integer) g
			) contract`,
			fixture.network,
			idStrings(fixture.providers[:providerStatsPartitionActiveProviders]),
			providerStatsPartitionOtherDestinations,
			providerStatsPartitionLegacyContracts,
			providerStatsPartitionYesterdayContracts,
			fixture.yesterday,
			fixture.today,
			todaySpan,
			now,
			providerStatsPartitionTodayContracts,
		)
		exec(`
			INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count, checkpoint)
			SELECT contract_id, COALESCE(close_time, create_time), party,
				CASE WHEN party = 'source' THEN 1000003 ELSE (hashint8(n)::bigint & 2147483647) % 9 * 1009 END,
				n % 19 = 0
			FROM (
				SELECT contract_id, close_time, create_time, p.party, row_number() OVER (ORDER BY contract_id, p.party) AS n
				FROM transfer_contract CROSS JOIN (VALUES ('source'), ('destination')) p(party)
			) report
			WHERE (party = 'destination' AND n % 7 <> 0) OR (party = 'source' AND n % 5 <> 0)`)
		exec(`
			INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
			SELECT (lpad(to_hex(floor(extract(epoch FROM $1::timestamp + g * interval '1 minute') * 1000)::bigint), 12, '0')
					|| substr(md5('provider-stats-partition-orphan-' || g), 1, 20))::uuid,
				$1::timestamp + g * interval '1 minute', 'destination', 999983
			FROM generate_series(1, 50) g`, fixture.yesterday)
	}, server.OptReadWrite())
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract, contract_close`))
	}, server.OptReadWrite())
	// as production leaves them, the empty days ahead read never analyzed
	server.ClearEmptyPartitionLeafStatistics(ctx, now)
	return fixture
}

// The partitions of each contract table that hold rows.
func providerStatsPartitionFilledLeaves(t testing.TB, ctx context.Context) map[string]int {
	t.Helper()
	filled := map[string]int{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, table := range []string{"transfer_contract", "contract_close"} {
			result, err := conn.Query(ctx, fmt.Sprintf(`SELECT tableoid::regclass::text, count(*) FROM %s GROUP BY 1`, table))
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var leaf string
					var count int
					server.Raise(result.Scan(&leaf, &count))
					filled[leaf] = count
				}
			})
		}
	})
	return filled
}

// The cost settings a plan is asserted under: the test database's own, and
// those of a database on solid-state storage with its working set cached,
// which favor index reads, with parallel plans allowed.
var providerStatsPlannerSettings = []struct {
	name     string
	settings []string
}{
	{name: "test database"},
	{name: "cached", settings: []string{
		"random_page_cost = 1.1",
		"effective_cache_size = '768GB'",
		"work_mem = '234MB'",
		"hash_mem_multiplier = 16",
		"max_parallel_workers_per_gather = 4",
	}},
}

// A provider statistics read, before and now, with the arguments of each.
type providerStatsPartitionRead struct {
	name       string
	beforeSql  string
	sql        string
	beforeArgs func(ids []string, start time.Time) []any
	args       func(ids []string, start time.Time) []any
}

func providerStatsPartitionReads() []providerStatsPartitionRead {
	windowArgs := func(ids []string, start time.Time) []any {
		return []any{ids, start}
	}
	boundArgs := func(ids []string, start time.Time) []any {
		return []any{ids, start, providerStatsCreatedFromContractId(start)}
	}
	return []providerStatsPartitionRead{
		{name: "list transfer bytes", beforeSql: providerStatsTransferBytesArrayBeforeSQL, sql: providerStatsTransferBytesSQL, beforeArgs: windowArgs, args: windowArgs},
		{name: "list contracts", beforeSql: providerStatsContractsBeforeSQL, sql: providerStatsContractsSQL, beforeArgs: windowArgs, args: boundArgs},
		{name: "overview transfer bytes", beforeSql: providerStatsOverviewTransferBytesBeforeSQL, sql: providerStatsOverviewTransferBytesSQL, beforeArgs: windowArgs, args: windowArgs},
		{name: "overview contracts", beforeSql: providerStatsOverviewContractsBeforeSQL, sql: providerStatsOverviewContractsSQL, beforeArgs: windowArgs, args: boundArgs},
	}
}

// The plan of a statement as the services run it: prepared, and executed in
// the plan cache mode, under the settings, in a read-only transaction. With
// analyze, the statement runs.
func providerStatsPreparedPlan(t testing.TB, ctx context.Context, conn server.PgConn, settings []string, mode string, analyze bool, sql string, args []any) providerTransferTestPlanNode {
	t.Helper()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	server.Raise(err)
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		_, _ = conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE provider_stats_partition_plan`)
	}()
	for _, setting := range append([]string{"plan_cache_mode = " + mode}, settings...) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL `+setting))
	}
	// EXECUTE takes its arguments as expressions, not bind parameters
	types := []string{}
	literals := []string{}
	for _, arg := range args {
		switch value := arg.(type) {
		case []string:
			types = append(types, "uuid[]")
			literals = append(literals, fmt.Sprintf(`'{%s}'::uuid[]`, strings.Join(value, ",")))
		case time.Time:
			types = append(types, "timestamp")
			literals = append(literals, fmt.Sprintf(`'%s'::timestamp`, value.UTC().Format("2006-01-02 15:04:05.999999")))
		case server.Id:
			types = append(types, "uuid")
			literals = append(literals, fmt.Sprintf(`'%s'::uuid`, value))
		default:
			t.Fatalf("no literal for %T", arg)
		}
	}
	server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`PREPARE provider_stats_partition_plan(%s) AS %s`, strings.Join(types, ", "), sql)))
	options := "FORMAT JSON"
	if analyze {
		options = "ANALYZE, TIMING OFF, FORMAT JSON"
	}
	var raw []byte
	server.Raise(tx.QueryRow(ctx, fmt.Sprintf(`EXPLAIN (%s) EXECUTE provider_stats_partition_plan(%s)`, options, strings.Join(literals, ", ")), pgx.QueryExecModeSimpleProtocol).Scan(&raw))
	var plans []struct {
		Plan providerTransferTestPlanNode
	}
	server.Raise(json.Unmarshal(raw, &plans))
	if len(plans) != 1 {
		t.Fatalf("unexpected plan: %s", raw)
	}
	return plans[0].Plan
}

// The nodes of a plan that read a relation, with the index condition each
// reads by: its own, or for a bitmap heap scan its bitmap index scans'.
func providerStatsPlanScans(node providerTransferTestPlanNode, visit func(node providerTransferTestPlanNode, cond string)) {
	if node.Relation != "" {
		cond := node.Cond
		if node.NodeType == "Bitmap Heap Scan" {
			var bitmapConds func(providerTransferTestPlanNode)
			bitmapConds = func(child providerTransferTestPlanNode) {
				cond += " " + child.Cond
				for _, grandchild := range child.Plans {
					bitmapConds(grandchild)
				}
			}
			for _, child := range node.Plans {
				bitmapConds(child)
			}
		}
		visit(node, cond)
	}
	for _, child := range node.Plans {
		providerStatsPlanScans(child, visit)
	}
}

// How each filled partition of transfer_contract is read: by its destination
// index, by a range of contract ids (a contract count's bound, where the
// window starts in the partition's last ids), or "sequential" for a scan
// without either condition, which reads the whole partition.
func providerStatsContractLeafReads(plan providerTransferTestPlanNode, filled map[string]int) map[string]string {
	reads := map[string]string{}
	providerStatsPlanScans(plan, func(node providerTransferTestPlanNode, cond string) {
		if planContractTable(node.Relation) != "transfer_contract" || filled[node.Relation] == 0 {
			return
		}
		read := "sequential " + node.NodeType
		switch {
		case strings.Contains(cond, "destination_id"):
			read = "destination " + node.NodeType
		case strings.Contains(cond, "contract_id"):
			read = "contract id range " + node.NodeType
		}
		if previous, ok := reads[node.Relation]; ok && previous != read {
			read = previous + ", " + read
		}
		reads[node.Relation] = read
	})
	return reads
}

// The root cause: with the provider array of a network whose providers are
// three times a day's distinct destinations, each statement as it ran before
// read yesterday's partition, wholly in its window, with a sequential scan in
// its custom plan under at least one of the cost settings (at this scale the
// contract counts' presorted index read wins under the cached ones), where
// the network's contracts are one in twenty of the day. Each read now reads
// yesterday's partition through its destination index, and no partition of
// transfer_contract that holds rows whole, in custom and generic plans, under
// both cost settings.
func TestProviderStatsReadsReadEveryContractPartitionByDestination(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("cuts the test database over itself")
		}
		fixture := seedProviderStatsPartitionFixture(t, ctx)
		filled := providerStatsPartitionFilledLeaves(t, ctx)
		yesterdayLeaf := server.ContractPartitionName("transfer_contract", fixture.yesterday)
		if filled[yesterdayLeaf] != providerStatsPartitionYesterdayContracts {
			t.Fatalf("yesterday's partition holds %d contracts: %v", filled[yesterdayLeaf], filled)
		}
		ids := providerStatsIdStrings(fixture.providers)
		start := fixture.yesterday
		rootCauses := map[string][]string{}
		server.Db(ctx, func(conn server.PgConn) {
			for _, settings := range providerStatsPlannerSettings {
				for _, read := range providerStatsPartitionReads() {
					before := providerStatsContractLeafReads(providerStatsPreparedPlan(t, ctx, conn, settings.settings, "force_custom_plan", false, read.beforeSql, read.beforeArgs(ids, start)), filled)
					if strings.HasPrefix(before[yesterdayLeaf], "sequential") {
						rootCauses[read.name] = append(rootCauses[read.name], settings.name)
					}
					for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
						reads := providerStatsContractLeafReads(providerStatsPreparedPlan(t, ctx, conn, settings.settings, mode, false, read.sql, read.args(ids, start)), filled)
						if !strings.HasPrefix(reads[yesterdayLeaf], "destination") || strings.Contains(reads[yesterdayLeaf], ",") {
							t.Fatalf("%s (%s, %s) reads yesterday's partition by %q: %v", read.name, settings.name, mode, reads[yesterdayLeaf], reads)
						}
						for leaf, how := range reads {
							if strings.Contains(how, "sequential") || strings.Contains(how, "Seq Scan") {
								t.Fatalf("%s (%s, %s) reads %s by %q: %v", read.name, settings.name, mode, leaf, how, reads)
							}
						}
						t.Logf("%s (%s, %s): %v, before %v", read.name, settings.name, mode, reads, before)
					}
				}
			}
		})
		for _, read := range providerStatsPartitionReads() {
			if len(rootCauses[read.name]) == 0 {
				t.Fatalf("%s before read yesterday's partition by its index under every cost setting; the test no longer sees the root cause", read.name)
			}
		}
		t.Logf("read yesterday's partition whole before, by cost setting: %v", rootCauses)
	})
}

// The id prefixes of the provider list's id groups: the first four bytes of
// each eligible contract's id.
func providerStatsEligibleIdPrefixes(t testing.TB, ctx context.Context, ids []string, start time.Time) map[string]bool {
	t.Helper()
	prefixes := map[string]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT DISTINCT encode(substr(uuid_send(contract_id), 1, 4), 'hex')
			FROM transfer_contract WHERE destination_id = ANY($1::uuid[]) AND close_time >= $2`, ids, start)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var prefix string
				server.Raise(result.Scan(&prefix))
				prefixes[prefix] = true
			}
		})
	})
	return prefixes
}

// The executions of each partition's report lookup in a run of the provider
// list's transfer bytes.
func providerStatsReportLeafLoops(plan providerTransferTestPlanNode) map[string]int {
	loops := map[string]int{}
	providerStatsPlanScans(plan, func(node providerTransferTestPlanNode, cond string) {
		if planContractTable(node.Relation) == "contract_close" {
			loops[node.Relation] += int(node.Loops)
		}
	})
	return loops
}

// Each of the provider list's id groups looks its reports up in the
// partitions of contract_close that hold its prefix: one, or two for the
// group whose prefix spans midnight. A partition that holds no group, such as
// each day ahead, is never run. As the list ran before, every partition of
// contract_close ran once for every group.
func TestProviderStatsTransferBytesLookUpEachGroupInItsPartition(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("cuts the test database over itself")
		}
		fixture := seedProviderStatsPartitionFixture(t, ctx)
		ranges := contractPartitionRanges(t, ctx)
		ids := providerStatsIdStrings(fixture.providers)
		nonzero := func(counts map[string]int) map[string]int {
			maps.DeleteFunc(counts, func(name string, count int) bool { return count == 0 })
			return counts
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, start := range []time.Time{fixture.now.Add(-24 * time.Hour), fixture.yesterday.Add(12 * time.Hour)} {
				prefixes := providerStatsEligibleIdPrefixes(t, ctx, ids, start)
				if len(prefixes) < 100 {
					t.Fatalf("the window from %s has %d id groups", start, len(prefixes))
				}
				want := map[string]int{}
				ahead := 0
				for name, partition := range ranges {
					if partition.table != "contract_close" {
						continue
					}
					for prefix := range prefixes {
						// the group's ids are [first, last], and the partition's [lower, upper)
						first := server.RequireParseId(prefix + "-0000-0000-0000-000000000000")
						last := server.RequireParseId(prefix + "-ffff-ffff-ffff-ffffffffffff")
						if partition.lower.Cmp(last) <= 0 && first.Cmp(partition.upper) < 0 {
							want[name] += 1
						}
					}
					if !fixture.today.Add(24*time.Hour).After(server.ContractIdTime(partition.lower)) && want[name] == 0 {
						ahead += 1
					}
				}
				if ahead < server.ContractPartitionAheadDays {
					t.Fatalf("the days ahead hold groups: %v", want)
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					settings := []string{"max_parallel_workers_per_gather = 0"}
					loops := providerStatsReportLeafLoops(providerStatsPreparedPlan(t, ctx, conn, settings, mode, true, providerStatsTransferBytesSQL, []any{ids, start}))
					if !maps.Equal(nonzero(loops), want) {
						t.Fatalf("from %s (%s) the groups' lookups ran %v, want %v", start, mode, loops, want)
					}
					beforeLoops := providerStatsReportLeafLoops(providerStatsPreparedPlan(t, ctx, conn, settings, mode, true, providerStatsTransferBytesArrayBeforeSQL, []any{ids, start}))
					if len(beforeLoops) < len(want)+ahead {
						t.Fatalf("from %s (%s) before, the lookups ran %v; the test no longer sees the root cause", start, mode, beforeLoops)
					}
					for name, count := range beforeLoops {
						if count != len(prefixes) {
							t.Fatalf("from %s (%s) before, %s ran %d times for %d groups; the test no longer sees the root cause", start, mode, name, count, len(prefixes))
						}
					}
					t.Logf("from %s (%s): %d groups, lookups %v, before %v", start, mode, len(prefixes), loops, beforeLoops)
				}
			}
		})
	})
}

// The partitions of transfer_contract a plan reads.
func providerStatsPlanContractLeaves(plan providerTransferTestPlanNode) []string {
	leaves := []string{}
	providerStatsPlanScans(plan, func(node providerTransferTestPlanNode, cond string) {
		if planContractTable(node.Relation) == "transfer_contract" && !slices.Contains(leaves, node.Relation) {
			leaves = append(leaves, node.Relation)
		}
	})
	slices.Sort(leaves)
	return leaves
}

// The contract counts read no partition minted wholly before their window
// less the margin, in custom and generic plans: with a window from yesterday
// noon, not the legacy partition, which they read before.
func TestProviderStatsContractCountsReadNoPartitionBeforeTheirWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("cuts the test database over itself")
		}
		fixture := seedProviderStatsPartitionFixture(t, ctx)
		ranges := contractPartitionRanges(t, ctx)
		ids := providerStatsIdStrings(fixture.providers)
		start := fixture.yesterday.Add(12 * time.Hour)
		floor := providerStatsCreatedFromContractId(start)
		legacy := ""
		for name, partition := range ranges {
			if _, _, isLegacy, ok := server.ParseContractPartitionName(name); ok && isLegacy && partition.table == "transfer_contract" {
				legacy = name
			}
		}
		if legacy == "" {
			t.Fatal("no legacy partition")
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, read := range providerStatsPartitionReads() {
				if !strings.Contains(read.name, "contracts") {
					continue
				}
				before := providerStatsPlanContractLeaves(providerStatsPreparedPlan(t, ctx, conn, nil, "force_custom_plan", false, read.beforeSql, read.beforeArgs(ids, start)))
				if !slices.Contains(before, legacy) {
					t.Fatalf("%s before read %v; the test no longer sees the partitions it read", read.name, before)
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					leaves := providerStatsPlanContractLeaves(providerStatsPreparedPlan(t, ctx, conn, nil, mode, false, read.sql, read.args(ids, start)))
					if len(leaves) == 0 {
						t.Fatalf("%s (%s) reads no partition", read.name, mode)
					}
					for _, leaf := range leaves {
						if partition := ranges[leaf]; partition.upper.Cmp(floor) <= 0 {
							t.Fatalf("%s (%s) reads %s, minted before its window: %v", read.name, mode, leaf, leaves)
						}
					}
					t.Logf("%s (%s): %v, before %v", read.name, mode, leaves, before)
				}
			}
		})
	})
}

// Every read answers what it answered before on the partitioned layout, for
// every provider selection and window, under custom and generic plans: the
// windows open in the legacy partition's days, at the cutover, across the
// contract id margin of the cutover, on a day's last hours and in the last
// hour, at a contract's close and at a late close.
func TestProviderStatsReadsMatchBeforeWhenPartitioned(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("cuts the test database over itself")
		}
		fixture := seedProviderStatsPartitionFixture(t, ctx)
		p := fixture.providers
		var closeTime, lateCloseTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT min(close_time) FROM transfer_contract WHERE destination_id = ANY($1::uuid[]) AND contract_id >= $2`,
				idStrings(p), server.ContractIdFloor(fixture.yesterday.Add(6*time.Hour))).Scan(&closeTime))
			server.Raise(conn.QueryRow(ctx, `SELECT max(close_time) FROM transfer_contract WHERE destination_id = ANY($1::uuid[]) AND contract_id < $2 AND close_time IS NOT NULL`,
				idStrings(p), server.ContractIdFloor(fixture.yesterday)).Scan(&lateCloseTime))
		})
		if !fixture.yesterday.Before(lateCloseTime) {
			t.Fatalf("no late close: %s", lateCloseTime)
		}
		selections := []struct {
			name string
			ids  []server.Id
		}{
			{"network", p},
			{"active", p[:providerStatsPartitionActiveProviders]},
			{"idle", p[providerStatsPartitionActiveProviders:]},
			{"one", p[:1]},
			{"duplicate", []server.Id{p[0], p[0], p[1]}},
			{"empty", []server.Id{}},
			{"missing", []server.Id{server.NewId()}},
		}
		starts := []time.Time{
			fixture.yesterday.Add(-2 * 24 * time.Hour),
			fixture.yesterday,
			fixture.yesterday.Add(-ContractIdCreateTimeMargin),
			fixture.yesterday.Add(ContractIdCreateTimeMargin + time.Minute),
			fixture.today.Add(-3 * time.Hour),
			fixture.now.Add(-time.Hour),
			fixture.now.Add(-24 * time.Hour),
			closeTime,
			lateCloseTime,
			lateCloseTime.Add(time.Microsecond),
		}
		checked := 0
		server.Db(ctx, func(conn server.PgConn) {
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				for _, selection := range selections {
					for _, start := range starts {
						for _, read := range providerStatsPartitionReads() {
							func() {
								tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
								server.Raise(err)
								defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
								server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
								ids := providerStatsIdStrings(selection.ids)
								name := fmt.Sprintf("%s/%s/%s/%s", read.name, mode, selection.name, start.Format(time.RFC3339Nano))
								want := providerStatsPartitionRows(t, ctx, tx, read.beforeSql, read.beforeArgs(ids, start))
								if got := providerStatsPartitionRows(t, ctx, tx, read.sql, read.args(ids, start)); !reflect.DeepEqual(got, want) {
									t.Fatalf("%s: got=%v want=%v", name, got, want)
								}
								if selection.name == "network" && start.Equal(fixture.now.Add(-24*time.Hour)) && len(want) < 2 {
									t.Fatalf("%s: the fixture lost its shape: %v", name, want)
								}
								checked++
							}()
						}
					}
				}
			}
		})
		if checked != 2*len(selections)*len(starts)*len(providerStatsPartitionReads()) {
			t.Fatalf("checked %d reads", checked)
		}
	})
}

// Every row of a statement as text, keyed by its first column.
func providerStatsPartitionRows(t testing.TB, ctx context.Context, q providerStatsListQuerier, sql string, args []any) map[string]string {
	t.Helper()
	rows := map[string]string{}
	result, err := q.Query(ctx, sql, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			values, err := result.Values()
			server.Raise(err)
			texts := []string{}
			for _, value := range values {
				if id, ok := value.([16]byte); ok {
					value = server.Id(id)
				}
				texts = append(texts, fmt.Sprint(value))
			}
			if _, exists := rows[texts[0]]; exists {
				t.Fatalf("%s returned twice", texts[0])
			}
			rows[texts[0]] = strings.Join(texts[1:], "|")
		}
	})
	return rows
}
