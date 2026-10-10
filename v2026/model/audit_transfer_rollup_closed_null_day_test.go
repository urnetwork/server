package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Growing unrelated open contracts must not grow the selected day's contract
// work. Replay the actual migrated index into the temporary plan fixture; the
// original fixture retains the pre-migration access paths for the causal arm.
func TestRollupTransferAuditClosedNullDayWork(t *testing.T) {
	if testing.Short() {
		t.Skip("daily rollup closed-NULL query-work population")
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
					t.Errorf("daily work fixture cleanup: %v", err)
				}
			}()
			var definition string
			server.Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid) FROM pg_index
				WHERE indexrelid=to_regclass('public.transfer_contract_audit_closed_null_day')
				AND indrelid='public.transfer_contract'::regclass AND indisvalid AND indisready AND indislive`).Scan(&definition))
			if strings.Count(definition, " ON public.transfer_contract ") != 1 {
				t.Fatal("migrated audit index does not belong to the expected contract table")
			}
			fixtureIndex := strings.Replace(definition, " ON public.transfer_contract ", " ON pg_temp.transfer_contract ", 1)
			server.RaisePgResult(conn.Exec(ctx, transferAuditWorkFixtureSQL))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract
				SELECT lpad(to_hex(4000000+g),32,'0')::uuid,lpad('1',32,'0')::uuid,lpad('1',32,'0')::uuid,
				 lpad('1',32,'0')::uuid,lpad('2',32,'0')::uuid,lpad('3',32,'0')::uuid,
				 7,timestamp '2026-01-01',NULL,NULL,repeat('synthetic-width-',8)
				FROM generate_series(1,524288)g; ANALYZE transfer_contract`))
			var contracts, reports, selected int64
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM transfer_contract),
				(SELECT count(*) FROM contract_close),(SELECT count(*) FROM transfer_contract
				WHERE timestamp '2026-10-06'<=close_time AND close_time<timestamp '2026-10-07')`).Scan(&contracts, &reports, &selected))
			if contracts != 1184296 || reports != 1520014 || selected != 40105 {
				t.Fatalf("unexpected work population: contracts=%d reports=%d selected=%d", contracts, reports, selected)
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `DROP INDEX IF EXISTS pg_temp.transfer_contract_audit_closed_null_day`))
				before := explainTransferAuditWork(t, ctx, conn, transferAuditDailyBytesSQL, mode, "2026-10-06", "2026-10-07", 280716)
				server.RaisePgResult(conn.Exec(ctx, fixtureIndex))
				after := explainTransferAuditWork(t, ctx, conn, transferAuditDailyBytesSQL, mode, "2026-10-06", "2026-10-07", 280716)
				if mode == "force_custom_plan" && (before.contractRepeatedLoops || before.contracts < float64(selected)*4) {
					t.Fatalf("open-history growth did not discriminate the old access path: %+v", before)
				}
				if after.contractRepeatedLoops || after.contractUpper > float64(selected)+16 ||
					after.bitmapEntries > 2*float64(selected)+32 || !after.indexes["transfer_contract_audit_closed_null_day"] {
					t.Fatalf("%s closed-NULL day lost its bounded access path: %+v", mode, after)
				}
				// A sequential scan has no bitmap work to compare. The absolute
				// selected-day cap above still bounds a newly chosen bitmap.
				if after.contractUpper > before.contractUpper*1.05+16 || after.reportUpper > before.reportUpper*1.05+16 ||
					(before.bitmapEntries > 0 && after.bitmapEntries > before.bitmapEntries*1.05+16) ||
					after.buffers > before.buffers*1.05+128 || after.tempBlocks > before.tempBlocks*1.05+128 {
					t.Fatalf("%s audit index amplified query work: before=%+v after=%+v", mode, before, after)
				}
				t.Logf("mode=%s contract_work=%.0f->%.0f report_work=%.0f->%.0f buffers=%.0f->%.0f",
					mode, before.contractUpper, after.contractUpper, before.reportUpper, after.reportUpper, before.buffers, after.buffers)
			}
		})
	})
}
