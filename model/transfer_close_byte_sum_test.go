package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Assert work, not elapsed time. Temporary tables model the close-time partial
// indexes at a scale where a window is far larger than a chunk: 660,008
// contracts, 1,520,014 closes and 524,288 unrelated open contracts. Every
// chunk of the narrow (about 40,000 contracts, NULL outcomes included) and
// dense (600,000 contracts) days must read at most one chunk of contracts and
// destination closes in both custom and generic plans, and the chunks must add
// up to the single-statement totals.
func TestTransferCloseByteChunkWorkIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("transfer close chunk query-work population")
	}
	if transferCloseStatementContractLimit < transferCloseByteChunkSize {
		t.Fatalf("chunk size %d exceeds the statement limit %d", transferCloseByteChunkSize, transferCloseStatementContractLimit)
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			defer func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if _, err := conn.Exec(cleanup, `DROP TABLE IF EXISTS pg_temp.contract_close,pg_temp.transfer_contract;
					RESET search_path; RESET statement_timeout`); err != nil {
					t.Errorf("chunk work fixture cleanup: %v", err)
				}
			}()
			// replay the migrated NULL-outcome close-day index into the fixture
			var definition string
			server.Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid) FROM pg_index
				WHERE indexrelid=to_regclass('public.transfer_contract_audit_closed_null_day')
				AND indrelid='public.transfer_contract'::regclass AND indisvalid AND indisready AND indislive`).Scan(&definition))
			if strings.Count(definition, " ON public.transfer_contract ") != 1 {
				t.Fatal("migrated audit index does not belong to the expected contract table")
			}
			server.RaisePgResult(conn.Exec(ctx, transferAuditWorkFixtureSQL))
			server.RaisePgResult(conn.Exec(ctx, strings.Replace(definition, " ON public.transfer_contract ", " ON pg_temp.transfer_contract ", 1)))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract
				SELECT lpad(to_hex(4000000+g),32,'0')::uuid,lpad('1',32,'0')::uuid,lpad('1',32,'0')::uuid,
				 lpad('1',32,'0')::uuid,lpad('2',32,'0')::uuid,lpad('3',32,'0')::uuid,
				 7,timestamp '2026-01-01',NULL,NULL,repeat('synthetic-width-',8)
				FROM generate_series(1,524288)g; ANALYZE transfer_contract`))

			for _, window := range []struct {
				start string
				end   string
				want  int64
			}{
				{start: "2026-10-06", end: "2026-10-07", want: 280716},
				{start: "2026-09-01", end: "2026-09-02", want: 4059300},
			} {
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					func() {
						tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
						server.Raise(err)
						defer func() {
							cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
							defer cancel()
							_ = tx.Rollback(cleanup)
							// by name: pgx keeps its own cached statements on this connection
							_, _ = conn.Exec(cleanup, `DEALLOCATE transfer_close_chunk_false`)
							_, _ = conn.Exec(cleanup, `DEALLOCATE transfer_close_chunk_true`)
						}()
						server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='30s'; SET LOCAL lock_timeout='2s';
							SET LOCAL max_parallel_workers_per_gather=0; SET LOCAL work_mem='16MB'; SET LOCAL jit=off`))
						server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode=`+mode))
						for _, resume := range []bool{false, true} {
							server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
								`PREPARE transfer_close_chunk_%t(timestamp,uuid,timestamp,varchar) AS %s`,
								resume,
								transferCloseByteChunkSql(transferAuditRollupDayMarker, false, resume),
							)))
						}

						afterCloseTime := window.start + " 00:00:00"
						afterContractId := server.Id{}.String()
						resume := false
						var byteCount int64
						var contractCount int64
						var maxContractReads float64
						for chunk := 0; ; chunk += 1 {
							call := fmt.Sprintf(
								`EXECUTE transfer_close_chunk_%t(timestamp '%s', uuid '%s', timestamp '%s', 'destination')`,
								resume, afterCloseTime, afterContractId, window.end+" 00:00:00",
							)
							if chunk%25 == 0 {
								var raw []byte
								server.Raise(tx.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+call, pgx.QueryExecModeExec).Scan(&raw))
								var plans []struct{ Plan transferCloseStatementNode }
								server.Raise(json.Unmarshal(raw, &plans))
								var contractReads float64
								var reportReads float64
								var inspect func(transferCloseStatementNode)
								inspect = func(node transferCloseStatementNode) {
									reads := node.Loops * (node.Rows + node.Removed + node.Recheck)
									switch node.Relation {
									case "transfer_contract":
										contractReads += reads
									case "contract_close":
										reportReads += reads
									}
									for _, child := range node.Plans {
										inspect(child)
									}
								}
								inspect(plans[0].Plan)
								maxContractReads = max(maxContractReads, contractReads)
								// a merge of two index branches may read one look-ahead key per branch
								if transferCloseByteChunkSize+2 < contractReads || transferCloseByteChunkSize < reportReads {
									t.Fatalf("%s %s chunk %d read %.0f contracts and %.0f closes, more than one chunk of %d",
										window.start, mode, chunk, contractReads, reportReads, transferCloseByteChunkSize)
								}
							}

							var lastCloseTime time.Time
							var lastContractId server.Id
							var count int64
							var bytes int64
							err := tx.QueryRow(ctx, call, pgx.QueryExecModeExec).Scan(&lastCloseTime, &lastContractId, &count, &bytes)
							if errors.Is(err, pgx.ErrNoRows) {
								break
							}
							server.Raise(err)
							byteCount += bytes
							contractCount += count
							if count < transferCloseByteChunkSize {
								break
							}
							afterCloseTime = lastCloseTime.Format("2006-01-02 15:04:05.999999")
							afterContractId = lastContractId.String()
							resume = true
						}
						if byteCount != window.want {
							t.Fatalf("%s %s chunked total=%d, want %d", window.start, mode, byteCount, window.want)
						}
						// the measurement saw a full chunk, so the bound is not vacuous
						if maxContractReads < transferCloseByteChunkSize {
							t.Fatalf("%s %s measured at most %.0f contract reads in a chunk", window.start, mode, maxContractReads)
						}
						t.Logf("window=%s mode=%s contracts=%d bytes=%d max_chunk_contract_reads=%.0f", window.start, mode, contractCount, byteCount, maxContractReads)
					}()
				}
			}
		})
	})
}

