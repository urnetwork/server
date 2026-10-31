// A window holding more closed contracts than the census can record retains
// no record, so its read is the usage alone. Every result, refusal and census
// count must match the pre-change read (st_provider_usage_original_control_test.go).
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
)

// Nine providers on six networks; every fifth contract pays two of them and
// every eleventh is an expired zero-credit original.
type stWholeWindowTestProviders struct {
	clients  []server.Id
	networks []server.Id
}

func newStWholeWindowTestProviders() *stWholeWindowTestProviders {
	providers := &stWholeWindowTestProviders{}
	networks := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()}
	for i := range 9 {
		providers.clients = append(providers.clients, server.NewId())
		providers.networks = append(providers.networks, networks[i%len(networks)])
	}
	return providers
}

// Inserts count credited contracts closed at distinct times in [start, start+1h),
// cycling the three credited outcomes, with ids derived from prefix.
func (self *stWholeWindowTestProviders) seed(t testing.TB, ctx context.Context, prefix string, start time.Time, count int) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,
				 transfer_byte_count,create_time,outcome,close_time,provider_usage,usage_origin_is_source)
			SELECT md5($1||'-'||n)::uuid,$2,$2,$3,$3,1000000,$4::timestamp-interval '1 hour',
				(ARRAY['settled','dispute_resolved_to_source','dispute_resolved_to_destination'])[n%3+1],
				$4::timestamp+(n%3600)*interval '1 second'+(n%997)*interval '1 microsecond',
				CASE
				WHEN n%11=0 THEN '{"version":1,"byte_count":0,"providers":[],"excluded_reason":"expired_unconfirmed","expiry":{"capacity":1048576,"reports":{"source":{"byte_count":0,"checkpoint":false}}}}'::jsonb
				WHEN n%5=0 THEN jsonb_build_object('version',1,'byte_count',n%1000+1,'providers',
					CASE WHEN ($6::uuid[])[n%9+1]<($6::uuid[])[(n+4)%9+1]
					THEN jsonb_build_array(
						jsonb_build_object('client_id',($6::uuid[])[n%9+1]::text,'network_id',($7::uuid[])[n%9+1]::text,'byte_count',(n%1000+1)/2),
						jsonb_build_object('client_id',($6::uuid[])[(n+4)%9+1]::text,'network_id',($7::uuid[])[(n+4)%9+1]::text,'byte_count',n%1000+1-(n%1000+1)/2))
					ELSE jsonb_build_array(
						jsonb_build_object('client_id',($6::uuid[])[(n+4)%9+1]::text,'network_id',($7::uuid[])[(n+4)%9+1]::text,'byte_count',(n%1000+1)/2),
						jsonb_build_object('client_id',($6::uuid[])[n%9+1]::text,'network_id',($7::uuid[])[n%9+1]::text,'byte_count',n%1000+1-(n%1000+1)/2))
					END)
				ELSE jsonb_build_object('version',1,'byte_count',n%1000+1,'providers',jsonb_build_array(
					jsonb_build_object('client_id',($6::uuid[])[n%9+1]::text,'network_id',($7::uuid[])[n%9+1]::text,'byte_count',n%1000+1)))
				END,true
			FROM generate_series(1,$5::int) n
		`, prefix, server.NewId(), server.NewId(), start, count, self.clients, self.networks))
	})
}

// Inserts one credited contract with an exact usage snapshot.
func addStWholeWindowTestRow(t testing.TB, ctx context.Context, closeTime time.Time, snapshot any) server.Id {
	t.Helper()
	contractId := server.NewId()
	data, err := json.Marshal(snapshot)
	server.Raise(err)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (contract_id,source_id,source_network_id,destination_id,destination_network_id,
			transfer_byte_count,outcome,close_time,provider_usage,usage_origin_is_source)
			VALUES($1,$2,$2,$3,$3,1000000,'settled',$4,$5::jsonb,true)
		`, contractId, server.NewId(), server.NewId(), closeTime, data))
	})
	return contractId
}

