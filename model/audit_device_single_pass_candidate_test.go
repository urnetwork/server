// Qualification only: production keeps the two-range query. Candidate rows
// enter the owning reducer through its existing query interface, and native
// plans distinguish scan work from the combined aggregate's possible spill.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Carry and window events on startDay must remain different groups. The same
// maximum array, event-type filter and removed-carry postfilter are retained.
const auditDeviceSinglePassCandidateSql = `
	SELECT t.day, t.device_id, t.last_event[2] AS event_type, t.carry_in
	FROM (
		SELECT
			CASE WHEN event_time < @windowStart THEN @startDay
				ELSE to_char(event_time, 'YYYY-MM-DD') END AS day,
			device_id,
			MAX(ARRAY[event_id::varchar, event_type]) AS last_event,
			event_time < @windowStart AS carry_in
		FROM audit_device_event
		WHERE event_type IN (@eventTypeDeviceAdded, @eventTypeDeviceRemoved)
		GROUP BY day, device_id, carry_in
	) t
	WHERE NOT t.carry_in OR t.last_event[2] = @eventTypeDeviceAdded
	ORDER BY day ASC, carry_in DESC
`

// Only the owning reducer's query is replaceable; its ordering, row decoding,
// touched-day logic, active state and summary all run unchanged.
type auditDeviceCandidateQuery struct {
	query server.PgCanQuery
	sql   string
	rows  server.PgResult
	calls int
	count int
}

func (self *auditDeviceCandidateQuery) Query(ctx context.Context, sql string, args ...any) (server.PgResult, error) {
	if sql != computeStatsDeviceSql || self.calls != 0 {
		return nil, fmt.Errorf("unexpected device reducer query")
	}
	self.calls++
	rows, err := self.query.Query(ctx, self.sql, args...)
	if err != nil {
		return rows, err
	}
	self.rows = &auditDeviceCandidateRows{PgResult: rows, query: self}
	return self.rows, nil
}

// The older shared selector normalizes removed carry rows away. Check the
// unnormalized reducer stream too, including carry-before-events ordering.
type auditDeviceCandidateRows struct {
	server.PgResult
	query     *auditDeviceCandidateQuery
	lastDay   string
	lastCarry bool
}

func (self *auditDeviceCandidateRows) Scan(dest ...any) error {
	if err := self.PgResult.Scan(dest...); err != nil {
		return err
	}
	day, kind, carry := *dest[0].(*string), *dest[2].(*string), *dest[3].(*bool)
	if carry && kind != string(AuditEventTypeDeviceAdded) {
		return fmt.Errorf("removed carry row escaped the device query filter")
	}
	if day < self.lastDay || (day == self.lastDay && carry && !self.lastCarry) {
		return fmt.Errorf("device query changed day/carry ordering")
	}
	self.lastDay, self.lastCarry = day, carry
	self.query.count++
	return nil
}

// Reuse the current row selector and real reducer instead of maintaining a
// second implementation of connected-per-day semantics in this experiment.
func auditDeviceCandidateParity(t testing.TB, ctx context.Context, tx server.PgTx, cutoff time.Time, endDay string) (*Stats, int) {
	t.Helper()
	currentRows := auditStatsSelectedRows(ctx, tx, computeStatsDeviceSql, cutoff)
	candidateRows := auditStatsSelectedRows(ctx, tx, auditDeviceSinglePassCandidateSql, cutoff)
	if !maps.Equal(currentRows, candidateRows) {
		t.Fatalf("device candidate row multiset differs: current=%d candidate=%d", len(currentRows), len(candidateRows))
	}
	reduce := func(sql string) *Stats {
		query := &auditDeviceCandidateQuery{query: tx, sql: sql}
		stats := &Stats{}
		computeStatsDeviceWindow(ctx, stats, query, cutoff.Format(time.DateOnly), endDay, cutoff)
		if query.calls != 1 || query.rows == nil || query.count != len(candidateRows) {
			t.Fatal("owning device reducer did not consume the selected query")
		}
		server.Raise(query.rows.Err())
		return stats
	}
	current := reduce(computeStatsDeviceSql)
	candidate := reduce(auditDeviceSinglePassCandidateSql)
	if !maps.Equal(current.DevicesData, candidate.DevicesData) ||
		!maps.Equal(current.activeDevices, candidate.activeDevices) || current.DevicesSummary != candidate.DevicesSummary {
		t.Fatal("device candidate changed daily counts, final active state or summary")
	}
	return candidate, len(candidateRows)
}