// Seeds one window where every contract shares a single close time, so every
// chunk boundary splits a tie, plus a contract at the minimum id exactly at
// the window start and contracts just outside each edge. Settled rows carry
// an empty synthetic usage snapshot, which the settlement guard requires.
func seedTransferCloseTieWindow(t testing.TB, ctx context.Context, start time.Time, end time.Time, contractCount int) {
	t.Helper()
	tieTime := start.Add(end.Sub(start) / 2)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time, outcome, provider_usage
			)
			SELECT
				lpad(to_hex(2000000 + g), 32, '0')::uuid,
				lpad('c1', 32, '0')::uuid, lpad('c2', 32, '0')::uuid,
				lpad('c3', 32, '0')::uuid, lpad('c4', 32, '0')::uuid,
				1000, $1::timestamp - interval '1 hour', $1,
				CASE WHEN g % 5 = 0 THEN NULL ELSE 'settled' END,
				'{"version":1,"byte_count":0,"providers":[]}'::jsonb
			FROM generate_series(1, $2::int) g
			UNION ALL
			SELECT edge.contract_id, lpad('c1', 32, '0')::uuid, lpad('c2', 32, '0')::uuid,
				lpad('c3', 32, '0')::uuid, lpad('c4', 32, '0')::uuid,
				1000, edge.close_time - interval '1 hour', edge.close_time, 'settled',
				'{"version":1,"byte_count":0,"providers":[]}'::jsonb
			FROM (VALUES
				('00000000-0000-0000-0000-000000000000'::uuid, $3::timestamp),
				('00000000-0000-0000-0000-0000000000f1'::uuid, $3::timestamp - interval '1 microsecond'),
				('00000000-0000-0000-0000-0000000000f2'::uuid, $4::timestamp)
			) edge(contract_id, close_time)
			`,
			tieTime,
			contractCount,
			start,
			end,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
			SELECT contract_id, close_time, 'destination',
				('x' || right(contract_id::text, 4))::bit(16)::int % 997 + 1
			FROM transfer_contract
			WHERE source_network_id = lpad('c1', 32, '0')::uuid
			UNION ALL
			SELECT contract_id, close_time, 'source', 999
			FROM transfer_contract
			WHERE source_network_id = lpad('c1', 32, '0')::uuid
			`,
		))
	})
}

