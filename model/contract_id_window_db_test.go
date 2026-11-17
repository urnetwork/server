package model

// The hot reads of unexpired contracts and their contract weeks
// (contract_id_bound.go):
//   - on partitioned contract tables each read plans, and locks, only the
//     partitions of its weeks, in a custom and in a generic plan (C1
//     readiness, 2026-11-17: with the floor alone every read planned and
//     locked every partition and its indexes);
//   - on the plain tables each read returns the rows, and reads the indexes,
//     that it did with the floor alone.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The columns the hot reads bound.
var unexpiredContractIdBoundColumns = []string{
	"contract_id",
	"transfer_contract.contract_id",
	"contract_close.contract_id",
	"transfer_escrow.contract_id",
}

// The statement as it was before its weeks: each bound is the floor alone.
func floorOnlyUnexpiredContractStatement(t testing.TB, template string) string {
	t.Helper()
	statement := template
	for _, column := range unexpiredContractIdBoundColumns {
		statement = strings.ReplaceAll(statement, unexpiredContractIdBoundSql(column), column+" >= "+unexpiredContractIdFloorSql)
	}
	if statement == template || strings.Contains(statement, contractWeeksLowerSql) || strings.Contains(statement, contractWeeksUpperSql) {
		t.Fatalf("the statement has no bound, or keeps a placeholder: %s", statement)
	}
	return statement
}

// One hot read of unexpired contracts as it runs: its template and the
// arguments its caller passes.
type unexpiredContractRead struct {
	name     string
	template string
	args     []any
}

// The ids a read's arguments name.
type unexpiredContractReadIds struct {
	sourceId             server.Id
	destinationId        server.Id
	payerNetworkId       server.Id
	sourceNetworkId      server.Id
	destinationNetworkId server.Id
}

func newUnexpiredContractReadIds() unexpiredContractReadIds {
	return unexpiredContractReadIds{
		sourceId:             server.NewId(),
		destinationId:        server.NewId(),
		payerNetworkId:       server.NewId(),
		sourceNetworkId:      server.NewId(),
		destinationNetworkId: server.NewId(),
	}
}

// Every hot read of unexpired contracts, with the arguments its caller passes:
// the companion origin reads with and without the side table
// (contract_origin_linger.go), the zero escrow origin reads, the open escrow
// read and the removed client's open contracts. The origin reads take the
// pair reversed, as their callers do; the removed client is the source.
func unexpiredContractReads(t testing.TB, ids unexpiredContractReadIds) []unexpiredContractRead {
	t.Helper()
	runtimeQueries := coveredCompanionRuntimeQueries(t)
	closeFloor := server.NowUtc().Add(-CompanionOriginCloseLinger)
	plainArgs := []any{ids.sourceId, ids.destinationId, closeFloor, ids.payerNetworkId}
	if runtimeQueries["earliest_origin"].binds == 5 {
		plainArgs = append(plainArgs, ByteCount(1024))
	}
	chainedArgs := []any{ids.sourceId, ids.destinationId, closeFloor, ids.payerNetworkId, ids.sourceNetworkId, ids.destinationNetworkId}
	if runtimeQueries["earliest_companion_origin"].binds == 7 {
		chainedArgs = append(chainedArgs, ByteCount(1024))
	}
	existsArgs := []any{ids.sourceId, ids.destinationId, closeFloor}
	zeroChainedArgs := []any{ids.sourceId, ids.destinationId, closeFloor, ids.sourceNetworkId, ids.destinationNetworkId}
	return []unexpiredContractRead{
		{name: "companion origin exists", template: companionOriginExistsSql, args: existsArgs},
		{name: "legacy companion origin exists", template: legacyCompanionOriginExistsSql, args: existsArgs},
		{name: "plain origin selection", template: runtimeQueries["earliest_origin"].template, args: plainArgs},
		{name: "legacy plain origin selection", template: legacyCompanionPlainOriginSql, args: plainArgs},
		{name: "chained origin selection", template: runtimeQueries["earliest_companion_origin"].template, args: chainedArgs},
		{name: "legacy chained origin selection", template: legacyCompanionChainedOriginSql, args: chainedArgs},
		{name: "zero escrow plain origin", template: zeroEscrowPlainOriginSql, args: existsArgs},
		{name: "legacy zero escrow plain origin", template: legacyZeroEscrowPlainOriginSql, args: existsArgs},
		{name: "zero escrow chained origin", template: zeroEscrowChainedOriginSql, args: zeroChainedArgs},
		{name: "legacy zero escrow chained origin", template: legacyZeroEscrowChainedOriginSql, args: zeroChainedArgs},
		{name: "open transfer escrows", template: modelFunctionStatementTemplate(t, "GetOpenTransferEscrowsOrderedByPriorityCreateTime"),
			args: []any{ids.sourceId, ids.destinationId, ByteCount(1024)}},
		{name: "removed client open contracts", template: removedClientOpenContractsSql, args: []any{ids.sourceId, clientRemovedOpenContractLimit}},
	}
}