// Independent expected counts cover cutoff equality, backdated higher IDs,
// inert removed carry, reassertions, midnight and the existing unbounded end.
func TestComputeStatsDeviceSinglePassCandidateBoundaries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
		day := func(n int) string { return cutoff.AddDate(0, 0, n).Format(time.DateOnly) }
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"))
			stats, selected := auditDeviceCandidateParity(t, ctx, tx, cutoff, day(5))
			if selected != 0 || len(stats.DevicesData) != 6 || len(stats.activeDevices) != 0 || stats.DevicesSummary != 0 {
				t.Fatal("empty device history changed")
			}
			for n := 0; n <= 5; n++ {
				if value, ok := stats.DevicesData[day(n)]; !ok || value != 0 {
					t.Fatalf("empty device history day %d = %d, present=%v", n, value, ok)
				}
			}
			insertAuditStatsTestEvents(ctx, tx, []auditStatsTestEvent{
				{id: 1, device: 1, at: cutoff.AddDate(0, 0, -30), kind: AuditEventTypeDeviceAdded},
				{id: 2, device: 2, at: cutoff.AddDate(0, 0, -20), kind: AuditEventTypeDeviceAdded},
				{id: 3, device: 2, at: cutoff.AddDate(0, 0, -10), kind: AuditEventTypeDeviceRemoved},
				{id: 4, device: 3, at: cutoff.Add(-time.Hour), kind: AuditEventTypeDeviceAdded},
				{id: 5, device: 3, at: cutoff.Add(time.Hour), kind: AuditEventTypeDeviceRemoved},
				{id: 6, device: 4, at: cutoff, kind: AuditEventTypeDeviceRemoved},
				{id: 7, device: 5, at: cutoff.Add(-time.Microsecond), kind: AuditEventTypeDeviceRemoved},
				{id: 8, device: 6, at: cutoff.Add(59*time.Hour + 50*time.Minute), kind: AuditEventTypeDeviceAdded},
				{id: 9, device: 6, at: cutoff.Add(60*time.Hour + 10*time.Minute), kind: AuditEventTypeDeviceRemoved},
				{id: 10, device: 7, at: cutoff.Add(4*24*time.Hour + 6*time.Hour), kind: AuditEventTypeDeviceRemoved},
				{id: 11, device: 7, at: cutoff.Add(4*24*time.Hour - 4*time.Hour), kind: AuditEventTypeDeviceAdded},
				{id: 12, device: 7, at: cutoff.Add(4*24*time.Hour + 7*time.Hour), kind: AuditEventTypeNetworkDeleted},
				{id: 13, device: 8, at: cutoff.Add(24 * time.Hour), kind: AuditEventTypeNetworkCreated},
				{id: 14, device: 9, at: cutoff.AddDate(0, 0, -180), kind: AuditEventTypeDeviceAdded},
				{id: 15, device: 9, at: cutoff.AddDate(0, 0, -30), kind: AuditEventTypeDeviceAdded},
				{id: 16, device: 10, at: cutoff.AddDate(0, 0, 8), kind: AuditEventTypeDeviceAdded},
				{id: 17, device: 10, at: cutoff.AddDate(0, 0, 8).Add(time.Hour), kind: AuditEventTypeDeviceRemoved},
				{id: 18, device: 11, at: cutoff.Add(time.Microsecond), kind: AuditEventTypeDeviceRemoved},
				{id: 19, device: 12, at: cutoff.Add(-2 * time.Hour), kind: AuditEventTypeDeviceAdded},
				{id: 20, device: 12, at: cutoff.Add(-3 * time.Hour), kind: AuditEventTypeDeviceRemoved},
			})
			stats, _ = auditDeviceCandidateParity(t, ctx, tx, cutoff, day(5))
			for n, want := range []int{5, 2, 3, 3, 3, 3, 3, 3, 4} {
				if got, ok := stats.DevicesData[day(n)]; !ok || got != want {
					t.Fatalf("device candidate day %d = %d, present=%v, want=%d", n, got, ok, want)
				}
			}
			wantActive := map[server.Id]bool{auditStatsTestId(1): true, auditStatsTestId(7): true, auditStatsTestId(9): true}
			if len(stats.DevicesData) != 9 || !maps.Equal(stats.activeDevices, wantActive) || stats.DevicesSummary != 4 {
				t.Fatal("device candidate changed carried state or future-timestamp behavior")
			}
		})
	})
}

