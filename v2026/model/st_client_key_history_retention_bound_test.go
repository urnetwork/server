package model

// The key history retention's batch plans and bounds (2026-11-16 incident).
//
// On Main, 2026-11-16, st_client_key_gone held 118M records and network_client
// 476M rows. Once the 7-day horizon made about 98% of the records due, the
// planner, which expects every record's client to still have a row (the
// anti-join estimate of one row: network_client has more distinct client ids
// than st_client_key_gone), planned the batch as a merge anti-join of the
// whole st_client_key_gone primary key with the whole network_client primary
// key under a blocking sort (cost 19.4M). One batch ran 30 minutes and held a
// snapshot 17M transaction ids old, while every sampled record's client was
// in fact gone. The fixture is that shape at test size: every record due,
// none of their clients with a row, and network_client four times larger.
//
// DB-backed: under the owner rule these run only after the branch, with its
// migrations, is merged to main.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/v2026"
)

const (
	// all due, in tie groups of goneKeyHistoryPlanGroupCount like a reap
	// batch's or a discovery page's records
	goneKeyHistoryPlanRecordCount = 7500
	goneKeyHistoryPlanGroupCount  = 50
	// other clients, more than the records as on Main; at most ANALYZE's
	// sample, so the statistics are exact
	goneKeyHistoryPlanClientCount = 30000
)

// The batch statement as Main ran it until 2026-11-16, the first page of a
// pass: the due records selected with the client check as the filter of one
// ordered query.
const goneKeyHistoryIncidentBatchSql = `
	SELECT st_client_key_gone.client_id, st_client_key_gone.gone_time
	FROM st_client_key_gone
	WHERE
		st_client_key_gone.gone_time < $1 AND
		st_client_key_gone.gone_time < (now() AT TIME ZONE 'UTC') - st_client_key_history_retention_floor() AND
		NOT EXISTS (
			SELECT 1 FROM network_client
			WHERE network_client.client_id = st_client_key_gone.client_id
		)
	ORDER BY st_client_key_gone.gone_time, st_client_key_gone.client_id
	LIMIT $2
	FOR UPDATE OF st_client_key_gone SKIP LOCKED
`

// Main's planner costs (its postgresql.conf, read 2026-11-16), so the crossover
// between the paths is Main's.
const goneKeyHistoryPlanSettingsSql = `SET LOCAL random_page_cost = 1.1; SET LOCAL effective_cache_size = '768GB'`

