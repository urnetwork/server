// Retained terminal rows without an epoch timestamp are missing custody
// evidence. They must not disappear from every payout window during cutover.
package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Install a real pre-guard terminal row and retain its missing close time when
// applying the prospective writer/archive/revision migrations. No guard is
// disabled and no old immutable proof is rewritten after migration.
func seedMissingUsageTimeBeforeCutover(t testing.TB, ctx context.Context, outcome ContractOutcome, positive bool) (server.Id, time.Time) {
	t.Helper()
	server.ApplyDbMigrationsUpTo(ctx, 744)
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var snapshot *contractUsageSnapshot
	if positive {
		snapshot = &contractUsageSnapshot{Version: 1, ByteCount: 121,
			Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 121}}}
	}
	id := addStContractUsageSnapshotTestOutcome(t, ctx, start, snapshot, outcome)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET close_time=NULL WHERE contract_id=$1`, id))
	})
	server.ApplyDbMigrations(ctx)
	return id, start
}

// A large unplaceable history must return bounded debt evidence rather than
// streaming every unknown contract or walking ordinary open/canceled rows.
// Exercise the actual planner with installed indexes and real archive capture.
func TestStContractUsageMissingCloseTimeProbeStaysBounded(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,outcome)
				SELECT gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1000,'settled'
				FROM generate_series(1,2500)`))
		})
		server.ApplyDbMigrations(ctx)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id IN
				(SELECT contract_id FROM transfer_contract ORDER BY contract_id LIMIT 1250)`))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract; ANALYZE st_provider_usage_archive`))
			start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			rows, err := conn.Query(ctx, stEpochProviderUsageSql, start, start.Add(time.Hour))
			server.Raise(err)
			count := 0
			for rows.Next() {
				count++
			}
			server.Raise(rows.Err())
			rows.Close()
			if count != 2 {
				t.Fatalf("debt probe returned %d rows, want one per store", count)
			}
			var raw []byte
			server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+stEpochProviderUsageSql, start, start.Add(time.Hour)).Scan(&raw))
			var plan any
			server.Raise(json.Unmarshal(raw, &plan))
			indexes := map[string]bool{}
			var visit func(any)
			visit = func(value any) {
				switch node := value.(type) {
				case map[string]any:
					if name, ok := node["Index Name"].(string); ok && node["Actual Loops"].(float64) > 0 {
						indexes[name] = true
					}
					for _, child := range node {
						visit(child)
					}
				case []any:
					for _, child := range node {
						visit(child)
					}
				}
			}
			visit(plan)
			for _, name := range []string{"transfer_contract_usage_missing_time", "st_provider_usage_archive_close_time"} {
				if !indexes[name] {
					t.Fatalf("missing-time probe lost %s: %s", name, raw)
				}
			}
		})
	})
}

// A dated positive neighbor must not hide unplaceable terminal history. Both
// time-window and explicit-epoch payout consumers must fail without partial rows.
func TestStContractUsageMissingCloseTimeBlocksLiveCutover(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		id, start := seedMissingUsageTimeBeforeCutover(t, ctx, ContractOutcomeSettled, true)
		addStContractUsageSnapshotTestRow(t, ctx, start, &contractUsageSnapshot{Version: 1, ByteCount: 73,
			Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 73}}})
		for _, epoch := range []uint64{0, 901} {
			usage, err := getStEpochProviderUsage(ctx, epoch, start, start.Add(time.Hour))
			if err == nil || usage != nil || !strings.Contains(err.Error(), id.String()) || !strings.Contains(err.Error(), "close time") {
				t.Fatalf("epoch %d hid unplaceable live contract %s: usage=%+v error=%v", epoch, id, usage, err)
			}
		}
		networks, err := GetStEpochNetworkUsage(ctx, start, start.Add(time.Hour))
		if err == nil || networks != nil || !strings.Contains(err.Error(), id.String()) {
			t.Fatalf("legacy network payout hid missing custody: usage=%+v error=%v", networks, err)
		}
	})
}