// Every row of a statement, each as its values' text joined by "|", in the
// statement's order.
func unexpiredContractReadRows(t testing.TB, ctx context.Context, conn server.PgConn, sql string, args ...any) []string {
	t.Helper()
	rows := []string{}
	result, err := conn.Query(ctx, sql, args...)
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
			rows = append(rows, strings.Join(texts, "|"))
		}
	})
	return rows
}

// A contract table's partition and the contract ids it holds, [lower, upper),
// with a zero lower for the legacy partition, which starts at MINVALUE.
type contractPartitionRange struct {
	table string
	lower server.Id
	upper server.Id
}

// The partitions of the contract tables, by name.
func contractPartitionRanges(t testing.TB, ctx context.Context) map[string]contractPartitionRange {
	t.Helper()
	bound := regexp.MustCompile(`FOR VALUES FROM \((?:MINVALUE|'([0-9a-f-]{36})')\) TO \('([0-9a-f-]{36})'\)`)
	ranges := map[string]contractPartitionRange{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT parent.relname, child.relname, pg_get_expr(child.relpartbound, child.oid)
			FROM pg_inherits
			JOIN pg_class parent ON parent.oid = pg_inherits.inhparent
			JOIN pg_class child ON child.oid = pg_inherits.inhrelid
			WHERE parent.relname = ANY($1) AND parent.relnamespace = 'public'::regnamespace AND child.relkind = 'r'
		`, server.ContractPartitionTables)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var table, leaf, expression string
				server.Raise(result.Scan(&table, &leaf, &expression))
				parts := bound.FindStringSubmatch(expression)
				if parts == nil {
					t.Fatalf("%s: unexpected partition bound %s", leaf, expression)
				}
				var lower server.Id
				if parts[1] != "" {
					lower = server.RequireParseId(parts[1])
				}
				ranges[leaf] = contractPartitionRange{table: table, lower: lower, upper: server.RequireParseId(parts[2])}
			}
		})
	})
	if len(ranges) == 0 {
		t.Fatal("no contract partitions")
	}
	return ranges
}

// Whether the partition holds any id of the weeks.
func (self contractPartitionRange) overlaps(weeks contractWeeks) bool {
	return self.lower.Cmp(weeks.upper) < 0 && weeks.lower.Cmp(self.upper) < 0
}

// The relation names of every node of an EXPLAIN (FORMAT JSON) plan, and the
// sum of its "Subplans Removed".
func contractPlanRelations(t testing.TB, raw []byte) (relations []string, subplansRemoved int) {
	t.Helper()
	var plans []map[string]any
	server.Raise(json.Unmarshal(raw, &plans))
	var inspect func(node map[string]any)
	inspect = func(node map[string]any) {
		if relation, ok := node["Relation Name"].(string); ok {
			relations = append(relations, relation)
		}
		if removed, ok := node["Subplans Removed"].(float64); ok {
			subplansRemoved += int(removed)
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
	}
	inspect(plans[0]["Plan"].(map[string]any))
	return
}

// The partitions a statement plans outside the weeks, in a custom and in a
// generic plan, and the relations it locks outside them when it runs in a
// custom plan: a partition, or an index of one.
func unexpiredContractReadOutsideWeeks(
	t testing.TB,
	ctx context.Context,
	conn server.PgConn,
	ranges map[string]contractPartitionRange,
	weeks contractWeeks,
	sql string,
	args []any,
) (planned map[string][]string, locked []string, locks int) {
	t.Helper()
	planned = map[string][]string{}
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
		var raw []byte
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, args...).Scan(&raw))
		server.Raise(tx.Rollback(ctx))
		relations, _ := contractPlanRelations(t, raw)
		for _, relation := range relations {
			if partition, ok := ranges[relation]; ok && !partition.overlaps(weeks) {
				planned[mode] = append(planned[mode], relation)
			}
		}
	}
	tx, err := conn.Begin(ctx)
	server.Raise(err)
	defer tx.Rollback(context.WithoutCancel(ctx))
	server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`))
	result, err := tx.Query(ctx, sql, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
		}
	})
	held, err := tx.Query(ctx, `
		SELECT relation.relname, owner.relname
		FROM pg_locks
		JOIN pg_class relation ON relation.oid = pg_locks.relation
		LEFT JOIN pg_index ON pg_index.indexrelid = relation.oid
		JOIN pg_class owner ON owner.oid = COALESCE(pg_index.indrelid, relation.oid)
		WHERE pg_locks.pid = pg_backend_pid() AND pg_locks.locktype = 'relation'
	`)
	server.WithPgResult(held, err, func() {
		for held.Next() {
			var relation, owner string
			server.Raise(held.Scan(&relation, &owner))
			locks += 1
			if partition, ok := ranges[owner]; ok && !partition.overlaps(weeks) {
				locked = append(locked, relation)
			}
		}
	})
	return
}