// A checkpoint that is serialized between every chunk, as task args are, must
// resume to exactly the single-statement sum: every tie split by a chunk
// boundary counted once, the minimum id at the start included and the edges
// excluded, for both the all-outcome and the terminal-only sum.
func TestTransferCloseByteSumResumesAcrossCheckpoints(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		end := startOfUtcDay(server.NowUtc())
		start := end.Add(-24 * time.Hour)
		contractCount := 2*transferCloseByteChunkSize + 333
		seedTransferCloseTieWindow(t, ctx, start, end, contractCount)

		for _, terminalOnly := range []bool{false, true} {
			want := transferCloseStatementExpectedSum(ctx, start, end, terminalOnly)
			wantContractCount := int64(contractCount + 1)
			if terminalOnly {
				wantContractCount = int64(contractCount - contractCount/5 + 1)
			}
			checkpoint := TransferCloseByteSum{Start: start, End: end, TerminalOnly: terminalOnly}
			marker := transferAuditRollupDayMarker
			if terminalOnly {
				marker = clockUnrolledTailMarker
			}
			chunkCount := 0
			for !checkpoint.Complete {
				checkpointJson, err := json.Marshal(checkpoint)
				server.Raise(err)
				var resumed TransferCloseByteSum
				server.Raise(json.Unmarshal(checkpointJson, &resumed))
				resumed.advance(ctx, marker, 1)
				checkpoint = resumed
				chunkCount += 1
				if 10 < chunkCount {
					t.Fatal("resumed walk did not complete")
				}
			}
			if checkpoint.ByteCount != want || checkpoint.ContractCount != wantContractCount {
				t.Fatalf("terminal_only=%t resumed sum=%d over %d contracts, want %d over %d",
					terminalOnly, checkpoint.ByteCount, checkpoint.ContractCount, want, wantContractCount)
			}
			if wantChunkCount := int(wantContractCount)/transferCloseByteChunkSize + 1; chunkCount != wantChunkCount {
				t.Fatalf("terminal_only=%t walk took %d chunks, want %d", terminalOnly, chunkCount, wantChunkCount)
			}
			// a complete sum reads nothing more
			if !checkpoint.advance(ctx, marker, 1) || checkpoint.ByteCount != want {
				t.Fatal("complete sum changed on a further advance")
			}
		}
	})
}

// A resumed clock walk extended past its first end reads the contracts
// finalized after the backfill began, once, and does not reread the contracts
// already behind its cursor.
func TestClockBackfillExtendToReadsLaterContractsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		end := server.NowUtc().Add(-time.Minute)
		start := end.Add(-24 * time.Hour)
		// one in five is NULL-outcome, so more than one chunk is terminal
		contractCount := 2*transferCloseByteChunkSize + 77
		seedTransferCloseTieWindow(t, ctx, start, end, contractCount)

		backfill := NewClockBackfill(ctx)
		if backfill.Advance(ctx, 1) {
			t.Fatal("one chunk completed a walk of more than a chunk")
		}
		firstEnd := backfill.Tail.End
		// finalized after the backfill began, so outside its first window
		laterContractId := server.NewId()
		laterCloseTime := firstEnd.Add(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id, destination_network_id, destination_id,
					transfer_byte_count, create_time, close_time, outcome, provider_usage
				)
				VALUES ($1, $2, $2, $2, $2, 1000, $3::timestamp - interval '1 minute', $3, 'settled', '{"version":1,"byte_count":0,"providers":[]}'::jsonb)
				`,
				laterContractId,
				server.NewId(),
				laterCloseTime,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
				VALUES ($1, $2, 'destination', 1000003)
				`,
				laterContractId,
				laterCloseTime,
			))
		})

		backfill.ExtendTo(laterCloseTime.Add(time.Microsecond))
		if backfill.Tail.Complete || !backfill.Tail.End.After(laterCloseTime) {
			t.Fatal("extension did not reopen the walk past the later contract")
		}
		backfill.Advance(ctx, 10)
		if !backfill.Tail.Complete {
			t.Fatal("extended walk did not complete")
		}
		want := transferCloseStatementExpectedSum(ctx, ClockSinceTime, laterCloseTime.Add(time.Microsecond), true)
		if got := backfill.candidate(); got != want {
			t.Fatalf("extended candidate=%d, want %d", got, want)
		}
		// an earlier end never shrinks the walk
		backfill.ExtendTo(firstEnd)
		if !backfill.Tail.Complete || !backfill.Tail.End.After(laterCloseTime) {
			t.Fatal("an earlier end reopened or shortened the walk")
		}
	})
}