// Retention moves the exact credited rows into archive custody.
func archiveStWholeWindowTestRows(t testing.TB, ctx context.Context, prefix string, ns ...int) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, n := range ns {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=md5($1||'-'||$2::int)::uuid`, prefix, n))
		}
	})
}

// The half-open window [start, end) holding exactly count credited rows.
func stWholeWindowTestEnd(t testing.TB, ctx context.Context, start time.Time, count int) time.Time {
	t.Helper()
	var end time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT close_time FROM (
				SELECT close_time FROM transfer_contract WHERE $1<=close_time
					AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
				UNION ALL
				SELECT close_time FROM st_provider_usage_archive WHERE $1<=close_time
			) AS credited ORDER BY close_time OFFSET $2 LIMIT 1
		`, start, count).Scan(&end))
	})
	return end
}

// Credited bytes per provider in [start, end), computed by SQL alone from live
// and archived originals. An oracle independent of the Go decoder and readers.
func stWholeWindowTestOracle(t testing.TB, ctx context.Context, start, end time.Time) map[server.Id]int64 {
	t.Helper()
	totals := map[server.Id]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT (provider->>'client_id')::uuid, sum((provider->>'byte_count')::bigint)::bigint FROM (
				SELECT provider_usage FROM transfer_contract WHERE $1<=close_time AND close_time<$2
					AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
				UNION ALL
				SELECT provider_usage FROM st_provider_usage_archive WHERE $1<=close_time AND close_time<$2
			) AS credited CROSS JOIN LATERAL jsonb_array_elements(provider_usage->'providers') AS provider
			GROUP BY 1
		`, start, end)
		server.Raise(err)
		defer rows.Close()
		for rows.Next() {
			var clientId server.Id
			var total int64
			server.Raise(rows.Scan(&clientId, &total))
			totals[clientId] = total
		}
		server.Raise(rows.Err())
	})
	return totals
}

// Everything a census read returns or retains, in comparable form.
type stWholeWindowTestRead struct {
	Usages  string
	Err     string
	Count   uint64
	Records bool
	Hash    string
	Window  string
}

func stWholeWindowTestCompare(t testing.TB, read func(*payoutartifact.ClosedWorkCensus, **payoutartifact.ClosedWorkWindow) ([]*StProviderUsage, error), withCensus bool) stWholeWindowTestRead {
	t.Helper()
	var census *payoutartifact.ClosedWorkCensus
	var window *payoutartifact.ClosedWorkWindow
	var windowRef **payoutartifact.ClosedWorkWindow
	if withCensus {
		census = &payoutartifact.ClosedWorkCensus{Schema: payoutartifact.ClosedWorkSchema}
		windowRef = &window
	}
	usages, err := read(census, windowRef)
	result := stWholeWindowTestRead{}
	usageWire, marshalErr := json.Marshal(usages)
	server.Raise(marshalErr)
	result.Usages = string(usageWire)
	if err != nil {
		// a refused read returns its error alone; its scratch census is discarded
		result.Err = err.Error()
		return result
	}
	if census != nil {
		result.Count = census.Count
		result.Records = census.Records != nil
		if census.Records != nil {
			census.Sort()
			result.Hash = census.Hash()
		}
		windowWire, marshalErr := json.Marshal(window)
		server.Raise(marshalErr)
		result.Window = string(windowWire)
	}
	return result
}

// Reads one window with both the current and the pre-change reader, census and
// plain, and requires identical results. Returns the current census read.
func requireStWholeWindowTestEqual(t testing.TB, ctx context.Context, label string, epoch uint64, start, end time.Time) stWholeWindowTestRead {
	t.Helper()
	for _, withCensus := range []bool{true, false} {
		current := stWholeWindowTestCompare(t, func(census *payoutartifact.ClosedWorkCensus, window **payoutartifact.ClosedWorkWindow) ([]*StProviderUsage, error) {
			return getStEpochProviderUsageWithCensus(ctx, epoch, start, end, census, window)
		}, withCensus)
		original := stWholeWindowTestCompare(t, func(census *payoutartifact.ClosedWorkCensus, window **payoutartifact.ClosedWorkWindow) ([]*StProviderUsage, error) {
			return stEpochProviderUsageOriginalControl(ctx, epoch, start, end, census, window)
		}, withCensus)
		if current != original {
			t.Fatalf("%s (census=%t): current read differs from the original read:\ncurrent  %+v\noriginal %+v", label, withCensus, current, original)
		}
	}
	// the public whole-census reader agrees too, including its sorted census
	usages, census, window, err := GetStEpochProviderUsageWholeCensus(ctx, epoch, start, end)
	originalUsages, originalCensus, originalWindow, originalErr := stEpochProviderUsageWholeCensusControl(ctx, epoch, start, end)
	wire := func(values ...any) []byte {
		data, err := json.Marshal(values)
		server.Raise(err)
		return data
	}
	if fmt.Sprint(err) != fmt.Sprint(originalErr) || !bytes.Equal(wire(usages, census, window), wire(originalUsages, originalCensus, originalWindow)) {
		t.Fatalf("%s: public whole census differs from the original: %v / %v", label, err, originalErr)
	}
	return stWholeWindowTestCompare(t, func(census *payoutartifact.ClosedWorkCensus, window **payoutartifact.ClosedWorkWindow) ([]*StProviderUsage, error) {
		return getStEpochProviderUsageWithCensus(ctx, epoch, start, end, census, window)
	}, true)
}

func requireStWholeWindowTestOracle(t testing.TB, ctx context.Context, label string, start, end time.Time, read stWholeWindowTestRead) {
	t.Helper()
	var usages []*StProviderUsage
	server.Raise(json.Unmarshal([]byte(read.Usages), &usages))
	oracle := stWholeWindowTestOracle(t, ctx, start, end)
	got := map[server.Id]int64{}
	for _, usage := range usages {
		got[usage.ClientId] = usage.PayoutByteCount
	}
	for clientId, total := range oracle {
		if total == 0 {
			continue
		}
		if got[clientId] != total {
			t.Fatalf("%s: provider %s earned %d, the originals hold %d", label, clientId, got[clientId], total)
		}
	}
	for clientId, total := range got {
		if total != oracle[clientId] {
			t.Fatalf("%s: provider %s earned %d without matching originals (%d)", label, clientId, total, oracle[clientId])
		}
	}
}

// Above MaxClosedWorkRecords closed contracts the census retains no record,
// so the read must not wait on, or read, a single close report. A window the
// census can hold still reads every original report exactly as before.
func TestStEpochCensusWholeWindowReadsNoCloseReports(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		end := start.Add(time.Hour)
		providers := newStWholeWindowTestProviders()
		count := payoutartifact.MaxClosedWorkRecords + 1
		providers.seed(t, ctx, "whole-window-reports", start, count)
		smallStart := start.Add(2 * time.Hour)
		for range 3 {
			addStWholeWindowTestRow(t, ctx, smallStart, &contractUsageSnapshot{Version: 1, ByteCount: 7,
				Providers: []contractProviderUsage{{ClientId: providers.clients[0], NetworkId: providers.networks[0], ByteCount: 7}}})
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract`))
		})
		oracle := stWholeWindowTestOracle(t, ctx, start, end)

		server.Db(ctx, func(conn server.PgConn) {
			lock, err := conn.Begin(ctx)
			server.Raise(err)
			defer lock.Rollback(context.WithoutCancel(ctx))
			server.RaisePgResult(lock.Exec(ctx, `LOCK TABLE contract_close_report_evidence, contract_close_report IN ACCESS EXCLUSIVE MODE`))

			readCtx, readCancel := context.WithTimeout(ctx, 60*time.Second)
			usages, census, window, err := GetStEpochProviderUsageWholeCensus(readCtx, 17, start, end)
			readCancel()
			if err != nil {
				t.Fatalf("an overflowing census read waited on close reports it cannot retain: %v", err)
			}
			if census != nil || window != nil || len(usages) != len(oracle) {
				t.Fatalf("an overflowing census retained records or lost usage: census=%v window=%v usages=%d want %d", census != nil, window != nil, len(usages), len(oracle))
			}
			for _, usage := range usages {
				if usage.PayoutByteCount != oracle[usage.ClientId] {
					t.Fatalf("provider %s earned %d, the originals hold %d", usage.ClientId, usage.PayoutByteCount, oracle[usage.ClientId])
				}
			}

			// the complete census still reads its reports, so it waits on them
			smallCtx, smallCancel := context.WithTimeout(ctx, 5*time.Second)
			_, census, _, err = GetStEpochProviderUsageWholeCensus(smallCtx, 17, smallStart, smallStart.Add(time.Hour))
			smallCancel()
			if err == nil || census != nil {
				t.Fatalf("a census that holds its window no longer read its close reports: %v", err)
			}
		})
		usages, census, window, err := GetStEpochProviderUsageWholeCensus(ctx, 17, smallStart, smallStart.Add(time.Hour))
		if err != nil || census == nil || census.Count != 3 || len(census.Records) != 3 || window == nil || len(window.Records) != 3 || len(usages) != 1 || usages[0].PayoutByteCount != 21 {
			t.Fatalf("complete census changed: %+v %+v %+v %v", usages, census, window, err)
		}
	})
}