// Actual plan counters retain the physical scan and spill evidence. Root
// buffer counters are inclusive; summing those over children double-counts.
type auditDeviceCandidatePlan struct {
	NodeType       string                     `json:"Node Type"`
	Relation       string                     `json:"Relation Name,omitempty"`
	Index          string                     `json:"Index Name,omitempty"`
	EstimatedRows  float64                    `json:"Plan Rows"`
	Rows           float64                    `json:"Actual Rows"`
	Loops          float64                    `json:"Actual Loops"`
	Filtered       float64                    `json:"Rows Removed by Filter"`
	Rechecked      float64                    `json:"Rows Removed by Index Recheck"`
	HeapFetches    float64                    `json:"Heap Fetches"`
	SharedHits     int64                      `json:"Shared Hit Blocks"`
	SharedReads    int64                      `json:"Shared Read Blocks"`
	SharedDirtied  int64                      `json:"Shared Dirtied Blocks"`
	SharedWritten  int64                      `json:"Shared Written Blocks"`
	LocalHits      int64                      `json:"Local Hit Blocks"`
	LocalReads     int64                      `json:"Local Read Blocks"`
	LocalDirtied   int64                      `json:"Local Dirtied Blocks"`
	LocalWritten   int64                      `json:"Local Written Blocks"`
	TempReads      int64                      `json:"Temp Read Blocks"`
	TempWrites     int64                      `json:"Temp Written Blocks"`
	HashAggBatches int64                      `json:"HashAgg Batches"`
	PeakMemoryKiB  int64                      `json:"Peak Memory Usage"`
	DiskKiB        int64                      `json:"Disk Usage"`
	SortMethod     string                     `json:"Sort Method,omitempty"`
	SortSpaceType  string                     `json:"Sort Space Type,omitempty"`
	SortSpaceKiB   int64                      `json:"Sort Space Used"`
	WorkersPlanned int64                      `json:"Workers Planned"`
	WorkersStarted int64                      `json:"Workers Launched"`
	Plans          []auditDeviceCandidatePlan `json:"Plans,omitempty"`
}

// Work is kept separate from elapsed time; the latter is descriptive only.
type auditDeviceCandidateEvidence struct {
	Plan         auditDeviceCandidatePlan `json:"Plan"`
	ExecutionMs  float64                  `json:"Execution Time"`
	GenericPlans int64                    `json:"generic_plans"`
	CustomPlans  int64                    `json:"custom_plans"`
}

type auditDeviceCandidateWork struct {
	Scans          int     `json:"scans"`
	Sequential     int     `json:"sequential_scans"`
	IndexOnly      int     `json:"index_only_scans"`
	Covering       int     `json:"owning_covering_index_only_scans"`
	Visited        float64 `json:"visited_rows"`
	HeapFetches    float64 `json:"heap_fetches"`
	Buffers        int64   `json:"shared_and_local_blocks"`
	TempReads      int64   `json:"temp_read_blocks"`
	TempWrites     int64   `json:"temp_written_blocks"`
	DiskKiB        int64   `json:"aggregate_and_sort_disk_kib"`
	MaxHashBatches int64   `json:"maximum_hash_batches"`
	Workers        int64   `json:"workers_planned_or_started"`
}

