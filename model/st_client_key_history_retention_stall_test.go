package model

// The root causes of the 2026-11-21 key history retention stall
// (DBDISK-RETENTION-REVIEW.md §3.3).
//
//   - A delete batch was bounded by gone clients (a page of records), not by
//     the history rows those clients own. On Main the page at the cursor held
//     500 clients that owned 73,879 registrations; deleting them took longer
//     than the 5 s statement bound, so every run's first batch was canceled,
//     rolled back and retried at the next run, which made no progress either,
//     from 11-21 18Z on. Each run still reported success.
//   - A pass resumed from its cursor until it reached the horizon. The 30-day
//     reap of retired SDK window children records them gone as of their
//     deactivation, so their records were written behind a cursor that had
//     moved past those times, and waited for the pass to end: 55.3M of the
//     56.5M due records on 11-23.
//
// DB-backed: under the owner rule these run only after the branch, with its
// migrations, is merged to main.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// Generations 1..generationCount for a client, and its current head at the
// last one. The rows satisfy the table's shape checks; neither the guard nor
// the retention decodes them.
func insertGoneKeyHistoryTestGenerations(t testing.TB, ctx context.Context, clientId server.Id, networkId server.Id, generationCount int) {
	t.Helper()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
			SELECT
				$1::uuid,
				g,
				$2,
				sha256(convert_to($1::uuid::text || ':' || g, 'UTF8')),
				'sha256:' || encode(sha256(convert_to($1::uuid::text || ':' || g, 'UTF8')), 'hex'),
				'\x7b7d'::bytea
			FROM generate_series(1, $3::bigint) AS g
			`,
			clientId,
			goneKeyHistoryTestDomain,
			generationCount,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation) VALUES ($1, $2, $3, $4)`,
			clientId,
			goneKeyHistoryTestDomain,
			networkId,
			generationCount,
		))
	})
}

// Records the client as gone at this offset from the database clock.
func setGoneKeyHistoryTestAgo(t testing.TB, ctx context.Context, clientId server.Id, ago time.Duration) {
	t.Helper()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO st_client_key_gone (client_id, gone_time)
			VALUES ($1, (now() AT TIME ZONE 'UTC') - make_interval(secs => $2))
			ON CONFLICT (client_id) DO UPDATE SET gone_time = EXCLUDED.gone_time
			`,
			clientId,
			ago.Seconds(),
		))
	})
}

// Makes every history row delete in this test's database cost at least a
// millisecond, so the time a delete statement takes is set by the rows it
// deletes, as on Main, where one attempt got through about 61k registrations
// in its 5 s.
func slowGoneKeyHistoryTestDeletes(t testing.TB, ctx context.Context) {
	t.Helper()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			CREATE FUNCTION st_client_key_history_test_slow_delete()
			RETURNS trigger LANGUAGE plpgsql AS $test_slow_delete$
			BEGIN
				PERFORM pg_sleep(0.001);
				RETURN OLD;
			END
			$test_slow_delete$
		`))
		server.RaisePgResult(tx.Exec(ctx, `
			CREATE TRIGGER st_client_key_history_test_slow_delete
			BEFORE DELETE ON st_client_key_history
			FOR EACH ROW EXECUTE FUNCTION st_client_key_history_test_slow_delete()
		`))
	})
}