// Large windows (above the census record bound) and small ones (complete
// windows, the 1025-row window overflow, the empty sentinel) return exactly
// what the original read returns, for the census and the plain reader alike.
func TestStEpochUsageWholeWindowMatchesOriginal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		providers := newStWholeWindowTestProviders()
		count := payoutartifact.MaxClosedWorkRecords + 4000
		providers.seed(t, ctx, "whole-window", start, count)
		// credited work just outside the window on both sides
		outside := newStWholeWindowTestProviders()
		outside.seed(t, ctx, "whole-window-before", start.Add(-time.Hour), 50)
		outside.seed(t, ctx, "whole-window-after", start.Add(time.Hour), 50)
		addStWholeWindowTestRow(t, ctx, start.Add(-time.Microsecond), &contractUsageSnapshot{Version: 1, ByteCount: 5,
			Providers: []contractProviderUsage{{ClientId: providers.clients[1], NetworkId: providers.networks[1], ByteCount: 5}}})
		// archived originals inside and outside the window
		archiveStWholeWindowTestRows(t, ctx, "whole-window", 3, 99, 1000, 2047, 30001, count)
		archiveStWholeWindowTestRows(t, ctx, "whole-window-after", 1, 2)
		// every non-credited disposition
		server.Tx(ctx, func(tx server.PgTx) {
			for i := range 3 {
				at := start.Add(time.Duration(600+i) * time.Second)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,create_time,outcome,close_time)
					VALUES($1,$1,$1,$1,$1,0,$2,NULL,NULL),($3,$3,$3,$3,$3,0,$2,'canceled',NULL),($4,$4,$4,$4,$4,0,$2,'canceled',$2)`,
					server.NewId(), at, server.NewId(), server.NewId()))
			}
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract; ANALYZE st_provider_usage_archive`))
		})

		whole := requireStWholeWindowTestEqual(t, ctx, "whole hour", 17, start, start.Add(time.Hour))
		if whole.Err != "" || whole.Records || whole.Count != uint64(count) {
			t.Fatalf("whole hour: unexpected census %+v", whole)
		}
		requireStWholeWindowTestOracle(t, ctx, "whole hour", start, start.Add(time.Hour), whole)

		for _, rows := range []int{payoutartifact.MaxClosedWorkRecords, payoutartifact.MaxClosedWorkRecords + 1} {
			end := stWholeWindowTestEnd(t, ctx, start, rows)
			read := requireStWholeWindowTestEqual(t, ctx, fmt.Sprintf("%d rows", rows), 17, start, end)
			if read.Err != "" || read.Count != uint64(rows) || read.Records != (rows <= payoutartifact.MaxClosedWorkRecords) {
				t.Fatalf("%d rows: unexpected census %+v", rows, read.Count)
			}
			requireStWholeWindowTestOracle(t, ctx, fmt.Sprintf("%d rows", rows), start, end, read)
		}

		// 1025 credited rows overflow the window inventory, not the census
		overflowEnd := stWholeWindowTestEnd(t, ctx, start, 1100)
		overflow := requireStWholeWindowTestEqual(t, ctx, "window overflow", 17, start, overflowEnd)
		if overflow.Err != "" || !overflow.Records || overflow.Count != 1100 || overflow.Window != "null" {
			t.Fatalf("window overflow: unexpected census %+v", overflow.Window)
		}

		// a complete window inventory with every disposition
		small := requireStWholeWindowTestEqual(t, ctx, "complete window", 17, start.Add(600*time.Second), start.Add(605*time.Second))
		if small.Err != "" || !small.Records || small.Count == 0 || small.Window == "null" {
			t.Fatalf("complete window: unexpected census %+v", small)
		}
		var window payoutartifact.ClosedWorkWindow
		server.Raise(json.Unmarshal([]byte(small.Window), &window))
		dispositions := map[string]int{}
		for _, record := range window.Records {
			dispositions[record.Disposition]++
		}
		if uint64(dispositions["credited"]) != small.Count || dispositions["open"] != 3 || dispositions["unassigned_canceled"] != 3 || dispositions["canceled"] != 3 {
			t.Fatalf("complete window lost a disposition: %v", dispositions)
		}

		// no credited row: the sentinel alone carries the complete window
		empty := requireStWholeWindowTestEqual(t, ctx, "empty window", 17, start.Add(-3*time.Hour), start.Add(-2*time.Hour))
		if empty.Err != "" || empty.Count != 0 || empty.Usages != "[]" {
			t.Fatalf("empty window: unexpected census %+v", empty)
		}
	})
}

