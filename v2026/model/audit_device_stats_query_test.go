package model

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// This is the previous two-range/latest-ID query, with its database clock
// replaced by the test's explicit cutoff. It is an independent row-selection
// and query-work control, not a second production path.
const previousComputeStatsDeviceSql = `
	SELECT t.day, t.device_id, e.event_type, e.event_time < @windowStart AS carry_in
	FROM (
		SELECT to_char(event_time, 'YYYY-MM-DD') AS day, device_id,
			MAX(event_id::varchar) AS max_event_id
		FROM audit_device_event
		WHERE @windowStart <= event_time
			AND event_type IN (@eventTypeDeviceAdded, @eventTypeDeviceRemoved)
		GROUP BY day, device_id
		UNION ALL
		SELECT @startDay AS day, device_id, MAX(event_id::varchar) AS max_event_id
		FROM audit_device_event
		WHERE event_time < @windowStart
			AND event_type IN (@eventTypeDeviceAdded, @eventTypeDeviceRemoved)
		GROUP BY device_id
	) t
	JOIN audit_device_event e ON t.max_event_id::uuid = e.event_id
	ORDER BY day ASC
`

func auditStatsTestId(n uint64) server.Id {
	var id server.Id
	binary.BigEndian.PutUint64(id[8:], n)
	return id
}

type auditStatsTestEvent struct {
	id     uint64
	device uint64
	at     time.Time
	kind   AuditEventType
}

func insertAuditStatsTestEvents(ctx context.Context, tx server.PgTx, events []auditStatsTestEvent) {
	rows := make([][]any, 0, len(events))
	for _, event := range events {
		rows = append(rows, []any{auditStatsTestId(event.id), event.at,
			auditStatsTestId(1), auditStatsTestId(event.device), event.kind, "synthetic audit history"})
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"audit_device_event"},
		[]string{"event_id", "event_time", "network_id", "device_id", "event_type", "event_details"},
		pgx.CopyFromRows(rows))
	server.Raise(err)
}

func auditStatsQueryArgs(cutoff time.Time) server.PgNamedArgs {
	return server.PgNamedArgs{
		"startDay": cutoff.Format(time.DateOnly), "windowStart": cutoff,
		"eventTypeDeviceAdded": AuditEventTypeDeviceAdded, "eventTypeDeviceRemoved": AuditEventTypeDeviceRemoved,
	}
}

func TestComputeStatsDeviceWindowBoundaries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
		day := func(n int) string { return cutoff.AddDate(0, 0, n).Format(time.DateOnly) }
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"))
			stats := &Stats{}
			computeStatsDeviceWindow(ctx, stats, tx, day(0), day(5), cutoff)
			for n := 0; n <= 5; n++ {
				if value, ok := stats.DevicesData[day(n)]; !ok || value != 0 {
					t.Fatalf("empty history day %d = %d, present %v", n, value, ok)
				}
			}
			insertAuditStatsTestEvents(ctx, tx, []auditStatsTestEvent{
				// Carried connected state counts every eventless day.
				{1, 1, cutoff.AddDate(0, 0, -30), AuditEventTypeDeviceAdded},
				// An already disconnected historical device contributes nothing.
				{2, 2, cutoff.AddDate(0, 0, -20), AuditEventTypeDeviceAdded},
				{3, 2, cutoff.AddDate(0, 0, -10), AuditEventTypeDeviceRemoved},
				// The same date has both carry-in and a later in-window removal.
				{4, 3, cutoff.Add(-time.Hour), AuditEventTypeDeviceAdded},
				{5, 3, cutoff.Add(time.Hour), AuditEventTypeDeviceRemoved},
				// A removal exactly on the cutoff touches the first day; one
				// microsecond before it is only an inert disconnected baseline.
				{6, 4, cutoff, AuditEventTypeDeviceRemoved},
				{7, 5, cutoff.Add(-time.Microsecond), AuditEventTypeDeviceRemoved},
				// A connection straddling midnight counts both dates.
				{8, 6, cutoff.Add(59*time.Hour + 50*time.Minute), AuditEventTypeDeviceAdded},
				{9, 6, cutoff.Add(60*time.Hour + 10*time.Minute), AuditEventTypeDeviceRemoved},
				// Preserve event-ID ordering when a later revision backfills an
				// earlier timestamp. MAX(event_time) would give the wrong state.
				{10, 7, cutoff.Add(4*24*time.Hour + 6*time.Hour), AuditEventTypeDeviceRemoved},
				{11, 7, cutoff.Add(4*24*time.Hour - 4*time.Hour), AuditEventTypeDeviceAdded},
				// Other event types do not change this series or win its MAX.
				{12, 7, cutoff.Add(4*24*time.Hour + 7*time.Hour), AuditEventTypeNetworkDeleted},
				{13, 8, cutoff.Add(24 * time.Hour), AuditEventTypeNetworkCreated},
			})
			computeStatsDeviceWindow(ctx, stats, tx, day(0), day(5), cutoff)
			for n, want := range []int{3, 1, 2, 2, 2, 2} {
				if got := stats.DevicesData[day(n)]; got != want {
					t.Fatalf("connected-per-day day %d = %d, want %d", n, got, want)
				}
			}
			wantActive := map[server.Id]bool{auditStatsTestId(1): true, auditStatsTestId(7): true}
			if !maps.Equal(stats.activeDevices, wantActive) || stats.DevicesSummary != 2 {
				t.Fatalf("final state or last-three-day summary changed: active=%d summary=%d", len(stats.activeDevices), stats.DevicesSummary)
			}
		})
	})
}

type auditStatsSelectedRow struct {
	day   string
	id    server.Id
	kind  string
	carry bool
}