// Relation visits allow an honest N-to-N comparison when complementary index
// ranges already avoid reading one another's rows.
func (self auditDeviceCandidatePlan) work() auditDeviceCandidateWork {
	work := auditDeviceCandidateWork{
		Buffers:   self.SharedHits + self.SharedReads + self.LocalHits + self.LocalReads,
		TempReads: self.TempReads, TempWrites: self.TempWrites,
	}
	var visit func(auditDeviceCandidatePlan)
	visit = func(node auditDeviceCandidatePlan) {
		if node.Relation == "audit_device_event" {
			work.Scans++
			work.Visited += (node.Rows + node.Filtered + node.Rechecked) * node.Loops
			work.HeapFetches += node.HeapFetches * node.Loops
			if node.NodeType == "Seq Scan" {
				work.Sequential++
			}
			if node.NodeType == "Index Only Scan" {
				work.IndexOnly++
				if node.Index == "audit_device_event_stats_device_id" {
					work.Covering++
				}
			}
		}
		work.MaxHashBatches = max(work.MaxHashBatches, node.HashAggBatches)
		work.DiskKiB += node.DiskKiB
		if node.SortSpaceType == "Disk" {
			work.DiskKiB += node.SortSpaceKiB
		}
		work.Workers += node.WorkersPlanned + node.WorkersStarted
		for _, child := range node.Plans {
			visit(child)
		}
	}
	visit(self)
	return work
}

// The cost oracle accepts ties, but rejects an unexplained buffer/spill
// regression. A forced sequential gain alone cannot qualify production use.
func auditDeviceCandidateCost(current, candidate auditDeviceCandidateWork) (improved bool, regressions []string) {
	if candidate.Visited > current.Visited {
		regressions = append(regressions, "visited_rows")
	}
	for _, metric := range []struct {
		name               string
		current, candidate int64
	}{
		{name: "buffers", current: current.Buffers, candidate: candidate.Buffers},
		{name: "temp_reads", current: current.TempReads, candidate: candidate.TempReads},
		{name: "temp_writes", current: current.TempWrites, candidate: candidate.TempWrites},
		{name: "aggregate_and_sort_disk", current: current.DiskKiB, candidate: candidate.DiskKiB},
	} {
		if metric.candidate > metric.current {
			regressions = append(regressions, metric.name)
		}
		improved = improved || metric.candidate < metric.current
	}
	return improved || candidate.Visited < current.Visited, regressions
}

// A complementary-index tie is valid; an additional spill must stay visible.
func TestComputeStatsDeviceSinglePassCandidateCostOracle(t *testing.T) {
	current := auditDeviceCandidateWork{Visited: 100, Buffers: 20, TempReads: 3, TempWrites: 4, DiskKiB: 8}
	if improved, regressions := auditDeviceCandidateCost(current, current); improved || len(regressions) != 0 {
		t.Fatal("equal work incorrectly classified as a gain or regression")
	}
	candidate := current
	candidate.Buffers--
	if improved, regressions := auditDeviceCandidateCost(current, candidate); !improved || len(regressions) != 0 {
		t.Fatal("equal visited rows with fewer buffers must remain eligible")
	}
	candidate.TempWrites++
	if _, regressions := auditDeviceCandidateCost(current, candidate); len(regressions) != 1 || regressions[0] != "temp_writes" {
		t.Fatal("a buffer gain hid increased temporary writes")
	}
}

// All helper transactions begin on the caller's one direct connection. No
// transaction or connection remains held across fixture commit and vacuum.
func auditDeviceCandidateTx(ctx context.Context, conn server.PgConn, callback func(server.PgTx)) {
	tx, err := conn.Begin(ctx)
	server.Raise(err)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	callback(tx)
	server.Raise(tx.Commit(ctx))
}