// Each refusal that the read itself raises is identical in a whole window:
// a live and archived copy of one identity, a nonconserving original, one
// provider on two networks, and a provider total past int64.
func TestStEpochUsageWholeWindowRefusalsMatchOriginal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		providers := newStWholeWindowTestProviders()
		count := payoutartifact.MaxClosedWorkRecords + 100
		window := func(i int) time.Time { return base.Add(time.Duration(i) * 2 * time.Hour) }
		for i, prefix := range []string{"refusal-duplicate", "refusal-conservation", "refusal-network", "refusal-overflow"} {
			providers.seed(t, ctx, prefix, window(i), count)
		}

		// a reused identity: archived, then live again
		archiveStWholeWindowTestRows(t, ctx, "refusal-duplicate", 777)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,usage_origin_is_source,outcome,close_time,provider_usage)
				SELECT contract_id,$1,$1,$2,$2,1000000,true,outcome,close_time,provider_usage FROM st_provider_usage_archive WHERE contract_id=md5('refusal-duplicate-777')::uuid`,
				server.NewId(), server.NewId()))
		})
		// the decoder refuses an original whose providers do not sum to its total
		addStWholeWindowTestRow(t, ctx, window(1).Add(30*time.Minute), map[string]any{"version": 1, "byte_count": 10,
			"providers": []any{map[string]any{"client_id": providers.clients[2].String(), "network_id": providers.networks[2].String(), "byte_count": 9}}})
		// one provider on two networks
		addStWholeWindowTestRow(t, ctx, window(2).Add(30*time.Minute), &contractUsageSnapshot{Version: 1, ByteCount: 3,
			Providers: []contractProviderUsage{{ClientId: providers.clients[3], NetworkId: server.NewId(), ByteCount: 3}}})
		// two valid originals whose provider total passes int64
		huge := ByteCount(math.MaxInt64/2 + 1)
		overflowClient, overflowNetwork := server.NewId(), server.NewId()
		for range 2 {
			addStWholeWindowTestRow(t, ctx, window(3).Add(30*time.Minute), &contractUsageSnapshot{Version: 1, ByteCount: huge,
				Providers: []contractProviderUsage{{ClientId: overflowClient, NetworkId: overflowNetwork, ByteCount: huge}}})
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract; ANALYZE st_provider_usage_archive`))
		})

		for i, want := range []string{"has both live and archived usage", "nonconserving contract provider usage", "ambiguous network or overflowing total", "ambiguous network or overflowing total"} {
			read := requireStWholeWindowTestEqual(t, ctx, want, 17, window(i), window(i).Add(time.Hour))
			if !bytes.Contains([]byte(read.Err), []byte(want)) {
				t.Fatalf("window %d: refusal %q, want %q", i, read.Err, want)
			}
		}
	})
}