func auditStatsSelectedRows(ctx context.Context, tx server.PgTx, sql string, cutoff time.Time) map[auditStatsSelectedRow]int {
	selected := map[auditStatsSelectedRow]int{}
	rows, err := tx.Query(ctx, sql, auditStatsQueryArgs(cutoff))
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var row auditStatsSelectedRow
			server.Raise(rows.Scan(&row.day, &row.id, &row.kind, &row.carry))
			// Dropping an already-removed carry-in row is the only intentional
			// row-set difference. It supplies neither state nor daily evidence.
			if !row.carry || row.kind == AuditEventTypeDeviceAdded {
				selected[row]++
			}
		}
		server.Raise(rows.Err())
	})
	return selected
}

type auditStatsPlan struct {
	NodeType     string           `json:"Node Type"`
	RelationName string           `json:"Relation Name"`
	ActualRows   float64          `json:"Actual Rows"`
	ActualLoops  float64          `json:"Actual Loops"`
	FilteredRows float64          `json:"Rows Removed by Filter"`
	SharedHits   int64            `json:"Shared Hit Blocks"`
	SharedReads  int64            `json:"Shared Read Blocks"`
	TempReads    int64            `json:"Temp Read Blocks"`
	Plans        []auditStatsPlan `json:"Plans"`
}

func auditStatsExplain(ctx context.Context, tx server.PgTx, sql string, cutoff time.Time) auditStatsPlan {
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON, TIMING OFF) "+sql, auditStatsQueryArgs(cutoff))
	var plan []struct {
		Plan auditStatsPlan `json:"Plan"`
	}
	server.WithPgResult(rows, err, func() {
		if !rows.Next() {
			panic("missing audit stats query plan")
		}
		var raw []byte
		server.Raise(rows.Scan(&raw))
		server.Raise(json.Unmarshal(raw, &plan))
	})
	return plan[0].Plan
}

func (p auditStatsPlan) auditWork() (scans int, visited float64) {
	if p.RelationName == "audit_device_event" {
		scans++
		visited += (p.ActualRows + p.FilteredRows) * p.ActualLoops
	}
	for _, child := range p.Plans {
		n, v := child.auditWork()
		scans += n
		visited += v
	}
	return
}

func TestComputeStatsDeviceHistoryParityAndWork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
		events := make([]auditStatsTestEvent, 0, 60000)
		appendEvent := func(device uint64, at time.Time, kind AuditEventType) {
			events = append(events, auditStatsTestEvent{uint64(len(events) + 1), device, at, kind})
		}
		// Sweep-like daily transitions, ongoing connection reassertions and
		// retained disconnected clients. All dates and IDs are deterministic.
		for day := -30; day < 90; day++ {
			for device := uint64(1); device <= 240; device++ {
				at := cutoff.AddDate(0, 0, day).Add(-4 * time.Hour)
				appendEvent(device, at, AuditEventTypeDeviceAdded)
				endKind := AuditEventTypeDeviceRemoved
				if device%7 == 0 {
					endKind = AuditEventTypeDeviceAdded
				}
				appendEvent(device, at.Add(10*time.Hour), endKind)
			}
		}
		for device := uint64(241); device <= 1240; device++ {
			appendEvent(device, cutoff.AddDate(0, 0, -120), AuditEventTypeDeviceAdded)
			appendEvent(device, cutoff.AddDate(0, 0, -119), AuditEventTypeDeviceRemoved)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"))
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL max_parallel_workers_per_gather = 0"))
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL work_mem = '4MB'"))
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL jit = off"))
			insertAuditStatsTestEvents(ctx, tx, events)
			server.RaisePgResult(tx.Exec(ctx, "ANALYZE audit_device_event"))
			previous := auditStatsSelectedRows(ctx, tx, previousComputeStatsDeviceSql, cutoff)
			candidate := auditStatsSelectedRows(ctx, tx, computeStatsDeviceSql, cutoff)
			if !maps.Equal(previous, candidate) {
				t.Fatalf("latest event selection differs: previous=%d candidate=%d", len(previous), len(candidate))
			}
			stats := &Stats{}
			computeStatsDeviceWindow(ctx, stats, tx, cutoff.Format(time.DateOnly), cutoff.AddDate(0, 0, 90).Format(time.DateOnly), cutoff)
			for day := 0; day <= 90; day++ {
				want := 240
				if day == 90 {
					want = 240 / 7 // only the reasserted connections carry onward
				}
				if got := stats.DevicesData[cutoff.AddDate(0, 0, day).Format(time.DateOnly)]; got != want {
					t.Fatalf("history day %d = %d, want %d", day, got, want)
				}
			}
			before := auditStatsExplain(ctx, tx, previousComputeStatsDeviceSql, cutoff)
			after := auditStatsExplain(ctx, tx, computeStatsDeviceSql, cutoff)
			beforeScans, beforeVisited := before.auditWork()
			afterScans, afterVisited := after.auditWork()
			if afterScans != 2 || beforeScans != 3 || afterVisited >= beforeVisited {
				t.Fatalf("historical row lookup not removed: scans %d -> %d, visited %.0f -> %.0f", beforeScans, afterScans, beforeVisited, afterVisited)
			}
			t.Logf("synthetic history rows=%d selected=%d scans=%d->%d visited=%.0f->%.0f shared blocks=%d->%d temp read=%d->%d",
				len(events), len(candidate), beforeScans, afterScans, beforeVisited, afterVisited,
				before.SharedHits+before.SharedReads, after.SharedHits+after.SharedReads, before.TempReads, after.TempReads)
		})
	})
}