// The gone time of each client's record, for the clients that still have one.
func readGoneKeyHistoryTestGoneTimes(t testing.TB, ctx context.Context, clientIds []server.Id) map[server.Id]time.Time {
	t.Helper()
	goneTimes := map[server.Id]time.Time{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT client_id, gone_time FROM st_client_key_gone WHERE client_id = ANY($1)`, clientIds)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var goneTime time.Time
				server.Raise(result.Scan(&clientId, &goneTime))
				goneTimes[clientId] = goneTime
			}
		})
	})
	return goneTimes
}

// Whether a record at (goneTime, clientId) lies behind the cursor: a pass that
// resumes there never reads it again.
func goneKeyHistoryTestBehind(cursor StClientKeyHistoryRetentionCursor, goneTime time.Time, clientId server.Id) bool {
	if cursor.DeleteAfterGoneTime == nil || cursor.DeleteAfterClientId == nil {
		return false
	}
	if !goneTime.Equal(*cursor.DeleteAfterGoneTime) {
		return goneTime.Before(*cursor.DeleteAfterGoneTime)
	}
	return clientId.Cmp(*cursor.DeleteAfterClientId) <= 0
}

// A page of 14 gone clients that own 450 registrations each, 6,300 rows, at
// least a millisecond each: deleting them all takes longer than the batch's
// 5 s statement bound, as the stuck page's 73,879 rows did on Main. Each run
// deletes one batch. Every batch must stay inside its bounds and delete rows,
// the cursor must never pass a client that still has rows, and the runs must
// delete every row and end the pass. Before the fix every run's batch was
// canceled at 5 s and rolled back, deleting nothing, so the pass made no
// progress at all.
func TestGoneStClientKeyHistoryHeavyPageProgressesUnderItsBounds(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const clientCount = 14
		const generationCount = 450
		goneIds := []server.Id{}
		for i := range clientCount {
			goneId := server.NewId()
			insertGoneKeyHistoryTestGenerations(t, ctx, goneId, server.NewId(), generationCount)
			// one tie group of gone times, as a reap batch writes them, and a
			// later one
			setGoneKeyHistoryTestAgo(t, ctx, goneId, 10*24*time.Hour-time.Duration(i/7)*time.Minute)
			goneIds = append(goneIds, goneId)
		}
		slowGoneKeyHistoryTestDeletes(t, ctx)
		// Main's bounds: a page of 500 records, 5 s a statement, 10 s a
		// transaction; one batch a run
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.RunBudget = 0

		now := goneKeyHistoryTestDbNow(t, ctx)
		cursor := goneKeyHistoryTestDeleteOnly(now)
		var removedHistoryCount int64
		passEnded := false
		for run := 1; run <= 2*clientCount && !passEnded; run += 1 {
			var report *StClientKeyHistoryRetentionReport
			report, cursor = RemoveGoneStClientKeyHistory(ctx, now, cursor, settings)
			if report.DeleteStop == StClientKeyHistoryRetentionStopBatchBound {
				t.Fatalf("run %d: the batch ran into its bound and rolled back, deleting nothing (%+v); %d of %d registrations removed so far", run, report, removedHistoryCount, clientCount*generationCount)
			}
			if report.RemovedHistoryCount == 0 && report.DeleteStop != StClientKeyHistoryRetentionStopDue {
				t.Fatalf("run %d made no progress: %+v", run, report)
			}
			removedHistoryCount += report.RemovedHistoryCount
			// the cursor never passes a client with rows left
			goneTimes := readGoneKeyHistoryTestGoneTimes(t, ctx, goneIds)
			for _, goneId := range goneIds {
				rows := readGoneKeyHistoryTestRows(t, ctx, goneId)
				if rows.history == 0 {
					if rows.heads != 0 || rows.gone {
						t.Fatalf("run %d: a client without history kept its head or record: %+v", run, rows)
					}
					continue
				}
				goneTime, ok := goneTimes[goneId]
				if !ok {
					t.Fatalf("run %d: a client with %d registrations left lost its record", run, rows.history)
				}
				if goneKeyHistoryTestBehind(cursor, goneTime, goneId) {
					t.Fatalf("run %d: the cursor %+v passed a client with %d registrations left", run, cursor, rows.history)
				}
			}
			passEnded = report.DeleteStop == StClientKeyHistoryRetentionStopDue
		}
		if !passEnded || cursor.DeleteAfterGoneTime != nil || cursor.DeleteAfterClientId != nil {
			t.Fatalf("the runs did not end the pass: cursor %+v", cursor)
		}
		if removedHistoryCount != clientCount*generationCount {
			t.Fatalf("the runs reported %d registrations removed, want %d", removedHistoryCount, clientCount*generationCount)
		}
		for _, goneId := range goneIds {
			if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows != (goneKeyHistoryTestRows{}) {
				t.Fatalf("a gone client kept rows %+v", rows)
			}
		}
	})
}

// Records written behind a pass's cursor while it walks a long backlog, as the
// 30-day reap of retired SDK window children writes them (gone as of their
// deactivation), are deleted within a day of the pass's start, while the pass
// still has due records ahead of it. Before the fix they waited until the pass
// reached the horizon: on Main the pass stalled, so they waited for good. What
// the rules keep stays: a record whose client still has a row and a client
// gone 3 days, both behind the cursor.
func TestGoneStClientKeyHistoryReachesRecordsWrittenBehindItsCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		const aheadCount = 30
		aheadIds := []server.Id{}
		for i := range aheadCount {
			aheadId := server.NewId()
			insertGoneKeyHistoryTestRows(t, ctx, aheadId, server.NewId())
			setGoneKeyHistoryTestAgo(t, ctx, aheadId, 20*24*time.Hour-time.Duration(i)*time.Minute)
			aheadIds = append(aheadIds, aheadId)
		}
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.DeleteBatchCount = 2
		// one batch a run
		settings.RunBudget = 0

		now := goneKeyHistoryTestDbNow(t, ctx)
		cursor := goneKeyHistoryTestDeleteOnly(now)
		report, cursor := RemoveGoneStClientKeyHistory(ctx, now, cursor, settings)
		if report.RemovedClientCount != 2 || cursor.DeleteAfterClientId == nil || *cursor.DeleteAfterClientId != aheadIds[1] {
			t.Fatalf("the first run = %+v with cursor %+v, want the first page deleted", report, cursor)
		}

		// written behind the cursor after the pass went by
		backdatedIds := []server.Id{}
		for i := range 3 {
			backdatedId := server.NewId()
			insertGoneKeyHistoryTestRows(t, ctx, backdatedId, server.NewId())
			setGoneKeyHistoryTestAgo(t, ctx, backdatedId, 40*24*time.Hour-time.Duration(i)*time.Minute)
			backdatedIds = append(backdatedIds, backdatedId)
		}
		networkId, existingId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), existingId, "test", "test")
		insertGoneKeyHistoryTestRows(t, ctx, existingId, networkId)
		// a record a client that exists must never have
		setGoneKeyHistoryTestAgo(t, ctx, existingId, 50*24*time.Hour)
		recentId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, recentId, server.NewId())
		setGoneKeyHistoryTestAgo(t, ctx, recentId, 3*24*time.Hour)
		for _, clientId := range append(append([]server.Id{}, backdatedIds...), existingId) {
			goneTime := readGoneKeyHistoryTestGoneTimes(t, ctx, []server.Id{clientId})[clientId]
			if !goneKeyHistoryTestBehind(cursor, goneTime, clientId) {
				t.Fatalf("the fixture's record is not behind the cursor %+v", cursor)
			}
		}

		// a day on, the pass is still walking its backlog
		later := now.Add(24*time.Hour + time.Minute)
		for run := 1; run <= 4; run += 1 {
			report, cursor = RemoveGoneStClientKeyHistory(ctx, later, cursor, settings)
			if report.DeleteStop == StClientKeyHistoryRetentionStopDue {
				t.Fatalf("run %d ended the pass: the fixture's backlog is too short (%+v)", run, report)
			}
		}
		aheadLeft := 0
		for _, aheadId := range aheadIds {
			if readGoneKeyHistoryTestRows(t, ctx, aheadId).gone {
				aheadLeft += 1
			}
		}
		if aheadLeft == 0 {
			t.Fatal("the fixture's backlog is too short: the pass deleted it all")
		}
		for _, backdatedId := range backdatedIds {
			if rows := readGoneKeyHistoryTestRows(t, ctx, backdatedId); rows != (goneKeyHistoryTestRows{}) {
				t.Fatalf("a record written behind the cursor a day ago is still waiting while the pass walks %d records ahead of it: %+v, cursor %+v", aheadLeft, rows, cursor)
			}
		}

		// the rest of the pass, and the next
		settings.RunBudget = time.Minute
		for range 2 {
			_, cursor = RemoveGoneStClientKeyHistory(ctx, later, cursor, settings)
		}
		for _, aheadId := range aheadIds {
			if rows := readGoneKeyHistoryTestRows(t, ctx, aheadId); rows != (goneKeyHistoryTestRows{}) {
				t.Fatalf("a due client kept rows %+v", rows)
			}
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, existingId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
			t.Fatalf("a client that still has a row lost key history or its record: %+v", rows)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, recentId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
			t.Fatalf("a client gone 3 days lost key history or its record: %+v", rows)
		}
	})
}

// Every scan of the given relations in an EXPLAIN (FORMAT JSON) result, under
// Main's planner costs and settingsSql, as goneKeyHistoryPlanScans explains
// it; the table a statement modifies is not a scan.
func goneKeyHistoryPlanScansOf(t testing.TB, ctx context.Context, settingsSql string, explainSql string, relations []string, args ...any) (scans []goneKeyHistoryPlanScan) {
	t.Helper()
	sessionSql := goneKeyHistoryPlanSettingsSql
	if settingsSql != "" {
		sessionSql += "; " + settingsSql
	}
	var raw []byte
	if 0 < len(args) {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, sessionSql))
			server.Raise(tx.QueryRow(ctx, explainSql, args...).Scan(&raw))
		}, server.OptNoRetry())
	} else {
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			results, err := conn.Conn().PgConn().Exec(ctx,
				`BEGIN READ ONLY; `+sessionSql+`; `+explainSql+`; COMMIT`).ReadAll()
			server.Raise(err)
			for _, result := range results {
				if 0 < len(result.Rows) {
					raw = result.Rows[0][0]
				}
			}
		}, server.OptReadOnly())
	}
	var plans []struct {
		Plan goneKeyHistoryPlanNode
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("invalid plan: %v %s", err, raw)
	}
	var walk func(goneKeyHistoryPlanNode)
	walk = func(node goneKeyHistoryPlanNode) {
		if node.NodeType != "ModifyTable" && slices.Contains(relations, node.Relation) {
			scans = append(scans, goneKeyHistoryPlanScan{
				relation:  node.Relation,
				nodeType:  node.NodeType,
				index:     node.Index,
				indexCond: node.IndexCond,
				rows:      node.ActualRows * node.ActualLoops,
				loopCount: node.ActualLoops,
			})
		}
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(plans[0].Plan)
	return
}

// On Main's shape, with a client that owns 3,000 registrations among the
// page's due clients, the statements a batch adds read st_client_key_history
// only by its primary key: the registrations it deletes by client, at most
// DeleteBatchRowCount of them, as the analyzed plan shows; the delete of those
// rows by their whole key; and the records' delete probes it once per due
// client. The due probe reads one range of st_client_key_gone_gone_time. In
// their custom and generic plans.
func TestGoneStClientKeyHistoryBatchRowsReadOnlyByKey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		buildGoneKeyHistoryPlanFixture(t, ctx)
		settings := DefaultStClientKeyHistoryRetentionSettings()
		heavyId := server.RequireParseId("a0000000-0000-4000-8000-000000000000")
		dueIds := []server.Id{heavyId}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			// one registration for every gone client and for as many other
			// clients, and 3,000 for the first gone client
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
				SELECT
					client_id,
					g,
					$1,
					sha256(convert_to(client_id::text || ':' || g, 'UTF8')),
					'sha256:' || encode(sha256(convert_to(client_id::text || ':' || g, 'UTF8')), 'hex'),
					'\x7b7d'::bytea
				FROM (
					SELECT client_id, 1 AS generation_count FROM st_client_key_gone
					UNION ALL
					SELECT client_id, 1 FROM network_client
				) AS clients
				CROSS JOIN LATERAL generate_series(1, CASE WHEN client_id = $2 THEN 3000 ELSE generation_count END) AS g
				`,
				goneKeyHistoryTestDomain,
				heavyId,
			))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_client_key_history`))
			result, err := conn.Query(ctx, `SELECT client_id FROM st_client_key_gone WHERE client_id <> $1 ORDER BY gone_time, client_id LIMIT $2`, heavyId, settings.DeleteBatchCount-1)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					server.Raise(result.Scan(&clientId))
					dueIds = append(dueIds, clientId)
				}
			})
		}, server.OptReadWrite(), server.OptNoRetry())
		minGoneTime := goneKeyHistoryTestDbNow(t, ctx).Add(-settings.horizon())
		keyClientIds := []server.Id{heavyId, heavyId}
		keyDomainHashes := [][]byte{goneKeyHistoryTestDomain, goneKeyHistoryTestDomain}
		keyGenerations := []int64{1, 2}
		relations := []string{"st_client_key_history", "st_client_key_gone"}
		rowsSql := stClientKeyHistoryRetentionRowsSql(settings.DeleteBatchRowCount)
		for _, read := range []struct {
			name    string
			explain string
			args    []any
		}{
			{"rows", `EXPLAIN (FORMAT JSON) ` + rowsSql, []any{dueIds}},
			{"rows generic", `EXPLAIN (GENERIC_PLAN, FORMAT JSON) ` + rowsSql, nil},
			{"rows analyzed", `EXPLAIN (ANALYZE, TIMING OFF, FORMAT JSON) ` + rowsSql, []any{dueIds}},
			{"delete rows", `EXPLAIN (FORMAT JSON) ` + stClientKeyHistoryRetentionDeleteRowsSql, []any{keyClientIds, keyDomainHashes, keyGenerations}},
			{"delete rows generic", `EXPLAIN (GENERIC_PLAN, FORMAT JSON) ` + stClientKeyHistoryRetentionDeleteRowsSql, nil},
			{"delete records", `EXPLAIN (FORMAT JSON) ` + stClientKeyHistoryRetentionDeleteRecordsSql, []any{dueIds}},
			{"delete records generic", `EXPLAIN (GENERIC_PLAN, FORMAT JSON) ` + stClientKeyHistoryRetentionDeleteRecordsSql, nil},
			{"due probe", `EXPLAIN (FORMAT JSON) ` + stClientKeyHistoryDueProbeSql(settings.DueProbeLimit), []any{minGoneTime}},
			{"due probe generic", `EXPLAIN (GENERIC_PLAN, FORMAT JSON) ` + stClientKeyHistoryDueProbeSql(settings.DueProbeLimit), nil},
		} {
			scans := goneKeyHistoryPlanScansOf(t, ctx, stClientKeyHistoryRetentionPlanSql, read.explain, relations, read.args...)
			for _, scan := range scans {
				logged := scan
				if 120 < len(logged.indexCond) {
					logged.indexCond = logged.indexCond[:120] + "..."
				}
				t.Logf("%s: %v", read.name, logged)
			}
			if len(scans) == 0 {
				t.Fatalf("%s reads neither table", read.name)
			}
			for _, scan := range scans {
				switch {
				case scan.nodeType == "Seq Scan":
					t.Fatalf("%s reads %s whole: %v", read.name, scan.relation, scans)
				case scan.relation == "st_client_key_history" && scan.index == "st_client_key_history_pkey" && strings.HasPrefix(strings.TrimLeft(scan.indexCond, "("), "client_id = "):
					if strings.HasPrefix(read.name, "delete rows") && !(strings.Contains(scan.indexCond, "domain_hash = ") && strings.Contains(scan.indexCond, "generation = ")) {
						t.Fatalf("%s reads %s other than by its whole key", read.name, scan)
					}
				case scan.relation == "st_client_key_gone" && strings.HasPrefix(read.name, "delete records") &&
					(scan.index == "st_client_key_gone_pkey" || scan.nodeType == "Bitmap Heap Scan"):
					// the due clients' records by key: the only condition the
					// statement gives a bitmap is its client ids
				case scan.relation == "st_client_key_gone" && strings.HasPrefix(read.name, "due probe") &&
					scan.index == "st_client_key_gone_gone_time" && scan.indexCond != "":
				default:
					t.Fatalf("%s reads %s other than by key: %v", read.name, scan, scans)
				}
				if read.name == "rows analyzed" && float64(settings.DeleteBatchRowCount) < scan.rows {
					t.Fatalf("the batch's rows read %.0f registrations, want at most %d: %v", scan.rows, settings.DeleteBatchRowCount, scans)
				}
			}
		}
	})
}

// Pure: the statements a batch adds carry their limits.
func TestStClientKeyHistoryRetentionRowStatementsCarryTheirLimits(t *testing.T) {
	for _, test := range []struct {
		sql   string
		limit string
		count int
	}{
		{sql: stClientKeyHistoryRetentionRowsSql(1000), limit: "LIMIT 1000", count: 2},
		{sql: stClientKeyHistoryRetentionRowsSql(7), limit: "LIMIT 7", count: 2},
		{sql: stClientKeyHistoryDueProbeSql(1000000), limit: "LIMIT 1000000", count: 1},
	} {
		if strings.Count(test.sql, test.limit) != test.count || strings.Contains(test.sql, "{{") {
			t.Fatalf("statement without %d %s: %s", test.count, test.limit, test.sql)
		}
	}
}

// Pure: a page of clients with one registration each, the usual case, goes in
// one batch; a batch's rows and a start over are bounded; the probe is on.
func TestStClientKeyHistoryRetentionRowBoundDefaults(t *testing.T) {
	settings := DefaultStClientKeyHistoryRetentionSettings()
	if settings.DeleteBatchRowCount < settings.DeleteBatchCount || 2000 < settings.DeleteBatchRowCount {
		t.Fatalf("a batch deletes at most %d rows of a page of %d records", settings.DeleteBatchRowCount, settings.DeleteBatchCount)
	}
	if settings.DeletePassRestartInterval < 15*time.Minute || 6*time.Hour < settings.DeletePassRestartInterval {
		t.Fatalf("a pass starts over every %v", settings.DeletePassRestartInterval)
	}
	if settings.DueProbeLimit <= 0 {
		t.Fatalf("the due probe is off: %d", settings.DueProbeLimit)
	}
}

// Each run counts why its deletes ended and publishes the records still due,
// for the stall alert; a batch that runs into its bound fails the run
// (Bounded), and a probe that cannot complete keeps the last count.
func TestGoneStClientKeyHistoryCountsRunsAndDueRecords(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		dueIds := []server.Id{}
		for range 3 {
			dueId := server.NewId()
			insertGoneKeyHistoryTestRows(t, ctx, dueId, server.NewId())
			setGoneKeyHistoryTestDays(t, ctx, dueId, 8)
			dueIds = append(dueIds, dueId)
		}
		recentId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, recentId, server.NewId())
		setGoneKeyHistoryTestDays(t, ctx, recentId, 3)
		runs := func(stop string) float64 {
			return testutil.ToFloat64(stClientKeyHistoryRetentionRunsCounter.WithLabelValues(stop))
		}
		dueGauge := func() (count float64, at time.Time) {
			count = testutil.ToFloat64(retentionDueRowsGauge.WithLabelValues(RetentionTableStClientKeyGone))
			seconds := testutil.ToFloat64(retentionDueRowsTimestampGauge.WithLabelValues(RetentionTableStClientKeyGone))
			return count, time.UnixMilli(int64(seconds * 1000))
		}
		settings := DefaultStClientKeyHistoryRetentionSettings()

		setGoneKeyHistoryTestBrake(t, ctx, map[string]string{StClientKeyHistoryRetentionControlFieldMode: "off"})
		before := runs(StClientKeyHistoryRetentionStopBrakeOff)
		probeTime := time.Now()
		now := goneKeyHistoryTestDbNow(t, ctx)
		report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), settings)
		if report.DeleteStop != StClientKeyHistoryRetentionStopBrakeOff || report.DueRecordCount != 3 || report.Bounded() {
			t.Fatalf("with the brake off the run = %+v, want brake_off and 3 records due", report)
		}
		if runs(StClientKeyHistoryRetentionStopBrakeOff) != before+1 {
			t.Fatal("the run was not counted as brake_off")
		}
		if count, at := dueGauge(); count != 3 || at.Before(probeTime.Add(-time.Second)) {
			t.Fatalf("the due gauge = %v at %v, want 3 now", count, at)
		}

		setGoneKeyHistoryTestBrake(t, ctx, nil)
		settings.BatchLockTimeout = 0
		settings.BatchStatementTimeout = 200 * time.Millisecond
		before = runs(StClientKeyHistoryRetentionStopBatchBound)
		release, done := holdGoneKeyHistoryTestLock(t, ctx, `LOCK TABLE st_client_key_gone IN ACCESS EXCLUSIVE MODE`)
		report, _ = RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), settings)
		close(release)
		<-done
		if report.DeleteStop != StClientKeyHistoryRetentionStopBatchBound || !report.Bounded() || report.DueRecordCount != -1 {
			t.Fatalf("a bounded run = %+v, want batch_bound, a failed run and no probe", report)
		}
		if runs(StClientKeyHistoryRetentionStopBatchBound) != before+1 {
			t.Fatal("the run was not counted as batch_bound")
		}
		if count, _ := dueGauge(); count != 3 {
			t.Fatalf("a probe that did not complete moved the due gauge to %v", count)
		}

		settings = DefaultStClientKeyHistoryRetentionSettings()
		report, _ = RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), settings)
		if report.RemovedClientCount != 3 || report.DeleteStop != StClientKeyHistoryRetentionStopDue || report.DueRecordCount != 0 || report.Bounded() {
			t.Fatalf("the run = %+v, want the 3 due clients removed and none due", report)
		}
		if count, _ := dueGauge(); count != 0 {
			t.Fatalf("the due gauge = %v after the deletes, want 0", count)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, recentId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
			t.Fatalf("a client gone 3 days lost key history or its record: %+v", rows)
		}
	})
}