// On partitioned contract tables each hot read plans only the partitions of
// its weeks, in a custom plan and in the generic plan PostgreSQL caches, and
// running it locks no other partition or index of one. The test database is
// cut over at the current week, so its weeks ahead and, but early on Monday,
// the legacy partition are outside every read's weeks. The same statement
// with the floor alone, as the reads ran before, plans and locks partitions
// outside its weeks in both plans: the root cause, which the test proves
// against each read's own statement.
func TestUnexpiredContractReadsPlanAndLockOnlyTheirWeeksWhenPartitioned(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if !server.IsContractPartitioned(ctx) {
			server.Testing_PartitionContracts(ctx)
		}
		ranges := contractPartitionRanges(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		for _, read := range unexpiredContractReads(t, newUnexpiredContractReadIds()) {
			weeks := unexpiredContractWeeksAt(server.NowUtc())
			planned, locked, locks := unexpiredContractReadOutsideWeeks(t, ctx, conn, ranges, weeks, weeks.render(read.template), read.args)
			if len(planned) != 0 || len(locked) != 0 {
				t.Fatalf("%s plans %v and locks %v outside its weeks", read.name, planned, locked)
			}
			floorPlanned, floorLocked, floorLocks := unexpiredContractReadOutsideWeeks(t, ctx, conn, ranges, weeks, floorOnlyUnexpiredContractStatement(t, read.template), read.args)
			if len(floorPlanned["force_custom_plan"]) == 0 || len(floorPlanned["force_generic_plan"]) == 0 || len(floorLocked) == 0 {
				t.Fatalf("%s with the floor alone plans %v and locks %v outside its weeks; the test no longer sees the root cause", read.name, floorPlanned, floorLocked)
			}
			t.Logf("%s: %d relation locks, and %d with the floor alone", read.name, locks, floorLocks)
		}
	})
}