// Explicit prepare/execute is required: explain-select alone does not prove
// the application's cached custom/generic prepared plan was exercised.
func auditDeviceCandidateExplain(t testing.TB, ctx context.Context, tx server.PgTx, sql string, cutoff time.Time, mode string) auditDeviceCandidateEvidence {
	t.Helper()
	positionalSql := strings.NewReplacer(
		"@windowStart", "$1", "@startDay", "$2",
		"@eventTypeDeviceAdded", "$3", "@eventTypeDeviceRemoved", "$4",
	).Replace(sql)
	server.RaisePgResult(tx.Exec(ctx, `PREPARE audit_device_candidate_plan(timestamp,text,text,text) AS `+positionalSql))
	// Only fixed test timestamps and the two source constants enter literals.
	call := fmt.Sprintf(`EXECUTE audit_device_candidate_plan(timestamp '%s','%s','%s','%s')`,
		cutoff.Format("2006-01-02 15:04:05.999999"), cutoff.Format(time.DateOnly), AuditEventTypeDeviceAdded, AuditEventTypeDeviceRemoved)
	var raw []byte
	server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+call, pgx.QueryExecModeExec).Scan(&raw))
	if len(raw) > 128*1024 {
		t.Fatal("device candidate plan exceeded its bounded capture")
	}
	var plans []auditDeviceCandidateEvidence
	server.Raise(json.Unmarshal(raw, &plans))
	if len(plans) != 1 {
		t.Fatal("device candidate explain did not return one plan")
	}
	evidence := plans[0]
	server.Raise(tx.QueryRow(ctx, `SELECT generic_plans,custom_plans FROM pg_prepared_statements
		WHERE name='audit_device_candidate_plan'`).Scan(&evidence.GenericPlans, &evidence.CustomPlans))
	wantGeneric := int64(0)
	if mode == "force_generic_plan" {
		wantGeneric = 1
	}
	if evidence.GenericPlans != wantGeneric || evidence.CustomPlans != 1-wantGeneric {
		t.Fatalf("prepared plan mode %s not exercised: generic=%d custom=%d", mode, evidence.GenericPlans, evidence.CustomPlans)
	}
	server.RaisePgResult(tx.Exec(ctx, `DEALLOCATE audit_device_candidate_plan`))
	return evidence
}

// The same prepared covering-index plan runs before and after committed
// updates, before analyze can invalidate it. This establishes heap visibility
// separately from the candidate's unforced performance comparisons.
func auditDeviceCandidateVisibility(t testing.TB, ctx context.Context, conn server.PgConn, changed bool) {
	t.Helper()
	auditDeviceCandidateTx(ctx, conn, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan=off;
			SET LOCAL enable_bitmapscan=off; SET LOCAL enable_indexscan=on;
			SET LOCAL enable_indexonlyscan=on; SET LOCAL max_parallel_workers_per_gather=0;
			SET LOCAL jit=off; SET LOCAL statement_timeout='30s'; SET LOCAL plan_cache_mode=force_generic_plan`))
		if !changed {
			server.RaisePgResult(tx.Exec(ctx, `PREPARE audit_device_visibility_plan AS
				SELECT event_time,device_id,event_id,event_type FROM audit_device_event
				WHERE event_type IN ('device_added','device_removed')
				ORDER BY event_time,device_id,event_id`))
		}
		var raw []byte
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON)
			EXECUTE audit_device_visibility_plan`, pgx.QueryExecModeExec).Scan(&raw))
		var plans []auditDeviceCandidateEvidence
		server.Raise(json.Unmarshal(raw, &plans))
		if len(plans) != 1 {
			t.Fatal("missing visibility control plan")
		}
		work := plans[0].Plan.work()
		if work.Scans != 1 || work.IndexOnly != 1 || work.Covering != 1 || work.Workers != 0 || (work.HeapFetches > 0) != changed {
			t.Fatalf("visibility control changed=%v scans=%d index_only=%d owning_covering=%d heap_fetches=%.0f workers=%d",
				changed, work.Scans, work.IndexOnly, work.Covering, work.HeapFetches, work.Workers)
		}
		wire, err := json.Marshal(struct {
			Changed bool                         `json:"changed_rows"`
			Work    auditDeviceCandidateWork     `json:"work"`
			Plan    auditDeviceCandidateEvidence `json:"evidence"`
		}{Changed: changed, Work: work, Plan: plans[0]})
		server.Raise(err)
		t.Logf("DEVICE_HISTORY_VISIBILITY %s", wire)
		if changed {
			server.RaisePgResult(tx.Exec(ctx, `DEALLOCATE audit_device_visibility_plan`))
		}
	})
}