// Builds the fixture with deterministic ids, one family per prefix, and
// analyzes both tables.
func buildGoneKeyHistoryPlanFixture(t testing.TB, ctx context.Context) {
	t.Helper()
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			INSERT INTO st_client_key_gone (client_id, gone_time)
			SELECT
				('a0000000-0000-4000-8000-' || lpad(to_hex(i), 12, '0'))::uuid,
				(now() AT TIME ZONE 'UTC') - interval '40 days' + (i / %d) * interval '1 minute'
			FROM generate_series(0, %d) AS i`,
			goneKeyHistoryPlanGroupCount,
			goneKeyHistoryPlanRecordCount-1,
		)))
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			INSERT INTO network_client (client_id, network_id)
			SELECT
				('b0000000-0000-4000-8000-' || lpad(to_hex(i), 12, '0'))::uuid,
				'c0000000-0000-4000-8000-000000000000'::uuid
			FROM generate_series(0, %d) AS i`,
			goneKeyHistoryPlanClientCount-1,
		)))
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_client_key_gone, network_client`))
	}, server.OptReadWrite(), server.OptNoRetry())
}

type goneKeyHistoryPlanNode struct {
	NodeType    string                   `json:"Node Type"`
	Relation    string                   `json:"Relation Name"`
	Index       string                   `json:"Index Name"`
	IndexCond   string                   `json:"Index Cond"`
	ActualRows  float64                  `json:"Actual Rows"`
	ActualLoops float64                  `json:"Actual Loops"`
	Plans       []goneKeyHistoryPlanNode `json:"Plans"`
}

// One scan of the plan: the table, how it was read and, explained with
// analyze, the rows it returned over all its loops.
type goneKeyHistoryPlanScan struct {
	relation  string
	nodeType  string
	index     string
	indexCond string
	rows      float64
	loopCount float64
}

func (self goneKeyHistoryPlanScan) String() string {
	return fmt.Sprintf("%s %s %s (%s) rows=%.0f loops=%.0f", self.relation, self.nodeType, self.index, self.indexCond, self.rows, self.loopCount)
}

// The scans of st_client_key_gone and network_client in an EXPLAIN (FORMAT
// JSON) result, under Main's planner costs and settingsSql, the session
// settings the statement runs with. With args the statement is explained with
// them bound, as a custom plan, in a transaction that commits, so an analyze
// releases the row locks it took. Without args it is sent in the simple
// protocol, so a generic plan's placeholders reach the server unbound.
func goneKeyHistoryPlanScans(t testing.TB, ctx context.Context, settingsSql string, explainSql string, args ...any) (scans []goneKeyHistoryPlanScan) {
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
		if node.Relation == "st_client_key_gone" || node.Relation == "network_client" {
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

// A key probe: an index condition that is an equality on client_id.
func (self goneKeyHistoryPlanScan) keyed() bool {
	return strings.HasPrefix(self.indexCond, "(client_id = ")
}

// On Main's shape, the incident statement reads network_client whole. The
// batch statement, with the batch's session settings, reads one page of
// st_client_key_gone through its gone_time index, and otherwise reads
// st_client_key_gone and network_client only by key, once per record of the
// page, in its custom and generic plans, first and resumed. Explained with
// analyze, the first page reads at most a page and one tie group of records,
// and probes network_client at most a page of times.
func TestGoneStClientKeyHistoryBatchPlanReadsOnlyItsPage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		buildGoneKeyHistoryPlanFixture(t, ctx)
		settings := DefaultStClientKeyHistoryRetentionSettings()
		pageCount := settings.DeleteBatchCount
		minGoneTime := goneKeyHistoryTestDbNow(t, ctx).Add(-settings.horizon())
		// a record in the middle, inside its tie group
		cursorClientId := server.RequireParseId("a0000000-0000-4000-8000-000000000e1a")
		var cursorGoneTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT gone_time FROM st_client_key_gone WHERE client_id = $1`, cursorClientId).Scan(&cursorGoneTime))
		})

		// as it ran, without the batch's session settings
		incidentScans := goneKeyHistoryPlanScans(t, ctx, "", `EXPLAIN (FORMAT JSON) `+goneKeyHistoryIncidentBatchSql, minGoneTime, pageCount)
		t.Logf("incident: %v", incidentScans)
		readsWhole := false
		for _, scan := range incidentScans {
			if scan.relation == "network_client" && !scan.keyed() {
				readsWhole = true
			}
		}
		if !readsWhole {
			t.Fatalf("the fixture does not reproduce Main's incident plan, which read network_client whole: %v", incidentScans)
		}

		for _, read := range []struct {
			name    string
			explain string
			args    []any
		}{
			{"first", `EXPLAIN (FORMAT JSON) ` + stClientKeyHistoryRetentionBatchSql(pageCount, false), []any{minGoneTime}},
			{"first generic", `EXPLAIN (GENERIC_PLAN, FORMAT JSON) ` + stClientKeyHistoryRetentionBatchSql(pageCount, false), nil},
			{"resumed", `EXPLAIN (FORMAT JSON) ` + stClientKeyHistoryRetentionBatchSql(pageCount, true), []any{minGoneTime, cursorGoneTime, cursorClientId}},
			{"resumed generic", `EXPLAIN (GENERIC_PLAN, FORMAT JSON) ` + stClientKeyHistoryRetentionBatchSql(pageCount, true), nil},
			{"first analyzed", `EXPLAIN (ANALYZE, TIMING OFF, FORMAT JSON) ` + stClientKeyHistoryRetentionBatchSql(pageCount, false), []any{minGoneTime}},
		} {
			scans := goneKeyHistoryPlanScans(t, ctx, stClientKeyHistoryRetentionPlanSql, read.explain, read.args...)
			t.Logf("%s: %v", read.name, scans)
			pageReadCount := 0
			for _, scan := range scans {
				switch {
				case scan.relation == "st_client_key_gone" && scan.index == "st_client_key_gone_gone_time" && scan.indexCond != "":
					pageReadCount += 1
				case scan.relation == "st_client_key_gone" && scan.index == "st_client_key_gone_pkey" && scan.keyed():
				case scan.relation == "network_client" && scan.index == "network_client_pkey" && scan.keyed():
				default:
					t.Fatalf("%s reads %s other than by its page or by key: %v", read.name, scan, scans)
				}
			}
			if pageReadCount != 1 {
				t.Fatalf("%s reads st_client_key_gone_gone_time %d times, want once, for its page: %v", read.name, pageReadCount, scans)
			}
			if read.name != "first analyzed" {
				continue
			}
			for _, scan := range scans {
				switch {
				case scan.relation == "st_client_key_gone" && scan.index == "st_client_key_gone_gone_time":
					// the page, and the rest of its last tie group, which the
					// client_id order sorts
					if float64(pageCount+goneKeyHistoryPlanGroupCount) < scan.rows {
						t.Fatalf("the page read %.0f records, want at most %d: %v", scan.rows, pageCount+goneKeyHistoryPlanGroupCount, scans)
					}
				case scan.relation == "network_client":
					if float64(pageCount) < scan.loopCount {
						t.Fatalf("the batch probed network_client %.0f times, want at most %d: %v", scan.loopCount, pageCount, scans)
					}
				}
			}
		}
	})
}