// A statement that reaches the database without its weeks rendered fails to
// parse; it never reads without its bound.
func TestUnrenderedContractWeeksFailToParse(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			_, err := conn.Exec(ctx, `SELECT count(*) FROM transfer_contract WHERE `+unexpiredContractIdBoundSql("contract_id"))
			if err == nil || !strings.Contains(err.Error(), "syntax error") {
				t.Fatalf("an unrendered bound ran: %v", err)
			}
		})
	})
}

// Writes the fixture of the plain table comparison: for the pair, open and
// closed contracts, plain and companion, minted from before the floor to the
// margin ahead of now. Each has a live deadline and a creation time equal to
// its id's (now for an id ahead of now), so only the contract id bound can
// exclude one, and each origin selection's earliest is the earliest the bound
// admits; a closed contract is minted a second after the open ones of its
// offset, so no two tie on creation. The open plain contracts are funded and
// the closed ones are captured by the side table, so every read has rows.
// Returns the open plain contract of each offset.
func insertUnexpiredContractReadFixture(t testing.TB, ctx context.Context, ids unexpiredContractReadIds) (minted map[time.Duration]server.Id) {
	t.Helper()
	now := server.NowUtc()
	// two minutes either side of the unexpired age, beyond any skew between
	// the test's clock and the database's
	offsets := []time.Duration{
		-unexpiredContractIdAge - 5*time.Minute,
		-unexpiredContractIdAge - 2*time.Minute,
		-unexpiredContractIdAge + 2*time.Minute,
		-30 * time.Minute,
		-time.Minute,
		5 * time.Minute,
		ContractIdCreateTimeMargin - 2*time.Minute,
	}
	minted = map[time.Duration]server.Id{}
	server.Tx(ctx, func(tx server.PgTx) {
		for _, offset := range offsets {
			for _, kind := range []struct {
				companion bool
				closed    bool
			}{
				{companion: false, closed: false},
				{companion: true, closed: false},
				{companion: false, closed: true},
				{companion: true, closed: true},
			} {
				mintedAt := now.Add(offset)
				if kind.closed {
					mintedAt = mintedAt.Add(time.Second)
				}
				createTime := server.MinTime(mintedAt, now)
				contractId := contractIdMintedAt(mintedAt)
				if !kind.companion && !kind.closed {
					minted[offset] = contractId
				}
				var companionContractId *server.Id
				if kind.companion {
					companion := server.NewId()
					companionContractId = &companion
				}
				var closeTime *time.Time
				if kind.closed {
					closed := createTime.Add(time.Second)
					closeTime = &closed
				}
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO transfer_contract (
						contract_id, source_network_id, source_id, destination_network_id, destination_id,
						transfer_byte_count, companion_contract_id, payer_network_id, create_time, expiration_time,
						dispute, close_time
					)
					VALUES ($1, $2, $3, $4, $5, 512, $6, $7, $8, $9, $10::timestamp IS NOT NULL, $10::timestamp)
				`, contractId, ids.sourceNetworkId, ids.sourceId, ids.destinationNetworkId, ids.destinationId,
					companionContractId, ids.payerNetworkId, createTime, now.Add(time.Hour), closeTime))
				if !kind.companion && !kind.closed {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count) VALUES ($1, $2, 512)`,
						contractId, server.NewId()))
				}
			}
		}
	})
	var captured int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_origin_linger WHERE source_id = $1 AND destination_id = $2`,
			ids.sourceId, ids.destinationId).Scan(&captured))
	})
	if captured != 2*len(offsets) {
		t.Fatalf("the side table captured %d of the pair's %d closed contracts", captured, 2*len(offsets))
	}
	return
}

// On the plain contract tables every hot read returns the rows it returned
// with the floor alone, for contracts minted on either side of the floor and
// up to the margin ahead of now.
func TestUnexpiredContractReadsReturnTheFloorsRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("plain contract tables only")
		}
		ids := newUnexpiredContractReadIds()
		minted := insertUnexpiredContractReadFixture(t, ctx, ids)
		earliest := minted[-unexpiredContractIdAge+2*time.Minute].String()
		server.Db(ctx, func(conn server.PgConn) {
			for _, read := range unexpiredContractReads(t, ids) {
				rows := unexpiredContractReadRows(t, ctx, conn, unexpiredContractWeeksAt(server.NowUtc()).render(read.template), read.args...)
				floorRows := unexpiredContractReadRows(t, ctx, conn, floorOnlyUnexpiredContractStatement(t, read.template), read.args...)
				if len(rows) == 0 {
					t.Fatalf("%s returns no row", read.name)
				}
				// the plain selections' earliest is the earliest the floor admits
				if strings.Contains(read.name, "plain origin") && !strings.HasPrefix(rows[0], earliest) {
					t.Fatalf("%s returns %v, want the contract minted %s ago, %s", read.name, rows, unexpiredContractIdAge-2*time.Minute, earliest)
				}
				slices.Sort(rows)
				slices.Sort(floorRows)
				if !slices.Equal(rows, floorRows) {
					t.Fatalf("%s returns %v, and with the floor alone %v", read.name, rows, floorRows)
				}
				t.Logf("%s: %d rows", read.name, len(rows))
			}
		})
	})
}

// The scans of a plan: each relation with its node type and index, sorted.
func contractPlanScans(t testing.TB, raw []byte) []string {
	t.Helper()
	var plans []map[string]any
	server.Raise(json.Unmarshal(raw, &plans))
	scans := []string{}
	var inspect func(node map[string]any)
	inspect = func(node map[string]any) {
		if relation, ok := node["Relation Name"].(string); ok {
			index, _ := node["Index Name"].(string)
			scans = append(scans, fmt.Sprintf("%s %v %s", relation, node["Node Type"], index))
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
	}
	inspect(plans[0]["Plan"].(map[string]any))
	slices.Sort(scans)
	return scans
}

// Contracts as production holds them, every id leading with its creation
// millisecond: 200,000 settled contracts over the last 30 days with their two
// closes, the lingering origins' fixture (contract_origin_linger_plan_test.go)
// for the pair, and the pair's 1,000 open contracts of the last half hour,
// funded, a third of them companions. Analyzed.
func seedTimeOrderedContractPlanFixture(t testing.TB, ctx context.Context) (sourceId server.Id, destinationId server.Id, payerNetworkId server.Id) {
	t.Helper()
	sourceId, destinationId, payerNetworkId = seedContractOriginLingerPlan(t, ctx)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			WITH settled AS (
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id, destination_network_id, destination_id,
					transfer_byte_count, create_time, close_time, payer_network_id, outcome
				)
				SELECT
					(lpad(to_hex(floor(extract(epoch FROM c.create_time) * 1000)::bigint), 12, '0') ||
						substr(md5(n::text || ':' || gen_random_uuid()::text), 1, 20))::uuid,
					gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
					1, c.create_time, c.create_time + interval '2 minutes', gen_random_uuid(), 'success'
				FROM generate_series(1, 200000) AS n
				CROSS JOIN LATERAL (
					SELECT (now() AT TIME ZONE 'UTC') - interval '30 days' + n * interval '12960 milliseconds' AS create_time
				) AS c
				RETURNING contract_id, close_time
			)
			INSERT INTO contract_close (contract_id, party, used_transfer_byte_count, close_time)
			SELECT contract_id, party, 1, close_time FROM settled
			CROSS JOIN (VALUES ('source'), ('destination')) AS closes(party)
		`))
		server.RaisePgResult(tx.Exec(ctx, `
			WITH open AS (
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id, destination_network_id, destination_id,
					transfer_byte_count, create_time, payer_network_id, companion_contract_id
				)
				SELECT
					(lpad(to_hex(floor(extract(epoch FROM c.create_time) * 1000)::bigint), 12, '0') ||
						substr(md5(n::text || ':' || gen_random_uuid()::text), 1, 20))::uuid,
					gen_random_uuid(), $1, gen_random_uuid(), $2, 1, c.create_time, $3,
					CASE WHEN n % 3 = 0 THEN gen_random_uuid() END
				FROM generate_series(1, 1000) AS n
				CROSS JOIN LATERAL (
					SELECT (now() AT TIME ZONE 'UTC') - interval '30 minutes' + n * interval '1800 milliseconds' AS create_time
				) AS c
				RETURNING contract_id
			)
			INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count)
			SELECT contract_id, gen_random_uuid(), 1 FROM open
		`, sourceId, destinationId, payerNetworkId))
	}, server.OptNoRetry())
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract, contract_close, transfer_escrow, contract_origin_linger`))
	}, server.OptReadWrite(), server.OptNoRetry())
	return
}

// On the plain contract tables, with production's time-ordered ids
// (seedTimeOrderedContractPlanFixture), every hot read scans the relations and
// indexes it scanned with the floor alone, in a custom plan and in the generic
// plan PostgreSQL caches, and returns the same rows.
func TestUnexpiredContractReadsKeepTheFloorsPlans(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 240k contracts; skipped in -short")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("plain contract tables only")
		}
		sourceId, destinationId, payerNetworkId := seedTimeOrderedContractPlanFixture(t, ctx)
		ids := newUnexpiredContractReadIds()
		ids.sourceId, ids.destinationId, ids.payerNetworkId = sourceId, destinationId, payerNetworkId
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		for _, read := range unexpiredContractReads(t, ids) {
			weeksSql := unexpiredContractWeeksAt(server.NowUtc()).render(read.template)
			floorSql := floorOnlyUnexpiredContractStatement(t, read.template)
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				scans := map[string][]string{}
				rows := map[string][]string{}
				for name, sql := range map[string]string{"weeks": weeksSql, "floor": floorSql} {
					tx, err := conn.Begin(ctx)
					server.Raise(err)
					server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
					var raw []byte
					server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, read.args...).Scan(&raw))
					server.Raise(tx.Rollback(ctx))
					scans[name] = contractPlanScans(t, raw)
					rows[name] = unexpiredContractReadRows(t, ctx, conn, sql, read.args...)
					slices.Sort(rows[name])
				}
				if !slices.Equal(scans["weeks"], scans["floor"]) {
					t.Fatalf("%s (%s) scans %v, and with the floor alone %v", read.name, mode, scans["weeks"], scans["floor"])
				}
				if !slices.Equal(rows["weeks"], rows["floor"]) {
					t.Fatalf("%s (%s) returns %d rows, and with the floor alone %d", read.name, mode, len(rows["weeks"]), len(rows["floor"]))
				}
				t.Logf("%s (%s): %d rows; %v", read.name, mode, len(rows["weeks"]), scans["weeks"])
			}
		}

		// the removed client's read had no bound before: it reads the same
		// relations and indexes as without one
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			reads := map[string][]string{}
			for name, sql := range map[string]string{
				"bounded":   unexpiredContractWeeksAt(server.NowUtc()).render(removedClientOpenContractsSql),
				"unbounded": unboundedRemovedClientOpenContractsSql(t),
			} {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
				var raw []byte
				server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, sourceId, clientRemovedOpenContractLimit).Scan(&raw))
				server.Raise(tx.Rollback(ctx))
				reads[name] = contractPlanReads(t, raw)
			}
			if !slices.Equal(reads["bounded"], reads["unbounded"]) {
				t.Fatalf("(%s) the removed client's read reads %v, and without its bound %v", mode, reads["bounded"], reads["unbounded"])
			}
			t.Logf("removed client open contracts against no bound (%s): %v", mode, reads["bounded"])
		}
	})
}
