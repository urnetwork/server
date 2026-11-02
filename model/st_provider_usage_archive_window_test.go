// The epoch usage read's duplicate probe joins only the archive rows that
// closed in the window: its plan stays bounded as the contract horizon reaper
// grows the archive, and its rows do not change.
package model

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Inserts count settled contracts named prefix-n, closed step milliseconds
// apart from firstClose, each with the same usage snapshot.
func insertStUsageWindowTestContracts(ctx context.Context, prefix string, count int, firstClose time.Time, stepMillis int, network server.Id, snapshot *contractUsageSnapshot) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,
				 transfer_byte_count,create_time,outcome,close_time,provider_usage,usage_origin_is_source)
			SELECT md5($1||'-'||n)::uuid,md5($1||'-source-'||n)::uuid,$2,md5($1||'-destination-'||n)::uuid,$3,
				1000000,$4::timestamp-interval '1 hour','settled',$4::timestamp+n*$5::int*interval '1 millisecond',$6,true
			FROM generate_series(0,$7::int-1) n
		`, prefix, server.NewId(), network, firstClose, stepMillis, snapshot, count))
	}, server.TxReadCommitted, server.OptNoRetry())
}

// Deletes the contracts prefix-n for n in [first, first+count); the capture
// trigger archives each, as the contract horizon reaper does.
func archiveStUsageWindowTestContracts(ctx context.Context, prefix string, first int, count int) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			DELETE FROM transfer_contract
			WHERE contract_id IN (SELECT md5($1||'-'||n)::uuid FROM generate_series($2::int,$2::int+$3::int-1) n)
		`, prefix, first, count))
	}, server.TxReadCommitted, server.OptNoRetry())
}