// Holds lockSql, with args, in a transaction of its own until release is
// closed or a minute passes, so a test can make a batch wait. held is closed
// once the lock is held; done once the transaction ended.
func holdGoneKeyHistoryTestLock(t testing.TB, ctx context.Context, lockSql string, args ...any) (release chan struct{}, done chan struct{}) {
	t.Helper()
	held := make(chan struct{})
	release = make(chan struct{})
	done = make(chan struct{})
	go func() {
		defer close(done)
		server.HandleError(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, lockSql, args...))
				close(held)
				select {
				case <-release:
				case <-time.After(time.Minute):
				}
			}, server.OptNoRetry())
		})
	}()
	select {
	case <-held:
	case <-done:
		t.Fatalf("the lock was not taken: %s", lockSql)
	}
	return
}

// A delete batch that runs into its lock wait, statement or transaction bound
// rolls back, and the run stops its deletes at batch_bound without
// discovering, keeping its cursor for the next run; with the lock gone the
// next run deletes.
func TestGoneStClientKeyHistoryBatchStopsAtItsBounds(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		goneId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
		setGoneKeyHistoryTestDays(t, ctx, goneId, 8)
		const headLockSql = `SELECT 1 FROM st_client_key_head WHERE client_id = $1 FOR UPDATE`
		const recordsLockSql = `LOCK TABLE st_client_key_gone IN ACCESS EXCLUSIVE MODE`
		for _, bound := range []struct {
			name    string
			lockSql string
			bound   func(*StClientKeyHistoryRetentionSettings)
		}{
			{
				name:    "lock wait",
				lockSql: headLockSql,
				bound: func(settings *StClientKeyHistoryRetentionSettings) {
					settings.BatchLockTimeout = 200 * time.Millisecond
				},
			},
			{
				name:    "statement",
				lockSql: recordsLockSql,
				bound: func(settings *StClientKeyHistoryRetentionSettings) {
					settings.BatchLockTimeout = 0
					settings.BatchStatementTimeout = 200 * time.Millisecond
				},
			},
			{
				name:    "transaction",
				lockSql: recordsLockSql,
				bound: func(settings *StClientKeyHistoryRetentionSettings) {
					settings.BatchLockTimeout = 0
					settings.BatchStatementTimeout = 0
					settings.BatchTimeout = 300 * time.Millisecond
				},
			},
		} {
			settings := DefaultStClientKeyHistoryRetentionSettings()
			bound.bound(settings)
			now := goneKeyHistoryTestDbNow(t, ctx)
			// a discovery pass is due, so a bounded run that went on would
			// discover
			cursor := StClientKeyHistoryRetentionCursor{}
			var args []any
			if bound.lockSql == headLockSql {
				args = []any{goneId}
			}
			release, done := holdGoneKeyHistoryTestLock(t, ctx, bound.lockSql, args...)
			report, next := RemoveGoneStClientKeyHistory(ctx, now, cursor, settings)
			close(release)
			<-done
			if report.DeleteStop != StClientKeyHistoryRetentionStopBatchBound || report.RemovedClientCount != 0 || report.ExaminedRecordCount != 0 || report.More {
				t.Fatalf("%s bound: the run = %+v, want batch_bound with nothing removed", bound.name, report)
			}
			if next.DeleteAfterGoneTime != nil || next.DeleteAfterClientId != nil || next.DiscoverCompleteTime != nil || next.DiscoverAfterClientId != nil {
				t.Fatalf("%s bound: the run moved its cursor to %+v", bound.name, next)
			}
			if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
				t.Fatalf("%s bound: a rolled back batch changed rows: %+v", bound.name, rows)
			}
		}

		now := goneKeyHistoryTestDbNow(t, ctx)
		report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		if report.RemovedClientCount != 1 || report.ExaminedRecordCount != 1 || report.DeleteStop != StClientKeyHistoryRetentionStopDue {
			t.Fatalf("with the lock gone the run = %+v, want the client removed", report)
		}
	})
}