// Deletion atomically moves the same debt into immutable archive custody. It
// must not make an empty epoch look complete merely by removing the live row.
func TestStContractUsageMissingCloseTimeSurvivesArchive(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		id, start := seedMissingUsageTimeBeforeCutover(t, ctx, ContractOutcomeSettled, false)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
		})
		server.Db(ctx, func(conn server.PgConn) {
			var archived int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_provider_usage_archive
				WHERE contract_id=$1 AND close_time IS NULL AND provider_usage IS NULL`, id).Scan(&archived))
			if archived != 1 {
				t.Fatal("migration/retention did not retain exact missing evidence")
			}
		})
		usage, err := GetStEpochProviderUsageAtEpoch(ctx, 902, start, start.Add(time.Hour))
		if err == nil || usage != nil || !strings.Contains(err.Error(), id.String()) || !strings.Contains(err.Error(), "close time") {
			t.Fatalf("retention forgave unknown archived epoch %s: usage=%+v error=%v", id, usage, err)
		}
	})
}

// The transaction itself is the barrier. A reader outside the deletion sees
// the live debt until commit, including after rollback; committed retention
// exposes exactly the same debt through the archive, never an empty epoch.
func TestStContractUsageMissingCloseTimeRetentionRollback(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		id, start := seedMissingUsageTimeBeforeCutover(t, ctx, ContractOutcomeDisputeResolvedToSource, true)
		assertBlocked := func() {
			t.Helper()
			usage, err := GetStEpochProviderUsageAtEpoch(ctx, 903, start, start.Add(time.Hour))
			if err == nil || usage != nil || !strings.Contains(err.Error(), id.String()) {
				t.Fatalf("retention hid missing time: usage=%+v error=%v", usage, err)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.Begin(ctx)
			server.Raise(err)
			defer tx.Rollback(ctx)
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
			assertBlocked()
			server.Raise(tx.Rollback(ctx))
			assertBlocked()
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
			assertBlocked()
			var live, archived int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_contract WHERE contract_id=$1),
				(SELECT count(*) FROM st_provider_usage_archive WHERE contract_id=$1 AND close_time IS NULL)`, id).Scan(&live, &archived))
			if live != 0 || archived != 1 {
				t.Fatalf("retention lost exact debt: live=%d archived=%d", live, archived)
			}
		})
	})
}

func TestStContractUsageMissingCloseTimeDisputeDestination(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		id, start := seedMissingUsageTimeBeforeCutover(t, ctx, ContractOutcomeDisputeResolvedToDestination, false)
		for _, archive := range []bool{false, true} {
			if archive {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
				})
			}
			usage, err := GetStEpochProviderUsageAtEpoch(ctx, 904, start.Add(-time.Hour), start)
			if err == nil || usage != nil || !strings.Contains(err.Error(), id.String()) {
				t.Fatalf("archive=%t: missing time assumed outside epoch: usage=%+v error=%v", archive, usage, err)
			}
		}
	})
}

// Open/canceled contracts do not own credited work. Their absent timestamps
// cannot disable a complete epoch, and the ordinary half-open window remains.
func TestStContractUsageMissingCloseTimeIgnoresUncreditedOutcomes(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		provider, network := server.NewId(), server.NewId()
		for _, at := range []time.Time{start.Add(-time.Second), start, start.Add(time.Hour)} {
			addStContractUsageSnapshotTestRow(t, ctx, at, &contractUsageSnapshot{Version: 1, ByteCount: 73,
				Providers: []contractProviderUsage{{ClientId: provider, NetworkId: network, ByteCount: 73}}})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, outcome := range []any{nil, "canceled"} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,outcome)
					VALUES($1,$2,$3,$4,$5,1000,$6)`, server.NewId(), server.NewId(), server.NewId(), provider, network, outcome))
			}
		})
		usage, err := GetStEpochProviderUsageAtEpoch(ctx, 905, start, start.Add(time.Hour))
		if err != nil || len(usage) != 1 || usage[0].ClientId != provider || usage[0].PayoutByteCount != 73 {
			t.Fatalf("uncredited null timestamp changed exact epoch: usage=%+v error=%v", usage, err)
		}
	})
}