// One 48,488-row migrated table spans 180 days with repeated connections,
// reassertions, discarded event types and old disconnected devices. Query
// execution is serial, with 64kB/4MB work_mem, no JIT or parallel workers.
// Every unforced cell is captured before cost regressions fail the candidate.
func TestComputeStatsDeviceSinglePassCandidateNativeWork(t *testing.T) {
	if testing.Short() {
		t.Skip("committed device-history candidate qualification")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
		defer cancel()
		origin := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		events := make([]auditStatsTestEvent, 0, 48488)
		appendEvent := func(device uint64, at time.Time, kind AuditEventType) {
			events = append(events, auditStatsTestEvent{id: uint64(len(events) + 1), device: device, at: at, kind: kind})
		}
		for day := 0; day < 180; day++ {
			for device := uint64(1); device <= 128; device++ {
				at := origin.AddDate(0, 0, day).Add(-4 * time.Hour)
				appendEvent(device, at, AuditEventTypeDeviceAdded)
				kind := AuditEventTypeDeviceRemoved
				if device%8 == 0 {
					kind = AuditEventTypeDeviceAdded
				}
				appendEvent(device, at.Add(10*time.Hour), kind)
			}
			appendEvent(1, origin.AddDate(0, 0, day), AuditEventTypeNetworkDeleted)
			appendEvent(129, origin.AddDate(0, 0, day), AuditEventTypeNetworkCreated)
		}
		for device := uint64(1000); device < 2024; device++ {
			appendEvent(device, origin.Add(-time.Hour), AuditEventTypeDeviceAdded)
			appendEvent(device, origin, AuditEventTypeDeviceRemoved)
		}
		if len(events) != 48488 {
			t.Fatalf("device fixture cardinality=%d, want=48488", len(events))
		}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			// The test environment owns this migrated disposable database.
			// Autovacuum must not erase the changed-row visibility control.
			auditDeviceCandidateTx(ctx, conn, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE audit_device_event SET (autovacuum_enabled=false)`))
				insertAuditStatsTestEvents(ctx, tx, events)
				server.RaisePgResult(tx.Exec(ctx, `UPDATE audit_device_event
					SET event_details=repeat('synthetic device history ',16)`))
			})
			// No transaction remains active here. The covering index comes
			// from the real migrations, not an index added for the experiment.
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) audit_device_event`))
			var fixtureRows int64
			var coveringValid bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_event`).Scan(&fixtureRows))
			server.Raise(conn.QueryRow(ctx, `SELECT i.indisvalid AND i.indisready AND i.indnkeyatts=3 AND i.indnatts=4
				FROM pg_index i WHERE i.indexrelid='audit_device_event_stats_device_id'::regclass`).Scan(&coveringValid))
			if fixtureRows != int64(len(events)) || !coveringValid {
				t.Fatal("device fixture count or migrated covering index is incorrect")
			}
			var plannerSettings []byte
			server.Raise(conn.QueryRow(ctx, `SELECT json_build_object(
				'server_version_num',current_setting('server_version_num'),
				'block_size',current_setting('block_size'),
				'random_page_cost',current_setting('random_page_cost'),
				'seq_page_cost',current_setting('seq_page_cost'),
				'effective_cache_size',current_setting('effective_cache_size'),
				'hash_mem_multiplier',current_setting('hash_mem_multiplier'),
				'enable_hashagg',current_setting('enable_hashagg'),
				'enable_sort',current_setting('enable_sort'))`).Scan(&plannerSettings))
			t.Logf("DEVICE_HISTORY_PLANNER_SETTINGS %s", plannerSettings)
			var costFailures []string
			improvedCells, cells := 0, 0
			for _, changed := range []bool{false, true} {
				if changed {
					// Committed updates dirty heap pages without changing query
					// inputs. Analyze refreshes stats but does not vacuum them.
					auditDeviceCandidateTx(ctx, conn, func(tx server.PgTx) {
						tag, err := tx.Exec(ctx, `UPDATE audit_device_event SET event_details=event_details || ' changed'
							WHERE get_byte(uuid_send(event_id),15)%8=0`)
						server.Raise(err)
						if tag.RowsAffected() != fixtureRows/8 {
							t.Fatal("device visibility update population changed")
						}
					})
				}
				auditDeviceCandidateVisibility(t, ctx, conn, changed)
				if changed {
					server.RaisePgResult(conn.Exec(ctx, `ANALYZE audit_device_event`))
				}
				for _, geometry := range []struct {
					name string
					day  int
				}{
					{name: "recent_heavy", day: 4},
					{name: "balanced", day: 90},
					{name: "carry_heavy", day: 176},
				} {
					cutoff := origin.AddDate(0, 0, geometry.day)
					for _, memory := range []string{"64kB", "4MB"} {
						for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
							cells++
							label := fmt.Sprintf("changed=%v/%s/%s/%s", changed, geometry.name, memory, mode)
							auditDeviceCandidateTx(ctx, conn, func(tx server.PgTx) {
								server.RaisePgResult(tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'; SET LOCAL jit=off;
									SET LOCAL max_parallel_workers_per_gather=0; SET LOCAL statement_timeout='30s';
									SET LOCAL lock_timeout='2s'; SET LOCAL enable_seqscan=on; SET LOCAL enable_indexscan=on;
									SET LOCAL enable_indexonlyscan=on; SET LOCAL enable_bitmapscan=on`))
								server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('work_mem',$1,true),set_config('plan_cache_mode',$2,true)`, memory, mode))
								_, selected := auditDeviceCandidateParity(t, ctx, tx, cutoff, origin.AddDate(0, 0, 181).Format(time.DateOnly))
								var carryRows, recentRows int64
								server.Raise(tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE event_time<$1),
									count(*) FILTER(WHERE event_time >= $1) FROM audit_device_event
									WHERE event_type IN ($2,$3)`, cutoff, AuditEventTypeDeviceAdded, AuditEventTypeDeviceRemoved).Scan(&carryRows, &recentRows))
								var current, candidate auditDeviceCandidateEvidence
								// Alternating order avoids attributing a fixed cache
								// warmup advantage to the candidate's elapsed time.
								if cells%2 == 0 {
									candidate = auditDeviceCandidateExplain(t, ctx, tx, auditDeviceSinglePassCandidateSql, cutoff, mode)
									current = auditDeviceCandidateExplain(t, ctx, tx, computeStatsDeviceSql, cutoff, mode)
								} else {
									current = auditDeviceCandidateExplain(t, ctx, tx, computeStatsDeviceSql, cutoff, mode)
									candidate = auditDeviceCandidateExplain(t, ctx, tx, auditDeviceSinglePassCandidateSql, cutoff, mode)
								}
								before, after := current.Plan.work(), candidate.Plan.work()
								if before.Scans != 2 || after.Scans != 1 || before.Workers != 0 || after.Workers != 0 {
									t.Fatalf("%s uncontrolled query shape: scans=%d->%d workers=%d->%d", label, before.Scans, after.Scans, before.Workers, after.Workers)
								}
								if current.Plan.Rows != float64(selected) || candidate.Plan.Rows != float64(selected) {
									t.Fatalf("%s prepared result count differs from selected row parity", label)
								}
								improved, regressions := auditDeviceCandidateCost(before, after)
								if improved && len(regressions) == 0 {
									improvedCells++
								}
								if len(regressions) != 0 {
									costFailures = append(costFailures, label+": "+strings.Join(regressions, ","))
								}
								wire, err := json.Marshal(struct {
									Cell        string                       `json:"cell"`
									FixtureRows int64                        `json:"fixture_rows"`
									CarryRows   int64                        `json:"carry_rows"`
									RecentRows  int64                        `json:"recent_rows"`
									Selected    int                          `json:"selected_rows"`
									CurrentWork auditDeviceCandidateWork     `json:"current_work"`
									NextWork    auditDeviceCandidateWork     `json:"candidate_work"`
									Current     auditDeviceCandidateEvidence `json:"current"`
									Candidate   auditDeviceCandidateEvidence `json:"candidate"`
									Regressions []string                     `json:"cost_regressions"`
								}{Cell: label, FixtureRows: fixtureRows, CarryRows: carryRows, RecentRows: recentRows,
									Selected: selected, CurrentWork: before, NextWork: after,
									Current: current, Candidate: candidate, Regressions: regressions})
								server.Raise(err)
								t.Logf("DEVICE_HISTORY_UNFORCED %s", wire)
							})
						}
					}
				}
			}
			// A separate controlled counterfactual proves removal of the
			// duplicate sequential pass. It is not an unforced gain claim.
			auditDeviceCandidateTx(ctx, conn, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'; SET LOCAL jit=off;
					SET LOCAL max_parallel_workers_per_gather=0; SET LOCAL work_mem='4MB';
					SET LOCAL statement_timeout='30s'; SET LOCAL plan_cache_mode=force_custom_plan;
					SET LOCAL enable_seqscan=on; SET LOCAL enable_indexscan=off;
					SET LOCAL enable_indexonlyscan=off; SET LOCAL enable_bitmapscan=off`))
				cutoff := origin.AddDate(0, 0, 90)
				auditDeviceCandidateParity(t, ctx, tx, cutoff, origin.AddDate(0, 0, 181).Format(time.DateOnly))
				current := auditDeviceCandidateExplain(t, ctx, tx, computeStatsDeviceSql, cutoff, "force_custom_plan")
				candidate := auditDeviceCandidateExplain(t, ctx, tx, auditDeviceSinglePassCandidateSql, cutoff, "force_custom_plan")
				before, after := current.Plan.work(), candidate.Plan.work()
				if before.Scans != 2 || before.Sequential != 2 || before.Visited != float64(2*fixtureRows) ||
					after.Scans != 1 || after.Sequential != 1 || after.Visited != float64(fixtureRows) {
					t.Fatalf("causal sequential control did not distinguish 2N/N: before=%+v after=%+v", before, after)
				}
				wire, err := json.Marshal(struct {
					Current   auditDeviceCandidateEvidence `json:"current"`
					Candidate auditDeviceCandidateEvidence `json:"candidate"`
				}{Current: current, Candidate: candidate})
				server.Raise(err)
				t.Logf("DEVICE_HISTORY_SEQUENTIAL_CONTROL %s", wire)
			})
			t.Logf("DEVICE_HISTORY_COST_VERDICT cells=%d improved_without_regression=%d regressing_cells=%d", cells, improvedCells, len(costFailures))
			if cells != 24 {
				t.Errorf("incomplete device candidate matrix: cells=%d, want=24", cells)
			}
			for _, failure := range costFailures {
				t.Errorf("candidate cost rejection: %s", failure)
			}
			if improvedCells == 0 {
				t.Error("candidate has no unforced work improvement; the sequential control alone is insufficient")
			}
		})
	})
}