// A discovery page that runs into its statement bound rolls back, records
// nothing and keeps the pass's cursor for the next run.
func TestGoneStClientKeyHistoryDiscoveryStopsAtItsBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		goneId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.BatchLockTimeout = 0
		settings.BatchStatementTimeout = 200 * time.Millisecond
		// the deletes stop at the brake, and the discovery runs either way
		setGoneKeyHistoryTestBrake(t, ctx, map[string]string{StClientKeyHistoryRetentionControlFieldMode: "off"})
		defer setGoneKeyHistoryTestBrake(t, ctx, nil)

		now := goneKeyHistoryTestDbNow(t, ctx)
		release, done := holdGoneKeyHistoryTestLock(t, ctx, `LOCK TABLE st_client_key_head IN ACCESS EXCLUSIVE MODE`)
		report, next := RemoveGoneStClientKeyHistory(ctx, now, StClientKeyHistoryRetentionCursor{}, settings)
		close(release)
		<-done
		if !report.DiscoverBounded || report.DiscoveredClientCount != 0 || report.DeleteStop != StClientKeyHistoryRetentionStopBrakeOff || report.More {
			t.Fatalf("the run = %+v, want a bounded discovery that recorded nothing", report)
		}
		if next.DiscoverAfterClientId != nil || next.DiscoverCompleteTime != nil {
			t.Fatalf("a bounded discovery moved its cursor to %+v", next)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows.gone {
			t.Fatal("a rolled back discovery page recorded a client")
		}

		report, next = RemoveGoneStClientKeyHistory(ctx, now, next, settings)
		if report.DiscoverBounded || report.DiscoveredClientCount != 1 || next.DiscoverCompleteTime == nil {
			t.Fatalf("with the lock gone the run = %+v and cursor %+v, want the client recorded and the pass complete", report, next)
		}
	})
}

// Pure: a batch stops at its own bounds only while the run's ctx is live, and
// every other raise propagates.
func TestStClientKeyHistoryRetentionBoundedOnlyAtItsBounds(t *testing.T) {
	live := context.Background()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	pgError := func(code string) error {
		return &pgconn.PgError{Code: code}
	}
	for _, test := range []struct {
		name    string
		ctx     context.Context
		r       any
		bounded bool
	}{
		{name: "lock wait", ctx: live, r: pgError(pgerrcode.LockNotAvailable), bounded: true},
		{name: "statement", ctx: live, r: pgError(pgerrcode.QueryCanceled), bounded: true},
		{name: "transaction", ctx: live, r: pgError(pgTransactionTimeoutCode), bounded: true},
		{name: "idle transaction", ctx: live, r: pgError(pgerrcode.IdleInTransactionSessionTimeout), bounded: true},
		{name: "deadline", ctx: live, r: errors.Join(server.DbContextDoneError, pgError(pgerrcode.QueryCanceled), context.DeadlineExceeded), bounded: true},
		{name: "wrapped lock wait", ctx: live, r: fmt.Errorf("batch: %w", pgError(pgerrcode.LockNotAvailable)), bounded: true},
		{name: "run ended", ctx: ended, r: pgError(pgerrcode.QueryCanceled), bounded: false},
		{name: "run ended with deadline", ctx: ended, r: context.DeadlineExceeded, bounded: false},
		{name: "missing floor", ctx: live, r: pgError(pgerrcode.UndefinedFunction), bounded: false},
		{name: "guard refusal", ctx: live, r: pgError(pgerrcode.RaiseException), bounded: false},
		{name: "not a database error", ctx: live, r: errors.New("other"), bounded: false},
		{name: "not an error", ctx: live, r: "Done", bounded: false},
	} {
		if bounded := stClientKeyHistoryRetentionBounded(test.ctx, test.r); bounded != test.bounded {
			t.Errorf("%s: bounded = %t, want %t", test.name, bounded, test.bounded)
		}
	}
}

// Pure: the statements carry their page size, and only the resumed forms
// read a cursor.
func TestStClientKeyHistoryRetentionStatementsCarryTheirPageSize(t *testing.T) {
	for _, test := range []struct {
		sql     string
		limit   string
		resumed bool
	}{
		{sql: stClientKeyHistoryRetentionBatchSql(500, false), limit: "LIMIT 500", resumed: false},
		{sql: stClientKeyHistoryRetentionBatchSql(7, true), limit: "LIMIT 7", resumed: true},
		{sql: stClientKeyHistoryDiscoverPageSql(10000, false), limit: "LIMIT 10000", resumed: false},
		{sql: stClientKeyHistoryDiscoverPageSql(3, true), limit: "LIMIT 3", resumed: true},
	} {
		if !strings.Contains(test.sql, test.limit) || strings.Contains(test.sql, "{{") {
			t.Fatalf("statement without %s: %s", test.limit, test.sql)
		}
		if resumed := strings.Contains(test.sql, "client_id) > ($2, $3)") || strings.Contains(test.sql, "client_id > $1"); resumed != test.resumed {
			t.Fatalf("statement resumed=%t, want %t: %s", resumed, test.resumed, test.sql)
		}
	}
}