// Terminal usage without a close time refuses every window, live or archived,
// even beside a whole window of dated work; the probe must see it first.
func TestStEpochUsageWholeWindowMissingCloseTimeMatchesOriginal(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprintf("archived=%t", archived), func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.ApplyDbMigrations = false
			env.Run(t, func(t testing.TB) {
				ctx := t.Context()
				id, start := seedMissingUsageTimeBeforeCutover(t, ctx, ContractOutcomeSettled, true)
				if archived {
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
					})
				}
				providers := newStWholeWindowTestProviders()
				providers.seed(t, ctx, "missing-time", start, payoutartifact.MaxClosedWorkRecords+100)
				server.Db(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract; ANALYZE st_provider_usage_archive`))
				})
				read := requireStWholeWindowTestEqual(t, ctx, "missing close time", 17, start, start.Add(time.Hour))
				if !bytes.Contains([]byte(read.Err), []byte(id.String())) || !bytes.Contains([]byte(read.Err), []byte("without a close time")) {
					t.Fatalf("missing close time refusal changed: %s", read.Err)
				}
			})
		})
	}
}

// An explicit historical debt is admitted only by its own epoch, inside a
// whole window just as in a census-sized one.
func TestStEpochUsageWholeWindowLegacyExclusionMatchesOriginal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.ApplyDbMigrations = false
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		debtAt := start.Add(17 * time.Minute)
		id := addStContractUsageSnapshotTestRow(t, ctx, debtAt, nil)
		server.ApplyDbMigrations(ctx)
		data, err := json.Marshal(contractUsageLegacyTestWire(id, debtAt, 987))
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET provider_usage=$2 WHERE contract_id=$1 AND provider_usage IS NULL`, id, data))
		})
		providers := newStWholeWindowTestProviders()
		count := payoutartifact.MaxClosedWorkRecords + 100
		providers.seed(t, ctx, "legacy", start, count)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract; ANALYZE st_provider_usage_archive`))
		})
		own := requireStWholeWindowTestEqual(t, ctx, "legacy own epoch", 17, start, start.Add(time.Hour))
		if own.Err != "" || own.Count != uint64(count+1) || own.Records {
			t.Fatalf("legacy debt in its own epoch: %+v", own.Err)
		}
		requireStWholeWindowTestOracle(t, ctx, "legacy own epoch", start, start.Add(time.Hour), own)
		other := requireStWholeWindowTestEqual(t, ctx, "legacy other epoch", 18, start, start.Add(time.Hour))
		if !bytes.Contains([]byte(other.Err), []byte("legacy usage exclusion differs")) {
			t.Fatalf("legacy debt borrowed another epoch: %s", other.Err)
		}
	})
}