// With an archive far larger than the window's share, as the contract horizon
// reaper makes it, the whole-window read under Main's costs and its hash join
// (Main, 2026-11-01: a Hash Left Join building on every archive partition)
// reads each archive partition by its close-time index range, hashes only the
// window's archived rows, and keeps them in one batch with work_mem scaled
// down to the archive as Main's is to its own.
func TestStEpochUsageArchiveJoinReadsOnlyTheWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		start, end := base, base.Add(time.Hour)
		network := server.NewId()
		snapshot := &contractUsageSnapshot{Version: 1, ByteCount: 11, Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: network, ByteCount: 11}}}
		// the archive outside the window: 30000 contracts closed before it
		const outside, inside = 30000, 40000
		insertStUsageWindowTestContracts(ctx, "archived-before", outside, base.Add(-3*time.Hour), 200, network, snapshot)
		archiveStUsageWindowTestContracts(ctx, "archived-before", 0, outside)
		// the window: more live contracts than archived ones, as on Main, and
		// three archived inside it
		insertStUsageWindowTestContracts(ctx, "window", inside, base, 50, network, snapshot)
		archiveStUsageWindowTestContracts(ctx, "window", 100, 3)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `VACUUM ANALYZE transfer_contract`))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_provider_usage_archive`))
		})

		var raw []byte
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			server.Raise(err)
			defer tx.Rollback(context.WithoutCancel(ctx))
			for _, setting := range stWholeWindowPlanMainCosts {
				server.RaisePgResult(tx.Exec(ctx, setting))
			}
			server.Raise(stEpochProviderUsageWholeWindowPlanInTx(ctx, tx))
			for _, setting := range []string{
				// Main's hash memory is about 3.66 GB against tens of millions of
				// archive rows; this is the same budget against 30000
				`SET LOCAL work_mem = '64kB'`,
				`SET LOCAL hash_mem_multiplier = 1`,
				// the join Main plans
				`SET LOCAL enable_nestloop = off`,
				`SET LOCAL enable_mergejoin = off`,
			} {
				server.RaisePgResult(tx.Exec(ctx, setting))
			}
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+stEpochProviderUsageSql, start, end).Scan(&raw))
		})
		var plan any
		server.Raise(json.Unmarshal(raw, &plan))

		hashed := false
		archiveRows := 0.0
		for _, node := range stWholeWindowPlanNodes(plan, nil) {
			switch {
			case node.Type == "Hash" && node.HashBatches != 1:
				t.Fatalf("the hash ran in %v batches: %s", node.HashBatches, raw)
			case node.TempBlocks > 0:
				t.Fatalf("the read spilled %v temp blocks in %s: %s", node.TempBlocks, node.Type, raw)
			case node.Type == "Bitmap Index Scan" && strings.HasPrefix(node.Index, "st_provider_usage_archive") && !strings.Contains(node.IndexCond, "close_time"):
				t.Fatalf("the read scanned all of the archive index %s: %s", node.Index, raw)
			}
			hashed = hashed || node.Type == "Hash"
			if node.Type == "Bitmap Heap Scan" && strings.HasPrefix(node.Relation, "st_provider_usage_archive") {
				archiveRows += node.Rows * node.Loops
			}
		}
		if !hashed {
			t.Fatalf("the read joined the archive without a hash, so this test no longer shows the hash's size: %s", raw)
		}
		// the three archived in the window, once by the join and once by the
		// archive arm
		if archiveRows != 6 {
			t.Fatalf("the read fetched %v archive rows; the window holds 3: %s", archiveRows, raw)
		}
	})
}

// One row of the usage statement.
type stUsageWindowTestRow struct {
	ContractId server.Id
	CloseTime  time.Time
	Duplicate  bool
	Usage      string
}

// The duplicate probe reads only the window's archive rows and loses nothing
// by it. The usage statement returns the same rows with and without archive
// rows outside the window, including both rows of a contract that is live and
// archived at once, and a close time exactly at the window's start counts on
// either side while one exactly at its end does not.
func TestStEpochUsageArchiveOutsideTheWindowChangesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
		end := start.Add(7 * 24 * time.Hour)
		network := server.NewId()
		snapshot := &contractUsageSnapshot{Version: 1, ByteCount: 17, Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: network, ByteCount: 17}}}
		insert := func(closeTime time.Time) server.Id {
			contractId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO transfer_contract
						(contract_id,source_id,source_network_id,destination_id,destination_network_id,
						 transfer_byte_count,create_time,outcome,close_time,provider_usage,usage_origin_is_source)
					VALUES($1,$2,$3,$4,$3,1000,$5::timestamp-interval '1 hour','settled',$5,$6,true)
				`, contractId, server.NewId(), network, server.NewId(), closeTime, snapshot))
			}, server.TxReadCommitted, server.OptNoRetry())
			return contractId
		}
		deleteLive := func(contractIds ...server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id = ANY($1::uuid[])`, contractIds))
			}, server.TxReadCommitted, server.OptNoRetry())
		}

		liveAtStart := insert(start)
		liveInside := insert(start.Add(time.Hour))
		liveAtEnd := insert(end)
		liveBeforeEnd := insert(end.Add(-time.Microsecond))
		archivedAtStart := insert(start)
		archivedInside := insert(start.Add(2 * time.Hour))
		archivedAtEnd := insert(end)
		archivedBeforeStart := insert(start.Add(-time.Microsecond))
		deleteLive(archivedAtStart, archivedInside, archivedAtEnd, archivedBeforeStart)
		// live and archived at once: the archive guard admits only an exact
		// copy of a live contract
		both := insert(start.Add(3 * time.Hour))
		bothBefore := insert(start.Add(-time.Hour))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO st_provider_usage_archive (contract_id,outcome,close_time,provider_usage)
				SELECT contract_id,outcome,close_time,provider_usage FROM transfer_contract WHERE contract_id = ANY($1::uuid[])
			`, []server.Id{both, bothBefore}))
		}, server.TxReadCommitted, server.OptNoRetry())

		read := func() []stUsageWindowTestRow {
			rows := []stUsageWindowTestRow{}
			server.Db(ctx, func(conn server.PgConn) {
				tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
				server.Raise(err)
				defer tx.Rollback(context.WithoutCancel(ctx))
				result, err := tx.Query(ctx, stEpochProviderUsageSql, start, end)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var row stUsageWindowTestRow
						var closeTime *time.Time
						var usage []byte
						server.Raise(result.Scan(&row.ContractId, &usage, &closeTime, &row.Duplicate))
						if closeTime != nil {
							row.CloseTime = closeTime.UTC()
						}
						row.Usage = string(usage)
						rows = append(rows, row)
					}
				})
			})
			slices.SortFunc(rows, func(a stUsageWindowTestRow, b stUsageWindowTestRow) int {
				if c := a.ContractId.Cmp(b.ContractId); c != 0 {
					return c
				}
				if a.Duplicate == b.Duplicate {
					return 0
				}
				if a.Duplicate {
					return 1
				}
				return -1
			})
			return rows
		}

		got := read()
		usage := ""
		if 0 < len(got) {
			usage = got[0].Usage
		}
		want := []stUsageWindowTestRow{
			{ContractId: liveAtStart, CloseTime: start, Usage: usage},
			{ContractId: liveInside, CloseTime: start.Add(time.Hour), Usage: usage},
			{ContractId: liveBeforeEnd, CloseTime: end.Add(-time.Microsecond), Usage: usage},
			{ContractId: archivedAtStart, CloseTime: start, Usage: usage},
			{ContractId: archivedInside, CloseTime: start.Add(2 * time.Hour), Usage: usage},
			{ContractId: both, CloseTime: start.Add(3 * time.Hour), Usage: usage},
			{ContractId: both, CloseTime: start.Add(3 * time.Hour), Duplicate: true, Usage: usage},
		}
		slices.SortFunc(want, func(a stUsageWindowTestRow, b stUsageWindowTestRow) int {
			if c := a.ContractId.Cmp(b.ContractId); c != 0 {
				return c
			}
			if a.Duplicate == b.Duplicate {
				return 0
			}
			if a.Duplicate {
				return 1
			}
			return -1
		})
		if usage == "" || !slices.Equal(got, want) {
			t.Fatalf("the window read %+v, want %+v (excluded: %s at the end, %s and %s outside)", got, want, liveAtEnd, archivedAtEnd, archivedBeforeStart)
		}

		// archive rows outside the window on both sides
		insertStUsageWindowTestContracts(ctx, "earlier", 1000, start.Add(-24*time.Hour), 60000, network, snapshot)
		archiveStUsageWindowTestContracts(ctx, "earlier", 0, 1000)
		insertStUsageWindowTestContracts(ctx, "later", 1000, end, 60000, network, snapshot)
		archiveStUsageWindowTestContracts(ctx, "later", 0, 1000)
		if again := read(); !slices.Equal(again, got) {
			t.Fatalf("archive rows outside the window changed the read: %+v, was %+v", again, got)
		}
	})
}
